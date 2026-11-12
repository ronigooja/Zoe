#!/usr/bin/env bash
# Synthetic host only: no Docker daemon, systemd, network, or real host data.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
[[ $EUID -eq 0 ]] || { echo 'run test-deploy.sh as root (CI uses sudo)'; exit 1; }
task_root="$(mktemp -d)"
trap 'rm -rf -- "$task_root"' EXIT
old=1111111111111111111111111111111111111111
new=2222222222222222222222222222222222222222
export OLD_SHA="$old" NEW_SHA="$new" REPO_ROOT="$repo_root"
fixture() {
  export FIXTURE="$task_root/$1" CASE="$1"
  mkdir -p "$FIXTURE"
  python3 - <<'PY'
import hashlib, json, os, pathlib, shutil, sqlite3, tarfile
p = pathlib.Path(os.environ['FIXTURE']); repo = pathlib.Path(os.environ['REPO_ROOT'])
old, new = os.environ['OLD_SHA'], os.environ['NEW_SHA']
for name in ['releases', 'units', 'bin', 'shims', 'archives', 'data']:
    (p/name).mkdir()
env = {'ZOE_SERVER_IMAGE': 'ghcr.io/test/zoe-server@sha256:'+'1'*64,
       'ZOE_MEMORY_WORKER_IMAGE': 'ghcr.io/test/zoe-memory-worker@sha256:'+'3'*64,
       'ZOE_RUNNER_IMAGE': 'ghcr.io/test/zoe-runner@sha256:'+'4'*64,
       'ZOE_VOICE_UPSTREAM_ROUTE': 'avas', 'ZOE_STEWARD_TIMEOUT_MS': '30000',
       'ZOE_VOICE_CLASSIFICATION_TIMEOUT_MS': '35000', 'ZOE_REGISTER_TIMEOUT_MS': '30000',
       'ZOE_MEMORY_BATCH_INTERVAL_HOURS': '24',
       'ZOE_REGISTER_BASE_URL': 'https://route.invalid', 'ZOE_REGISTER_MODEL': 'route',
       'ZOE_REGISTER_API_KEY_FILE': '/run/secrets/register_api_key',
       'ZOE_COMPACT_BASE_URL': 'https://compact.invalid', 'ZOE_COMPACT_MODEL': 'compact',
       'ZOE_COMPACT_API_KEY_FILE': '/run/secrets/compact_api_key',
       'ZOE_COMPACT_TIMEOUT_MS': '600000', 'ZOE_COMPACT_MAX_CHARACTERS': '8000',
       'ZOE_COMPACT_INTERVAL_MINUTES': '60', 'ZOE_REGISTER_RAW_WINDOW_MINUTES': '60'}
(p/'stack.env').write_text(''.join(k+'='+v+'\n' for k,v in env.items()))
(p/'stack.env').chmod(0o640)
config = {'services': {'zoe-server': {'environment': {k:v for k,v in env.items() if not k.endswith('_IMAGE')},
                                    'volumes': [{'type': 'bind', 'source': str(p/'data')}]}}}
(p/'config.json').write_text(json.dumps(config))
for revision in [old, new]:
    root = p/'releases'/revision if revision == old else p/'source'
    (root/'bin').mkdir(parents=True); (root/'deploy-linux').mkdir()
    for name in ['zoe-server', 'zoe-runner', 'zoe-secret-entrypoint', 'zoe']:
        text = '#!/bin/sh\nexit 0\n# '+revision+'\n'
        if name == 'zoe': text = '#!/bin/sh\ntest "$1" = --source-revision && echo '+revision+'\n'
        (root/'bin'/name).write_text(text); (root/'bin'/name).chmod(0o755)
    (root/'SOURCE_REVISION').write_text(revision+'\n')
    for unit in ['zoe-compose', 'zoe-runner', 'zoe-opencode']:
        shutil.copy(repo/'deploy/linux'/f'{unit}.service', root/'deploy-linux')
    shutil.copy(repo/'deploy/linux/install-cli.sh', root/'deploy-linux')
    (root/'deploy-linux/docker-compose.yml').write_text('services:\n  zoe-server:\n    image: ${ZOE_SERVER_IMAGE}\n')
root = p/'source'
case = os.environ['CASE']
if case == 'missing-unit-path':
    (root/'bin/zoe-runner').unlink()
if case == 'undefined-variable':
    with (root/'deploy-linux/docker-compose.yml').open('a') as out: out.write('    command: ${UNDEFINED_TEST_VALUE:?set required value}\n')
def sums(root):
    text = ''.join(hashlib.sha256(f.read_bytes()).hexdigest()+'  '+f.relative_to(root).as_posix()+'\n'
                   for f in sorted(root.rglob('*')) if f.is_file() and f.name not in ['SHA256SUMS', 'release-manifest.json'])
    (root/'SHA256SUMS').write_text(text)
sums(root)
artifacts = [{'name': f.name, 'path': f.relative_to(root).as_posix(), 'sha256': hashlib.sha256(f.read_bytes()).hexdigest()}
             for f in sorted(root.rglob('*')) if f.is_file()]
images = {name: {'ref': 'ghcr.io/test/'+name+':test', 'digest': 'sha256:'+digit*64}
          for name,digit in [('zoe-server','2'), ('zoe-memory-worker','3'), ('zoe-runner','4')]}
(root/'release-manifest.json').write_text(json.dumps({'source_revision':new,'architecture':'amd64','artifacts':artifacts,'images':images}))
if case == 'checksum': (root/'bin/zoe-server').write_text('corrupt')
with tarfile.open(p/'archives/server-linux-amd64.tar.gz', 'w:gz') as tar:
    for f in root.iterdir(): tar.add(f, arcname=f.name)
cli = p/'cli-source'; (cli/'bin').mkdir(parents=True); (cli/'deploy-linux').mkdir()
shutil.copy(root/'bin/zoe', cli/'bin/zoe'); shutil.copy(root/'SOURCE_REVISION', cli)
shutil.copy(repo/'deploy/linux/install-cli.sh', cli/'deploy-linux')
sums(cli)
with tarfile.open(p/'archives/zoe-linux-amd64.tar.gz', 'w:gz') as tar:
    for f in cli.iterdir(): tar.add(f, arcname=f.name)
(p/'releases/current').symlink_to(old)
(p/'bin/zoe').symlink_to(p/'releases'/old/'bin/zoe')
for unit in ['zoe-compose', 'zoe-runner', 'zoe-opencode']:
    shutil.copy(repo/'deploy/linux'/f'{unit}.service', p/'units')
db = sqlite3.connect(p/'db.sqlite')
db.executescript('create table voice_sessions(state text); create table voice_turns(id integer); '
                 'create table tasks(status text); create table schema_migrations(namespace text,version integer); '
                 "insert into schema_migrations values('voice',1); insert into voice_turns values(1); insert into tasks values('done');")
if case == 'active': db.execute("insert into voice_sessions values('active')")
db.commit(); db.close()
(p/'initial-env').write_bytes((p/'stack.env').read_bytes())
PY
  cat > "$FIXTURE/shims/shim" <<'PY'
#!/usr/bin/env python3
import datetime, json, os, pathlib, re, shutil, sqlite3, sys
p=pathlib.Path(os.environ['FIXTURE']); cmd=pathlib.Path(sys.argv[0]).name; args=sys.argv[1:]
old,new=os.environ['OLD_SHA'],os.environ['NEW_SHA']; case=os.environ['CASE']
current=(p/'releases/current').resolve().name
def record():
    with (p/'calls').open('a') as out: out.write(cmd+' '+ ' '.join(args)+'\n')
def healthy():
    if case == 'restore-required' and current == old and (p/'restarted').exists():
        with sqlite3.connect(p/'db.sqlite') as db:
            return db.execute('select max(version) from schema_migrations').fetchone()[0] == 1
    return True
def since_value():
    if '--since' not in args or args.count('--since') != 1:
        sys.exit('missing unique --since')
    return args[args.index('--since')+1]
def valid_rfc3339(value):
    if not re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})', value):
        return False
    try:
        datetime.datetime.fromisoformat(value.replace('Z','+00:00'))
    except ValueError:
        return False
    return True
if cmd == 'gh':
    if args[:2] == ['run','list']: print(json.dumps([{'databaseId':42,'headSha':new}]))
    elif args[:2] == ['run','view']: print(json.dumps({'headSha':new,'conclusion':'success','workflowName':'Server deployment artifact'}))
    elif args[:2] == ['run','download']:
        target=pathlib.Path(args[args.index('-D')+1]); name=args[args.index('-n')+1]
        source='server-linux-amd64.tar.gz' if name.startswith('server-release') else 'zoe-linux-amd64.tar.gz'
        shutil.copy(p/'archives'/source,target)
    else: sys.exit(2)
elif cmd == 'systemctl':
    if args[0] == 'cat': print((p/'units'/(args[1]+'.service')).read_text())
    else:
        record()
        if args[0] == 'is-active':
            assert args[1] == '--quiet'
            units=args[2:]
            assert units and all(unit in ['zoe-compose','zoe-runner','zoe-opencode'] for unit in units)
            # Real systemctl returns success for ANY active unit in this list.
            inactive = 'zoe-opencode' if case == 'inactive-service' and current == new else None
            sys.exit(0 if any(unit != inactive for unit in units) else 3)
        if args[:2] == ['restart','zoe-compose'] and current == new and not (p/'restarted').exists():
            (p/'restarted').touch()
            with sqlite3.connect(p/'db.sqlite') as db:
                db.executescript("insert into schema_migrations values('voice',2); insert into voice_turns values(2); insert into tasks values('done');")
elif cmd == 'curl': print('200' if healthy() else '503',end='')
elif cmd == 'journalctl':
    value=since_value()
    # Deployment's portable systemd v219 subset; RFC3339 T/Z is NOT accepted.
    if not re.fullmatch(r'@[0-9]+', value): sys.exit('unsupported journal timestamp')
    assert args == ['-u','zoe-runner','--since',value,'--no-pager']
    record()
    print('runner ready')
elif cmd == 'sleep': pass
elif cmd == 'docker':
    if args[0] == 'compose':
        if 'config' in args and '--format' in args: print((p/'config.json').read_text())
        elif 'ps' in args:
            assert args[-3:] == ['ps','-q','zoe-server']
            print('server-container')
    elif args[0] == 'pull': record()
    elif args[0] == 'inspect':
        assert args == ['inspect','server-container']
        env=json.loads((p/'config.json').read_text())['services']['zoe-server']['environment']
        print(json.dumps([{'Image':'sha256:'+'2'*64,'Config':{'Env':[k+'='+v for k,v in env.items()]}}]))
    elif args[:2] == ['image','inspect']:
        assert args == ['image','inspect','ghcr.io/test/zoe-server@sha256:'+'2'*64]
        print(json.dumps([{'Id':'sha256:'+'2'*64,'RepoDigests':['ghcr.io/test/zoe-server@sha256:'+'2'*64]}]))
    elif args[0] == 'logs':
        value=since_value()
        # Docker accepts seconds[.nanoseconds] or RFC3339, never @seconds.
        if not (re.fullmatch(r'[0-9]+(?:\.[0-9]{1,9})?', value) or valid_rfc3339(value)):
            sys.exit('unsupported Docker timestamp')
        assert args == ['logs','--since',value,'server-container']
        record()
        print('listening')
        if case in ['verify-failure','restore-required']: print('panic secret=DO_NOT_LEAK_SENTINEL')
    else: sys.exit(2)
else: sys.exit(2)
PY
  chmod 0755 "$FIXTURE/shims/shim"
  for tool in docker systemctl gh curl journalctl sleep; do ln -s shim "$FIXTURE/shims/$tool"; done
  export ZOE_RELEASES_DIR="$FIXTURE/releases" ZOE_ENV_FILE="$FIXTURE/stack.env" ZOE_DB_PATH="$FIXTURE/db.sqlite"
  export ZOE_API_URL=http://local/healthz ZOE_UNITS_DIR="$FIXTURE/units" ZOE_BIN_DIR="$FIXTURE/bin"
  export ZOE_DEPLOY_LOG_DIR="$FIXTURE/logs" ZOE_DEPLOY_HEALTH_ATTEMPTS=1
}
deploy() { PATH="$FIXTURE/shims:$PATH" bash "$repo_root/deploy/linux/deploy.sh" --sha "$new" "$@"; }
unchanged() {
  [[ "$(readlink -f "$FIXTURE/releases/current")" == "$FIXTURE/releases/$old" ]]
  [[ "$(readlink -f "$FIXTURE/bin/zoe")" == "$FIXTURE/releases/$old/bin/zoe" ]]
  cmp -s "$FIXTURE/initial-env" "$FIXTURE/stack.env"
  [[ ! -f "$FIXTURE/db.sqlite.pre-${new:0:12}" ]]
  [[ ! -f "$FIXTURE/calls" ]]
}
for failure in active missing-unit-path undefined-variable checksum; do
  fixture "$failure"
  if deploy; then echo "expected refusal: $failure"; exit 1; fi
  unchanged
done
fixture dry-run
deploy --dry-run
unchanged
[[ ! -e "$FIXTURE/releases/$new" && ! -e "$FIXTURE/releases/$new.dl" ]]
fixture happy
# Regression: the former shared RFC3339 argument fails on journalctl v219;
# Docker accepts it, but rejects the journal's @epoch form.
if PATH="$FIXTURE/shims:$PATH" journalctl -u zoe-runner --since 2026-11-12T09:31:44Z --no-pager >/dev/null 2>&1; then
  echo 'journal shim accepted the broken RFC3339 timestamp'; exit 1
fi
if PATH="$FIXTURE/shims:$PATH" docker logs --since @1794475904 server-container >/dev/null 2>&1; then
  echo 'Docker shim accepted journal-only @epoch'; exit 1
fi
PATH="$FIXTURE/shims:$PATH" docker logs --since 2026-11-12T09:31:44Z server-container >/dev/null
deploy
[[ "$(readlink -f "$FIXTURE/releases/current")" == "$FIXTURE/releases/$new" ]]
[[ "$("$FIXTURE/bin/zoe" --source-revision)" == "$new" ]]
[[ -f "$FIXTURE/logs/$new.done" && -f "$FIXTURE/db.sqlite.pre-${new:0:12}" ]]
[[ -x "$FIXTURE/releases/$old/bin/zoe" ]]
[[ ! -e "$FIXTURE/releases/$new.dl" ]]
[[ "$(grep -c '^docker pull ' "$FIXTURE/calls")" == 1 ]]
[[ "$(stat -c '%a' "$FIXTURE/stack.env.pre-${new:0:12}")" == 640 ]]
python3 - "$FIXTURE/calls" <<'PY'
import pathlib, re, sys
calls=pathlib.Path(sys.argv[1]).read_text()
journal=re.search(r'^journalctl -u zoe-runner --since @([0-9]+) --no-pager$',calls,re.M)
assert journal, 'deployment did not pass portable epoch to journalctl'
assert re.search(r'^docker logs --since '+journal[1]+r' server-container$',calls,re.M), 'Docker and journal cutoffs differ'
PY
cp "$FIXTURE/calls" "$FIXTURE/calls-before"
deploy
cmp -s "$FIXTURE/calls" "$FIXTURE/calls-before"
PATH="$FIXTURE/shims:$PATH" bash "$repo_root/deploy/linux/deploy.sh" --rollback --sha "$old"
[[ "$(readlink -f "$FIXTURE/releases/current")" == "$FIXTURE/releases/$old" ]]
[[ -f "$FIXTURE/releases/.deploy-$new/rolled-back" && -f "$FIXTURE/logs/$new.failed" ]]
# Same-SHA fresh attempt after rollback preserves original backup/state.
deploy
[[ -f "$FIXTURE/logs/$new.done" && ! -f "$FIXTURE/logs/$new.failed" ]]
[[ "$(readlink -f "$FIXTURE/releases/current")" == "$FIXTURE/releases/$new" ]]
[[ "$(cat "$FIXTURE/releases/.deploy-$new/db-backup")" == *'.attempt-'* ]]
[[ -f "$FIXTURE/db.sqlite.pre-${new:0:12}" ]]
[[ "$(find "$FIXTURE/releases" -maxdepth 1 -type d -name ".deploy-$new.attempt-*" | wc -l | tr -d ' ')" == 1 ]]
fixture incomplete
mkdir "$FIXTURE/releases/.deploy-$new"
if deploy; then echo 'incomplete transaction was accepted'; exit 1; fi
grep -q 'incomplete transaction' "$FIXTURE/logs/$new.log"
unchanged
for failure in verify-failure restore-required inactive-service; do
  fixture "$failure"
  if deploy; then echo "expected verification rollback: $failure"; exit 1; fi
  [[ "$(readlink -f "$FIXTURE/releases/current")" == "$FIXTURE/releases/$old" ]]
  [[ "$("$FIXTURE/bin/zoe" --source-revision)" == "$old" ]]
  cmp -s "$FIXTURE/initial-env" "$FIXTURE/stack.env"
  [[ -f "$FIXTURE/logs/$new.failed" && -f "$FIXTURE/releases/.deploy-$new/rolled-back" ]]
  grep -q 'deploy step=rollback status=ok' "$FIXTURE/logs/$new.log"
  if [[ "$failure" == inactive-service ]]; then
    grep -q 'deploy step=verify status=fail' "$FIXTURE/logs/$new.log"
    grep -q '^systemctl is-active --quiet zoe-opencode$' "$FIXTURE/calls"
  fi
  if grep -q DO_NOT_LEAK_SENTINEL "$FIXTURE/logs/$new.log"; then exit 1; fi
  if [[ "$failure" == restore-required ]]; then
    grep -q 'DATA LOSS:.*voice_turns=1 tasks=1' "$FIXTURE/logs/$new.log"
  else
    if grep -q 'DATA LOSS:' "$FIXTURE/logs/$new.log"; then exit 1; fi
  fi
done
echo 'host deployment transaction tests passed'
