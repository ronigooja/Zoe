#!/usr/bin/env bash
# Run on the Zoe host only. Never source deployment environment files.
set -euo pipefail
umask 077
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
helper="$script_dir/deploy-state.py"
sha="" run_id="" cli_run_id="" dry_run=false manual=false
usage() { echo 'usage: deploy.sh --sha FULL_SHA [--run-id ID] [--cli-run-id ID] [--dry-run] | --rollback --sha OLD_SHA' >&2; exit 2; }
while (($#)); do
  case "$1" in
    --sha) [[ $# -ge 2 ]] || usage; sha="$2"; shift 2 ;;
    --run-id) [[ $# -ge 2 ]] || usage; run_id="$2"; shift 2 ;;
    --cli-run-id) [[ $# -ge 2 ]] || usage; cli_run_id="$2"; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    --rollback) manual=true; shift ;;
    *) usage ;;
  esac
done
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || usage
[[ -z "$run_id" || "$run_id" =~ ^[0-9]+$ ]] || usage
[[ -z "$cli_run_id" || "$cli_run_id" =~ ^[0-9]+$ ]] || usage
[[ "$manual" == false || "$dry_run" == false ]] || usage
releases="${ZOE_RELEASES_DIR:-/opt/zoe/releases}"
env_file="${ZOE_ENV_FILE:-/etc/zoe/zoe-stack.env}"
db="${ZOE_DB_PATH:-/var/lib/zoe/server/data/zoe.sqlite}"
api="${ZOE_API_URL:-http://127.0.0.1:8787/healthz}"
units="${ZOE_UNITS_DIR:-/etc/systemd/system}"
bin_dir="${ZOE_BIN_DIR:-/usr/local/bin}"
log_root="${ZOE_DEPLOY_LOG_DIR:-/var/log/zoe-deploy}"
current_prefix="${ZOE_CURRENT_PREFIX:-/opt/zoe/releases/current}"
health_attempts="${ZOE_DEPLOY_HEALTH_ATTEMPTS:-30}"
if [[ ! "$health_attempts" =~ ^[0-9]+$ ]]; then usage; fi
if ((health_attempts < 1 || health_attempts > 30)); then usage; fi
[[ $EUID -eq 0 ]] || { echo 'deploy step=preflight status=fail reason=root-required'; exit 1; }
for tool in python3 docker systemctl journalctl gh curl sha256sum tar flock cp mv ln readlink cmp date sleep diff cut cat rm mkdir mktemp touch uname chmod install; do
  command -v "$tool" >/dev/null || { echo "deploy step=preflight status=fail missing=$tool"; exit 1; }
done
# A lock is operational metadata, not deployment mutation. Dry-run leaves none.
mkdir -p "$log_root"
exec 9>"$log_root/.lock"
flock -n 9 || { echo 'deploy step=preflight status=fail reason=deployment-locked'; exit 1; }
log="$log_root/$sha.log"
exec >>"$log" 2>&1
step=preflight armed=false loss=false scratch="" dl="" state="" restart_epoch="" own_dl=false
state_op() { python3 "$helper" "$@"; }
ok() { echo "deploy step=$step status=ok"; }
services_active() {
  local unit
  # Multi-unit is-active succeeds when ANY unit is active, not all.
  for unit in zoe-compose zoe-runner zoe-opencode; do
    systemctl is-active --quiet "$unit" >/dev/null 2>&1 || return 1
  done
}
health() { [[ "$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 3 "$1" 2>/dev/null)" == 200 ]]; }
wait_health() {
  local attempt
  for ((attempt=0; attempt<health_attempts; attempt++)); do
    if health "$api"; then return 0; fi
    sleep 2
  done
  return 1
}
selector() {
  ln -sfn -- "$1" "$releases/current.new" || return 1
  mv -Tf -- "$releases/current.new" "$releases/current"
}
cli_restore() {
  local target
  target="$(cat "$state/previous-cli")" || return 1
  [[ -x "$target" ]] || return 1
  ln -sfn -- "$target" "$bin_dir/.zoe-deploy-cli" || return 1
  mv -Tf -- "$bin_dir/.zoe-deploy-cli" "$bin_dir/zoe" || return 1
  "$bin_dir/zoe" --source-revision >/dev/null
}
rollback() {
  local old env_backup db_backup
  old="$(cat "$state/previous-release")" || return 1
  [[ -d "$old" && -f "$state/backup-complete" ]] || return 1
  systemctl stop zoe-compose >/dev/null 2>&1 || return 1
  env_backup="$env_file.pre-$(cat "$state/revision" | cut -c1-12)"
  db_backup="$db.pre-$(cat "$state/revision" | cut -c1-12)"
  if [[ -f "$state/env-backup" ]]; then env_backup="$(cat "$state/env-backup")" || return 1; fi
  if [[ -f "$state/db-backup" ]]; then db_backup="$(cat "$state/db-backup")" || return 1; fi
  state_op atomic-copy "$env_backup" "$env_file" || return 1
  selector "$old" || return 1
  # Try the old server against the migrated DB before considering data restore.
  if [[ -f "$state/restart-attempted" ]]; then
    local migration_status=0
    state_op migrated "$state/before.json" "$db" || migration_status=$?
    [[ "$migration_status" -le 1 ]] || return 1
    if [[ "$migration_status" -eq 0 ]]; then
      if ! systemctl start zoe-compose >/dev/null 2>&1 || ! wait_health; then
        systemctl stop zoe-compose >/dev/null 2>&1 || return 1
        # Stop every Zoe database writer before restoring via SQLite backup API.
        systemctl stop zoe-runner >/dev/null 2>&1 || return 1
        state_op restore-db "$db_backup" "$db" || return 1
        loss=true
        touch "$state/data-loss" || return 1
      fi
    fi
  fi
  cli_restore || return 1
  systemctl start zoe-compose >/dev/null 2>&1 || return 1
  wait_health || return 1
  if [[ -f "$state/runner-changed" || "$loss" == true ]]; then
    systemctl restart zoe-runner >/dev/null 2>&1 || return 1
  fi
  services_active || return 1
  touch "$state/rolled-back" || return 1
  return 0
}
finish() {
  local status=$? rollback_status=not-needed
  trap - EXIT
  if ((status != 0)); then
    echo "deploy step=$step status=fail"
    if [[ "$armed" == true ]]; then
      if rollback; then rollback_status=ok; else rollback_status=failed; fi
      echo "deploy step=rollback status=$([[ "$rollback_status" == ok ]] && echo ok || echo fail)"
    fi
    [[ "$dry_run" == true ]] || { rm -f "$log_root/$sha.done"; touch "$log_root/$sha.failed"; }
    echo "deploy summary=failed step=$step rollback=$rollback_status data_loss=$loss"
    if [[ "$loss" == true ]]; then echo 'DATA LOSS: data written after deploy started is lost'; fi
  fi
  # These selectors are transaction-owned scratch paths, never release data.
  if [[ "$dry_run" == false && -n "$state" && -d "$state" ]]; then
    [[ ! -L "$releases/current.new" ]] || rm -f "$releases/current.new"
    [[ ! -L "$bin_dir/.zoe-deploy-cli" ]] || rm -f "$bin_dir/.zoe-deploy-cli"
  fi
  [[ -z "$scratch" ]] || rm -rf -- "$scratch"
  [[ "$own_dl" == false || -z "$dl" ]] || rm -rf -- "$dl"
  exit "$status"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ "$manual" == true ]]; then
  # The last transaction targeting this previous SHA owns the rollback backups.
  state="$releases/.deploy-rollback-$sha"
  [[ -L "$state" ]] || { echo 'no recorded rollback transaction for requested SHA'; exit 1; }
  state_op idle "$db"
  step=rollback
  rollback
  ok
  rolled_revision="$(cat "$state/revision")"
  rm -f "$log_root/$rolled_revision.done"
  touch "$log_root/$rolled_revision.failed"
  echo "deploy summary=manual-rollback data_loss=$loss"
  exit 0
fi
state_op idle "$db"
health "$api"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/zoe-deploy.XXXXXX")"
state_op snapshot "$db" > "$scratch/before.json"
cat "$scratch/before.json"
previous="$(readlink -f "$releases/current")"
[[ -d "$previous" && "$(basename "$previous")" =~ ^[0-9a-f]{40}$ ]]
previous_cli="$(readlink -f "$bin_dir/zoe")"
[[ -L "$bin_dir/zoe" && -x "$previous_cli" ]]
echo "current release=$(basename "$previous" | cut -c1-12)"
state_op image-summary "$env_file"
state="$releases/.deploy-$sha"
if [[ -f "$state/complete" && "$previous" == "$releases/$sha" ]]; then
  echo 'deploy summary=already-complete; use a new SHA for another deployment'
  ok
  exit 0
fi
if [[ -d "$state" && ! -f "$state/rolled-back" ]]; then
  echo 'incomplete transaction; run --rollback --sha PREVIOUS_SHA before retrying'
  exit 1
fi
ok
step=download-verify
if [[ -z "$run_id" ]]; then
  gh run list -R ronigooja/Zoe --workflow server-release.yml --commit "$sha" --status success --limit 100 --json databaseId,headSha > "$scratch/runs.json" 2>/dev/null
  run_id="$(python3 - "$scratch/runs.json" "$sha" <<'PY'
import json, sys
matches = [run['databaseId'] for run in json.load(open(sys.argv[1])) if run['headSha'] == sys.argv[2]]
if not matches: raise SystemExit('no successful server release run for SHA')
print(matches[0])
PY
)"
fi
cli_run_id="${cli_run_id:-$run_id}"
for id in "$run_id" "$cli_run_id"; do
  gh run view "$id" -R ronigooja/Zoe --json headSha,conclusion,workflowName > "$scratch/run.json" 2>/dev/null
  python3 - "$scratch/run.json" "$sha" <<'PY'
import json, sys
run = json.load(open(sys.argv[1]))
assert run['headSha'] == sys.argv[2] and run['conclusion'] == 'success', 'run is not successful matching revision'
assert run['workflowName'] == 'Server deployment artifact', 'unexpected release workflow'
PY
done
if [[ "$dry_run" == true ]]; then dl="$scratch/download"; else dl="$releases/$sha.dl"; fi
[[ ! -e "$dl" ]] || { echo 'download directory already exists; inspect interrupted download before retry'; exit 1; }
mkdir -p "$dl/server-download" "$dl/cli-download" "$dl/server" "$dl/cli"
own_dl=true
gh run download "$run_id" -R ronigooja/Zoe -n "server-release-$sha" -D "$dl/server-download" >/dev/null 2>&1
gh run download "$cli_run_id" -R ronigooja/Zoe -n "operator-cli-$sha" -D "$dl/cli-download" >/dev/null 2>&1
state_op extract "$dl/server-download/server-linux-amd64.tar.gz" "$dl/server"
state_op extract "$dl/cli-download/zoe-linux-amd64.tar.gz" "$dl/cli"
for release in "$dl/server" "$dl/cli"; do
  (cd "$release" && sha256sum -c SHA256SUMS >/dev/null 2>&1)
  [[ "$(cat "$release/SOURCE_REVISION")" == "$sha" ]]
done
cmp -s "$dl/server/bin/zoe" "$dl/cli/bin/zoe"
[[ "$("$dl/cli/bin/zoe" --source-revision)" == "$sha" ]]
bash "$script_dir/deployment-candidate.sh" "$dl/server/release-manifest.json" --compose "$dl/server/deploy-linux/docker-compose.yml" --env-file "$env_file" --output "$scratch/plan.json" >/dev/null 2>&1
state_op validate "$dl/server" "$env_file" "$units" "$current_prefix"
python3 - "$dl/server/release-manifest.json" "$sha" <<'PY'
import json, sys
manifest = json.load(open(sys.argv[1]))
assert manifest['source_revision'] == sys.argv[2], 'manifest revision mismatch'
assert manifest['architecture'] == 'amd64', 'server release requires amd64 host'
PY
[[ "$(uname -m)" == x86_64 ]]
if [[ -e "$releases/$sha" ]]; then
  diff -qr "$dl/server" "$releases/$sha" >/dev/null 2>&1
fi
ok
if [[ "$dry_run" == true ]]; then
  echo 'deploy plan=backup,pull-changed-images,install-cli,switch,restart,verify; rollback=automatic'
  echo 'deploy summary=dry-run; release,env,database,CLI,services unchanged'
  exit 0
fi
step=backup
backup_suffix="${sha:0:12}"
if [[ -f "$state/rolled-back" ]]; then
  # A fresh attempt must not reuse the pre-rollback DB snapshot or overwrite it.
  attempt="$(date +%s)-$$"
  archived_state="$state.attempt-$attempt"
  mv -T -- "$state" "$archived_state"
  # Preserve the old manual-rollback reference until the new record is complete.
  ln -sfn "$archived_state" "$releases/.deploy-rollback-$(basename "$previous")"
  backup_suffix="${sha:0:12}.attempt-$attempt"
fi
mkdir "$state"
printf '%s\n' "$sha" > "$state/revision"
printf '%s\n' "$previous" > "$state/previous-release"
printf '%s\n' "$previous_cli" > "$state/previous-cli"
cp "$scratch/before.json" "$state/before.json"
printf '%s\n' "$db.pre-$backup_suffix" > "$state/db-backup"
printf '%s\n' "$env_file.pre-$backup_suffix" > "$state/env-backup"
state_op backup "$db" "$db.pre-$backup_suffix"
[[ ! -e "$env_file.pre-$backup_suffix" ]]
cp -p "$env_file" "$env_file.pre-$backup_suffix"
touch "$state/backup-complete"
ln -sfn "$state" "$releases/.deploy-rollback-$(basename "$previous")"
if ! cmp -s "$previous/bin/zoe-runner" "$dl/server/bin/zoe-runner"; then touch "$state/runner-changed"; fi
if [[ ! -e "$releases/$sha" ]]; then
  chmod 0755 "$dl/server"
  mv -T "$dl/server" "$releases/$sha"
fi
ok
# From this point failures also restore image environment / CLI before switching.
armed=true
step=images
state_op images "$releases/$sha" "$env_file" > "$scratch/images"
while IFS='=' read -r key ref; do
  # Pull by immutable digest; suppress registry output (credentials / full digests).
  docker pull "$ref" >/dev/null 2>&1
  digest="${ref##*@sha256:}"
  echo "image pulled name=$key digest=${digest:0:12}"
done < "$scratch/images"
state_op edit-images "$releases/$sha" "$env_file"
ok
step=install-cli
ZOE_RELEASES_DIR="$releases" ZOE_BIN_DIR="$bin_dir" bash "$dl/cli/deploy-linux/install-cli.sh" "$dl/cli-download/zoe-linux-amd64.tar.gz" >/dev/null 2>&1
[[ "$("$bin_dir/zoe" --source-revision)" == "$sha" ]]
ok
step=switch
qdrant_storage="$(state_op qdrant-storage "$releases/$sha" "$env_file")"
# Do not let Compose auto-create a root-owned directory on existing hosts.
# Existing storage (and every other data directory) is left untouched.
if [[ -n "$qdrant_storage" && ! -d "$qdrant_storage" ]]; then
  install -d -m 0700 -o 1000 -g 1000 -- "$qdrant_storage"
fi
state_op idle "$db"
selector "$sha"
# Last check immediately before service restart; no remote calls between them.
state_op idle "$db"
# systemd v219 accepts @epoch; Docker accepts bare seconds, not @epoch.
restart_epoch="$(date +%s)"
[[ "$restart_epoch" =~ ^[0-9]+$ ]]
runner_restart_epoch="$restart_epoch"
touch "$state/restart-attempted"
systemctl restart zoe-compose >/dev/null 2>&1
wait_health
if [[ -f "$state/runner-changed" ]]; then
  systemctl restart zoe-runner >/dev/null 2>&1
  # The old process has exited; @epoch includes new startup logs in this second.
  runner_restart_epoch="$(date +%s)"
  [[ "$runner_restart_epoch" =~ ^[0-9]+$ ]]
fi
ok
step=verify
services_active
health "$api"
if [[ -n "${ZOE_PUBLIC_HEALTH_URL:-}" ]]; then health "$ZOE_PUBLIC_HEALTH_URL"; fi
container="$(docker compose --env-file "$env_file" -f "$releases/current/deploy-linux/docker-compose.yml" ps -q zoe-server 2>/dev/null)"
[[ -n "$container" && "$container" != *$'\n'* ]]
state_op container-verify "$releases/$sha" "$env_file" "$container" "$script_dir/expected-env.txt"
state_op db-verify "$state/before.json" "$db"
journalctl -u zoe-runner --since "@$runner_restart_epoch" --no-pager > "$scratch/runner.log" 2>/dev/null
state_op logs "$scratch/runner.log" runner
docker logs --since "$restart_epoch" "$container" > "$scratch/server.log" 2>&1
state_op logs "$scratch/server.log" server
[[ "$("$bin_dir/zoe" --source-revision)" == "$sha" ]]
ok
touch "$state/complete"
rm -f "$log_root/$sha.failed"
touch "$log_root/$sha.done"
armed=false
step=cleanup
rm -rf -- "$dl"
dl=""
ok
echo 'deploy summary=success backups=retained previous-release=retained previous-cli=retained'
