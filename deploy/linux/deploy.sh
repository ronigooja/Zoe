#!/usr/bin/env bash
# Run on the Zoe host only. Never source deployment environment files.
set -euo pipefail
umask 077
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
helper="$script_dir/deploy-state.py"
sha="" run_id="" cli_run_id="" artifact_dir="" from="" dry_run=false prompts_mode=keep
usage() { echo 'usage: deploy.sh --sha FULL_SHA [--run-id ID] [--cli-run-id ID] [--dry-run] [--prompts keep|overwrite] | --sha FULL_SHA (--artifact-dir DIR | --from HOST_ALIAS:DIRNAME) [--dry-run] [--prompts keep|overwrite]' >&2; exit 2; }
while (($#)); do
  case "$1" in
    --sha) [[ $# -ge 2 ]] || usage; sha="$2"; shift 2 ;;
    --run-id) [[ $# -ge 2 ]] || usage; run_id="$2"; shift 2 ;;
    --cli-run-id) [[ $# -ge 2 ]] || usage; cli_run_id="$2"; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    --prompts) [[ $# -ge 2 ]] || usage; prompts_mode="$2"; shift 2 ;;
    --artifact-dir) [[ $# -ge 2 ]] || usage; artifact_dir="$2"; shift 2 ;;
    --from) [[ $# -ge 2 && -n "$2" ]] || usage; from="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || usage
[[ "$prompts_mode" == keep || "$prompts_mode" == overwrite ]] || usage
[[ -z "$run_id" || "$run_id" =~ ^[0-9]+$ ]] || usage
[[ -z "$cli_run_id" || "$cli_run_id" =~ ^[0-9]+$ ]] || usage
if [[ -n "$artifact_dir" ]]; then
  [[ -z "$run_id$cli_run_id$from" ]] || usage
fi
if [[ -n "$from" ]]; then
  [[ -z "$run_id$cli_run_id$artifact_dir" ]] || usage
  [[ "$from" =~ ^(builder-nb03hk|builder-vm02hk):([A-Za-z0-9._-]+)$ ]] || usage
  [[ "${from#*:}" != *..* && "${from#*:}" != . ]] || usage
fi
releases="${ZOE_RELEASES_DIR:-/opt/zoe/releases}"
env_file="${ZOE_ENV_FILE:-/etc/zoe/zoe-stack.env}"
prompt_dir="${ZOE_PROMPT_DIR:-/etc/zoe/prompts}"
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
  if [[ "$tool" == gh && -n "$artifact_dir$from" ]]; then continue; fi
  command -v "$tool" >/dev/null || { echo "deploy step=preflight status=fail missing=$tool"; exit 1; }
done
if [[ -n "$from" ]]; then
  command -v scp >/dev/null || { echo 'deploy step=preflight status=fail missing=scp'; exit 1; }
fi
# A lock is operational metadata, not deployment mutation. Dry-run leaves none.
mkdir -p "$log_root"
exec 9>"$log_root/.lock"
flock -n 9 || { echo 'deploy step=preflight status=fail reason=deployment-locked'; exit 1; }
log="$log_root/$sha.log"
exec >>"$log" 2>&1
step=preflight scratch="" dl="" state="" restart_epoch="" own_dl=false
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
finish() {
  local status=$?
  trap - EXIT
  if ((status != 0)); then
    if [[ "$step" == prompts ]]; then
      echo 'deploy step=prompts status=failed'
    else
      echo "deploy step=$step status=fail"
    fi
    [[ "$dry_run" == true ]] || { rm -f "$log_root/$sha.done"; touch "$log_root/$sha.failed"; }
    echo "deploy summary=failed step=$step; inspect host before further mutation"
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
if [[ -d "$state" ]]; then
  echo 'incomplete transaction; operator inspection required before retrying'
  exit 1
fi
ok
step=download-verify
if [[ -n "$from" ]]; then
  # Root-created private scratch is exit-owned. Never execute a remote shell;
  # scp uses the host's preconfigured SSH alias and verified host key.
  mkdir -m 0700 "$scratch/pull"
  scp -r -oBatchMode=yes -oStrictHostKeyChecking=yes -- \
    "${from%%:*}:/root/Agents/zoe-release-build/${from#*:}" "$scratch/pull/" >/dev/null 2>&1
  artifact_dir="$scratch/pull/${from#*:}"
fi
if [[ -z "$artifact_dir" ]]; then
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
fi
if [[ "$dry_run" == true ]]; then dl="$scratch/download"; else dl="$releases/$sha.dl"; fi
[[ ! -e "$dl" ]] || { echo 'download directory already exists; inspect interrupted download before retry'; exit 1; }
mkdir -p "$dl/server-download" "$dl/cli-download" "$dl/server" "$dl/cli"
own_dl=true
if [[ -n "$artifact_dir" ]]; then
  state_op staged-copy "$artifact_dir" "$sha" "$dl/staged"
  cp "$dl/staged/server-linux-amd64.tar.gz" "$dl/server-download/"
  cp "$dl/staged/zoe-linux-amd64.tar.gz" "$dl/cli-download/"
else
  gh run download "$run_id" -R ronigooja/Zoe -n "server-release-$sha" -D "$dl/server-download" >/dev/null 2>&1
  gh run download "$cli_run_id" -R ronigooja/Zoe -n "operator-cli-$sha" -D "$dl/cli-download" >/dev/null 2>&1
fi
state_op extract "$dl/server-download/server-linux-amd64.tar.gz" "$dl/server"
state_op extract "$dl/cli-download/zoe-linux-amd64.tar.gz" "$dl/cli"
for release in "$dl/server" "$dl/cli"; do
  (cd "$release" && sha256sum -c SHA256SUMS >/dev/null 2>&1)
  [[ "$(cat "$release/SOURCE_REVISION")" == "$sha" ]]
done
cmp -s "$dl/server/bin/zoe" "$dl/cli/bin/zoe"
[[ "$("$dl/cli/bin/zoe" --source-revision)" == "$sha" ]]
# Resolve a missing legacy-host setting without editing or sourcing the env file.
ZOE_MEMORY_API_TOKEN_FILE="$(state_op memory-token-path "$env_file")"
export ZOE_MEMORY_API_TOKEN_FILE
bash "$script_dir/deployment-candidate.sh" "$dl/server/release-manifest.json" --compose "$dl/server/deploy-linux/docker-compose.yml" --env-file "$env_file" --output "$scratch/plan.json" >/dev/null 2>&1
state_op validate "$dl/server" "$env_file" "$units" "$current_prefix" "$prompt_dir"
python3 - "$dl/server/release-manifest.json" "$sha" <<'PY'
import json, sys
manifest = json.load(open(sys.argv[1]))
assert manifest['source_revision'] == sys.argv[2], 'manifest revision mismatch'
assert manifest['architecture'] == 'amd64', 'server release requires amd64 host'
PY
[[ "$(uname -m)" == x86_64 ]]
if [[ -n "$artifact_dir" ]]; then state_op staged-images "$dl/staged" "$dl/server" verify; fi
if [[ -e "$releases/$sha" ]]; then
  diff -qr "$dl/server" "$releases/$sha" >/dev/null 2>&1
fi
ok
step=prompts
prompts() {
  local prompt_release="$dl/server"
  [[ -d "$prompt_release" ]] || prompt_release="$releases/$sha"
  python3 - "$prompt_release" "$prompt_dir" "$prompts_mode" "$sha" "$1" <<'PY'
import hashlib, json, os, pathlib, re, shutil, sys, tempfile
release, host, mode, sha, action = sys.argv[1:]
release, host = pathlib.Path(release), pathlib.Path(host)
required = {'ui_controller.txt', 'register.txt', 'compact.txt', 'steward.txt', 'task_ui_display.txt', 'voice_instruction.txt'}
try:
    source = release/'prompts'
    files = sorted(source.iterdir())
    if not required <= {p.name for p in files} or not files:
        raise ValueError('missing prompts')
    manifest = {p['path']: p['sha256'] for p in json.loads((release/'release-manifest.json').read_text())['artifacts']}
    sums = (release/'SHA256SUMS').read_text().splitlines()
    plans = []
    for p in files:
        if not re.fullmatch(r'[A-Za-z0-9_]+\.txt', p.name) or p.is_symlink() or not p.is_file():
            raise ValueError('invalid prompt')
        data = p.read_bytes()
        if not data or not (p.stat().st_mode & 0o444): raise ValueError('empty or unreadable prompt')
        digest = hashlib.sha256(data).hexdigest()
        if manifest.get('prompts/'+p.name) != digest or digest+'  prompts/'+p.name not in sums:
            raise ValueError('unverified prompt')
        dest = host/p.name
        if dest.is_symlink() or (dest.exists() and not dest.is_file()): raise ValueError('invalid host prompt')
        if not dest.exists(): decision = 'install'
        elif dest.read_bytes() == data: decision = 'identical'
        elif mode == 'keep': decision = 'keep(differs)'
        else: decision = 'overwrite'
        if decision == 'overwrite' and os.path.lexists(host/(p.name+'.pre-'+sha[:12])):
            raise ValueError('backup exists')
        plans.append((p, dest, data, decision))
    parent = host
    while not parent.exists(): parent = parent.parent
    if not parent.is_dir() or not (parent.stat().st_mode & 0o222) or not os.access(parent, os.W_OK | os.X_OK):
        raise ValueError('prompt directory not writable')
    if action == 'plan':
        for p, dest, data, decision in plans:
            print(f'prompt name={p.name} bytes={len(data)} action={decision}')
    else:
        missing = not host.exists()
        host.mkdir(mode=0o755, parents=True, exist_ok=True)
        if missing: host.chmod(0o755)
        counts = {'install': 0, 'overwrite': 0, 'kept': 0}
        for p, dest, data, decision in plans:
            if decision not in ('install', 'overwrite'):
                counts['kept'] += 1
                continue
            if decision == 'overwrite':
                backup = host/(p.name+'.pre-'+sha[:12])
                with backup.open('xb') as out, dest.open('rb') as old:
                    shutil.copyfileobj(old, out)
                shutil.copystat(dest, backup)
            fd, temporary = tempfile.mkstemp(prefix='.zoe-prompt-', dir=host)
            try:
                with os.fdopen(fd, 'wb') as out:
                    out.write(data); out.flush(); os.fsync(out.fileno())
                os.chmod(temporary, 0o644); os.chown(temporary, 0, 0)
                os.replace(temporary, dest)
            finally:
                if os.path.exists(temporary): os.unlink(temporary)
            counts[decision] += 1
        print(f"deploy step=prompts status=ok installed={counts['install']} kept={counts['kept']} overwritten={counts['overwrite']}")
except (OSError, ValueError, KeyError, TypeError):
    sys.exit(1)
PY
}
prompts plan
step=download-verify
if [[ "$dry_run" == true ]]; then
  if [[ -n "$artifact_dir" ]]; then
    echo 'deploy plan=backup,prompts,memory-token,load-images,install-cli,switch,restart,verify'
  else
    echo 'deploy plan=backup,prompts,memory-token,pull-changed-images,install-cli,switch,restart,verify'
  fi
  echo 'deploy summary=dry-run; release,env,database,CLI,services unchanged; prompts unchanged'
  exit 0
fi
step=backup
backup_suffix="${sha:0:12}"
mkdir "$state"
cp "$scratch/before.json" "$state/before.json"
state_op backup "$db" "$db.pre-$backup_suffix"
[[ ! -e "$env_file.pre-$backup_suffix" ]]
cp -p "$env_file" "$env_file.pre-$backup_suffix"
if ! cmp -s "$previous/bin/zoe-runner" "$dl/server/bin/zoe-runner"; then touch "$state/runner-changed"; fi
if [[ ! -e "$releases/$sha" ]]; then
  chmod 0755 "$dl/server"
  mv -T "$dl/server" "$releases/$sha"
fi
ok
step=prompts
prompts install
step=memory-token
state_op memory-token "$env_file"
step=images
if [[ -n "$artifact_dir" ]]; then
  state_op staged-images "$dl/staged" "$releases/$sha" load
else
state_op images "$releases/$sha" "$env_file" > "$scratch/images"
while IFS='=' read -r key ref; do
  # Pull by immutable digest; suppress registry output (credentials / full digests).
  docker pull "$ref" >/dev/null 2>&1
  digest="${ref##*@sha256:}"
  echo "image pulled name=$key digest=${digest:0:12}"
done < "$scratch/images"
fi
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
step=cleanup
rm -rf -- "$dl"
dl=""
ok
echo 'deploy summary=success backups=retained'
