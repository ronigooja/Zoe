#!/usr/bin/env bash
# Synthetic host only: no Docker daemon, systemd, network, or real host data.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
compose_skipped=false
coverage_result() {
  if [[ "$compose_skipped" == true ]]; then
    echo 'SKIPPED: real Qdrant Compose rendering (deploy/linux/docker-compose.yml and services/memory/docker-compose.yml); synthetic checks do not replace this coverage' >&2
    if [[ "${ZOE_TEST_ALLOW_COMPOSE_SKIP:-0}" != 1 ]]; then return 77; fi
    echo 'PARTIAL ONLY: ZOE_TEST_ALLOW_COMPOSE_SKIP=1 explicitly permits missing Compose coverage' >&2
  fi
}
# Log-only fixtures also run without root or Linux host utilities.
check_log_rules() {
  python3 - "$repo_root/deploy/linux/deploy-state.py" <<'PY'
import pathlib, subprocess, sys, tempfile
prefix = 'runner iteration failed; retrying after idle interval: '
cases = [
    ('runner', 'level=warn '+prefix+'error_kind=refused', True),
    ('runner', 'level=info register status=error', True),
    ('runner', 'level=debug error_kind=unknown', True),
    ('runner', 'level=error '+prefix+'error_kind=unknown', False),
    ('runner', 'LEVEL=ERROR failed', False),
    ('runner', 'level=fatal stopped', False),
    ('runner', 'level=panic stopped', False),
    ('runner', 'legacy error retrying', False),
    ('runner', 'level=warn unexpected panic anywhere', False),
    ('runner', 'level=info fatal anywhere', False),
    ('runner', 'runner ready', True),
    ('server', 'listening\nlevel=info register status=error', True),
    ('server', 'listening\nlevel=info migration failed', False),
]
with tempfile.TemporaryDirectory() as directory:
    path = pathlib.Path(directory)/'service.log'
    for kind, line, passes in cases:
        path.write_text(line+' secret=DO_NOT_LEAK_SENTINEL\n')
        result = subprocess.run([sys.executable, sys.argv[1], 'logs', str(path), kind], capture_output=True, text=True)
        assert (result.returncode == 0) == passes, (kind, line, result.returncode)
        assert 'DO_NOT_LEAK_SENTINEL' not in result.stdout+result.stderr
        if not passes:
            assert 'log failures count=1' in result.stdout
print('deployment log severity fixtures passed')
PY
}
check_log_rules
# Focused store-identity simulation; runnable without root or a Docker daemon.
check_staged_identity() {
  python3 - "$repo_root/deploy/linux/deploy-state.py" <<'PY'
import hashlib, io, json, os, pathlib, subprocess, sys, tarfile, tempfile
helper = sys.argv[1]
with tempfile.TemporaryDirectory() as directory:
    root = pathlib.Path(directory)
    stage = root/'stage'; stage.mkdir()
    release = root/'release'; release.mkdir()
    shim = root/'docker'
    shim.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
a=sys.argv[1:]; root=pathlib.Path(os.environ['IDENTITY_ROOT'])
with (root/'calls').open('a') as out: out.write(' '.join(a)+'\\n')
data=json.loads((root/'fixture.json').read_text()); case=os.environ['IDENTITY_CASE']
if a[0]=='load':
    if case=='load-error': sys.exit('secret=DO_NOT_LEAK_SENTINEL')
    print('Loaded image: fixture:test'); sys.exit()
assert a[:2]==['image','inspect']
if os.environ['IDENTITY_STORE']=='containerd' and a[2]==data['config_id']:
    sys.exit('No such image')
spec=data['spec']
if case=='inspect-error': sys.exit('secret=DO_NOT_LEAK_SENTINEL')
if case=='layers': spec['RootFS']['Layers']=['sha256:'+'9'*64]
if case=='tag-only': spec.pop('RootFS'); spec.pop('Config')
if case=='config': spec['Config']['Env']=['WRONG=1']
if case=='platform': spec['Architecture']='arm64'
print(json.dumps([spec]))
''')
    shim.chmod(0o700)
    config = {'architecture':'amd64', 'os':'linux', 'config':{'Env':['EXPECTED=1']},
              'rootfs':{'type':'layers','diff_ids':['sha256:'+'1'*64]}}
    content = json.dumps(config).encode()
    config_id = 'sha256:'+hashlib.sha256(content).hexdigest()
    images = {}; files = {}
    for name in ['zoe-server','zoe-memory-worker','zoe-runner']:
        archive = name+'.tar'
        with tarfile.open(stage/archive, 'w') as tar:
            for filename, payload in [('config.json',content), ('manifest.json',json.dumps([
                    {'Config':'config.json','RepoTags':['fixture:test'],'Layers':[]}]).encode())]:
                entry=tarfile.TarInfo(filename); entry.size=len(payload); tar.addfile(entry,io.BytesIO(payload))
        digest=hashlib.sha256((stage/archive).read_bytes()).hexdigest()
        images[name]={'archive':archive,'id':config_id,'sha256':digest}; files[archive]=digest
    (stage/'build-machine-manifest.json').write_text(json.dumps({'images':images,'files':files}))
    (release/'release-manifest.json').write_text(json.dumps({'images':dict.fromkeys(images,config_id)}))
    for store in ['classic','containerd']:
        host_id=config_id if store=='classic' else 'sha256:'+'2'*64
        (root/'fixture.json').write_text(json.dumps({'config_id':config_id, 'spec':{
            'Id':host_id,'Architecture':'amd64','Os':'linux','Config':config['config'],
            'RootFS':{'Type':'layers','Layers':config['rootfs']['diff_ids']}}}))
        for case in ['success','layers','tag-only','config','platform','load-error','inspect-error']:
            mapping=release/'host-image-ids.json'; mapping.unlink(missing_ok=True)
            (root/'calls').write_text('')
            env=dict(os.environ,PATH=str(root)+os.pathsep+os.environ['PATH'],IDENTITY_ROOT=str(root),
                     IDENTITY_CASE=case,IDENTITY_STORE=store)
            result=subprocess.run([sys.executable,helper,'staged-images',str(stage),str(release),'load'],
                                  env=env,capture_output=True,text=True)
            log=result.stdout+result.stderr
            assert (result.returncode==0)==(case=='success'), (store,case,log)
            assert 'DO_NOT_LEAK_SENTINEL' not in log
            if case=='success':
                assert all(v['host_id']==host_id for v in json.loads(mapping.read_text()).values())
                refs=subprocess.check_output([sys.executable,helper,'images',str(release),str(root/'calls')],text=True)
                assert refs.count('='+host_id)==3
                assert ('image inspect '+host_id) in (root/'calls').read_text()
                if store=='containerd':
                    assert ('image inspect '+config_id) not in (root/'calls').read_text()
                    # The simulated store explicitly rejects lookup by config digest.
                    missing=subprocess.run([str(shim),'image','inspect',config_id],env=env,capture_output=True,text=True)
                    assert missing.returncode and 'No such image' in missing.stderr
            else:
                assert not mapping.exists()
                if case.endswith('error'):
                    assert 'error=CalledProcessError' in log and 'command=docker ' in log
                    assert ('load' if case=='load-error' else 'image inspect') in log
                else:
                    assert 'loaded image ' in log and 'mismatch' in log
print('staged classic/containerd identity, immutable pinning and redacted error fixtures passed')
PY
}
check_staged_identity
if [[ "${ZOE_TEST_STAGED_IDENTITY_ONLY:-0}" == 1 ]]; then exit 0; fi
# Focused GitHub-selection simulation; no root, downloads or host commands.
python3 - "$repo_root/deploy/linux/deploy.sh" <<'PY'
import os, pathlib, subprocess, sys, tempfile
text = pathlib.Path(sys.argv[1]).read_text()
start = text.index('if [[ -z "$run_id" ]]; then', text.index('step=download-verify'))
end = text.index('\nfi\nif [[ "$dry_run"', start)
selection = text[start:end]
with tempfile.TemporaryDirectory() as directory:
    root = pathlib.Path(directory)
    gh = root/'gh'
    gh.write_text('''#!/usr/bin/env python3
import json, os, sys
with open(os.environ['CALLS'], 'a') as stream: stream.write(' '.join(sys.argv[1:])+'\\n')
if sys.argv[2] == 'list':
    print(json.dumps([{'databaseId':99,'headSha':'f'*40},{'databaseId':42,'headSha':'2'*40}]))
else:
    result={'headSha':'2'*40,'conclusion':'success','workflowName':'Server deployment artifact'}
    if os.environ.get('BAD'): result[os.environ['BAD']]='wrong'
    print(json.dumps(result))
''')
    gh.chmod(0o700)
    for server, cli, expected in [('', '', ('42','42')), ('','43',('42','43')), ('44','',('44','44')), ('44','43',('44','43'))]:
        for bad in ['', 'headSha', 'conclusion', 'workflowName']:
            calls = root/'calls'; calls.unlink(missing_ok=True)
            env = dict(os.environ, PATH=str(root)+os.pathsep+os.environ['PATH'], CALLS=str(calls), BAD=bad,
                       run_id=server, cli_run_id=cli, sha='2'*40, scratch=str(root))
            result = subprocess.run(['bash','-eu','-c',selection], env=env, capture_output=True)
            assert (result.returncode == 0) == (not bad)
            lines = calls.read_text().splitlines()
            assert any(line.startswith('run list ') for line in lines) == (not server)
            if not bad:
                assert [line.split()[2] for line in lines if line.startswith('run view ')] == list(expected)
print('GitHub automatic/explicit run-selection fixtures passed')
PY
# Check the current long-syntax YAML layout even without Docker or PyYAML.
python3 - "$repo_root" <<'PY'
import pathlib, re, sys
repo = pathlib.Path(sys.argv[1])
compose = (repo/'deploy/linux/docker-compose.yml').read_text()
assert compose.count('ZOE_MEMORY_API_TOKEN_FILE: /run/secrets/memory_api_token') == 2
assert compose.count('- source: memory_api_token') == 2
assert 'ZOE_MEMORY_API_URL: http://zoe-memory-worker:8788' in compose
assert 'ZOE_MEMORY_API_ADDR: 0.0.0.0:8788' in compose
expected = (repo/'deploy/linux/expected-env.txt').read_text().splitlines()
assert 'ZOE_MEMORY_API_URL' in expected and 'ZOE_MEMORY_API_TOKEN_FILE' in expected
def block(text, name, indent):
    matches = list(re.finditer(r'^'+' '*indent+re.escape(name)+r':\s*\n', text, re.M))
    assert len(matches) == 1, ('missing or duplicate block', name)
    start = matches[0].end()
    end = re.search(r'^ {0,'+str(indent)+r'}\S', text[start:], re.M)
    return text[start:start+end.start()] if end else text[start:]
for relative in ['deploy/linux/docker-compose.yml', 'services/memory/docker-compose.yml']:
    text = (repo/relative).read_text()
    qdrant = block(block(text, 'services', 0), 'qdrant', 2)
    volumes = block(qdrant, 'volumes', 4)
    assert re.fullmatch(
        r'      - type: bind\n'
        r'        source: \$\{ZOE_MEMORY_DATA_ROOT(?::[^}\n]*)?\}/qdrant\n'
        r'        target: /qdrant/storage\n'
        r'        bind:\n'
        r'          create_host_path: false\n\s*', volumes), (relative, volumes)
    # Inspect all tmpfs spellings, rejecting unsupported inline YAML as well.
    tmpfs = ''
    if re.search(r'^    tmpfs:', qdrant, re.M):
        tmpfs = block(qdrant, 'tmpfs', 4)
        entries = [line.strip() for line in tmpfs.splitlines() if line.strip()]
        assert entries and all(re.fullmatch(r'- /tmp(?::[^\n]*)?', line) for line in entries), (relative, tmpfs)
    if relative == 'deploy/linux/docker-compose.yml':
        assert tmpfs, relative
        for setting in ['user: "1000:1000"', 'read_only: true', 'mem_limit: 2g']:
            assert qdrant.splitlines().count('    '+setting) == 1, (relative, setting)
print('Qdrant static Compose storage fixtures passed (both files; production hardening retained)')
PY
# Render real Compose files when the CLI is available; no daemon is needed.
if command -v docker >/dev/null && docker compose version >/dev/null 2>&1; then
  python3 - "$repo_root" <<'PY'
import os, pathlib, re, subprocess, sys, tempfile
import json
repo = pathlib.Path(sys.argv[1])
with tempfile.TemporaryDirectory() as directory:
    env_file = pathlib.Path(directory)/'stack.env'
    for relative in ['deploy/linux/docker-compose.yml', 'services/memory/docker-compose.yml']:
        file = repo/relative
        values = {name: 'fixture' for name in re.findall(r'\$\{([A-Z_0-9]+):\?', file.read_text())}
        values['ZOE_MEMORY_DATA_ROOT'] = str(pathlib.Path(directory)/'memory')
        env_file.write_text(''.join(name+'='+value+'\n' for name, value in values.items()))
        # Do not let the developer's exported settings override this fixture.
        env = {name: value for name, value in os.environ.items() if not name.startswith('ZOE_')}
        rendered = subprocess.check_output([sys.executable, str(repo/'deploy/linux/deploy-state.py'),
                                            'compose', str(repo), str(env_file), str(file)], env=env, text=True)
        qdrant = json.loads(rendered)['services']['qdrant']
        mounts = qdrant.get('volumes', [])
        assert len(mounts) == 1, (relative, mounts)
        mount = mounts[0]
        assert mount['type'] == 'bind', (relative, mount)
        assert mount['source'] == values['ZOE_MEMORY_DATA_ROOT']+'/qdrant', (relative, mount)
        assert mount['target'] == '/qdrant/storage', (relative, mount)
        # Compose omits false (the long-syntax bind default) in normalized JSON.
        assert mount.get('bind', {}).get('create_host_path', False) is False, (relative, mount)
        tmpfs = qdrant.get('tmpfs', [])
        assert not any(path.split(':', 1)[0] == '/qdrant/storage' for path in tmpfs), relative
        assert qdrant['environment']['QDRANT__STORAGE__SNAPSHOTS_PATH'] == '/qdrant/storage/snapshots', relative
        if relative == 'deploy/linux/docker-compose.yml':
            assert qdrant['user'] == '1000:1000' and qdrant['read_only'] is True, relative
            assert int(qdrant['mem_limit']) == 2*1024*1024*1024, relative
            assert len(tmpfs) == 1 and tmpfs[0].startswith('/tmp:'), (relative, tmpfs)
print('Qdrant rendered Compose storage fixtures passed (both files)')
PY
else
  compose_skipped=true
  echo 'SKIPPED: Qdrant rendered Compose storage fixtures (both files): docker compose CLI unavailable; final status will be 77 unless ZOE_TEST_ALLOW_COMPOSE_SKIP=1' >&2
fi
# These read-only path checks also run on a non-root development host.
python3 - "$repo_root/deploy/linux/deploy-state.py" <<'PY'
import contextlib, io, json, pathlib, runpy, subprocess, sys, tempfile
from unittest.mock import patch
helper = sys.argv[1]
with tempfile.TemporaryDirectory() as directory:
    root = pathlib.Path(directory).resolve()
    config = {'services': {
        'qdrant': {'volumes': [{'type': 'bind', 'source': str(root/'qdrant'), 'target': '/qdrant/storage'}]},
        'zoe-memory-worker': {'volumes': [{'type': 'bind', 'source': str(root), 'target': '/var/lib/zoe/memory'}]}}}
    def storage():
        global operations
        sys.argv = [helper, 'qdrant-storage', 'release', 'env']
        with patch.object(subprocess, 'check_output', return_value=json.dumps(config)), contextlib.redirect_stdout(io.StringIO()) as out:
            operations = runpy.run_path(helper, run_name='__main__')
        return out.getvalue().strip()
    assert storage() == str(root/'qdrant')
    assert not (root/'qdrant').exists(), 'path discovery mutated storage'
    qdrant = config['services'].pop('qdrant')
    assert storage() == '', 'release without Qdrant must remain supported'
    config['services']['qdrant'] = qdrant
    original_volumes = qdrant.pop('volumes')
    try:
        storage()
    except SystemExit:
        pass
    else:
        raise AssertionError('Qdrant without durable storage accepted')
    qdrant['volumes'] = original_volumes
    (root/'qdrant').mkdir(mode=0o700)
    assert storage() == str(root/'qdrant')
    for change in [{'type': 'tmpfs'}, {'source': str(root/'other') }]:
        mount = config['services']['qdrant']['volumes'][0]
        original = mount.copy()
        mount.update(change)
        try:
            storage()
        except SystemExit:
            pass
        else:
            raise AssertionError('unsafe Qdrant storage accepted')
        mount.update(original)
    # Exercise the preflight exemption without Docker, root, or host mutation.
    storage()
    (root/'qdrant').rmdir()
    release = root/'release'
    (release/'bin').mkdir(parents=True)
    (release/'deploy-linux').mkdir()
    (release/'deploy-linux/docker-compose.yml').write_text('services: {}\n')
    (release/'release-manifest.json').write_text(json.dumps({'images': {
        'zoe-server': {'digest': 'sha256:'+'1'*64, 'ref': 'test/server:test'}}}))
    for name in ['zoe', 'zoe-server', 'zoe-runner', 'zoe-secret-entrypoint']:
        executable = release/'bin'/name
        executable.touch()
        executable.chmod(0o700)
    units = root/'units'
    units.mkdir()
    for unit in ['zoe-compose', 'zoe-runner', 'zoe-opencode']:
        (units/(unit+'.service')).touch()
    env = root/'stack.env'
    env.touch()
    def validate():
        def output(args, **kwargs):
            return json.dumps(config) if args[0] == 'docker' else ''
        with patch.object(subprocess, 'check_output', side_effect=output):
            operations['validate'](str(release), str(env), str(units), '/unused/current')
    validate()
    assert not (root/'qdrant').exists(), 'preflight created storage'
    qdrant = config['services'].pop('qdrant')
    validate()
    config['services']['qdrant'] = qdrant
    for service, spec in [
        ('qdrant', {'volumes': []}),
        ('qdrant', {'volumes': [{'type': 'tmpfs', 'target': '/qdrant/storage'}]}),
        ('other', {'volumes': [{'type': 'bind', 'source': str(root/'missing')}]}),
    ]:
        original = config['services'].get(service)
        config['services'][service] = spec
        try:
            validate()
        except SystemExit:
            pass
        else:
            raise AssertionError('unsafe preflight accepted: '+service)
        if original is None:
            del config['services'][service]
        else:
            config['services'][service] = original
    # Only the planned memory secret may be absent; configs/other secrets fail.
    secrets_dir = root/'secrets'
    secrets_dir.mkdir()
    env.write_text('ZOE_RUNNER_TOKEN_FILE='+str(secrets_dir/'runner-token')+'\n')
    token = secrets_dir/'memory-api-token'
    config['secrets'] = {'memory_api_token': {'file': str(token)}}
    validate()
    assert not token.exists() and 'ZOE_MEMORY_API_TOKEN_FILE=' not in env.read_text()
    for kind, name, path in [('secrets', 'other', token), ('configs', 'memory_api_token', token),
                             ('secrets', 'memory_api_token', root/'unexpected')]:
        original = config.get(kind)
        config[kind] = {name: {'file': str(path)}}
        try:
            validate()
        except SystemExit:
            pass
        else:
            raise AssertionError('unplanned missing file accepted: '+kind+'/'+name)
        if original is None:
            del config[kind]
        else:
            config[kind] = original
    env.write_text('UNRELATED=$(touch SHOULD_NEVER_RUN)\n')
    try:
        operations['memory_token_path'](str(env))
    except SystemExit as error:
        assert 'cannot determine' in str(error)
    else:
        raise AssertionError('missing runner paths accepted')
    env.write_text('ZOE_RUNNER_TOKEN_FILE='+str(secrets_dir/'runner-token')+'\n')
    token.write_text('synthetic-token')
    token.chmod(0o600)
    real_stat = pathlib.Path.stat
    def token_stat(path, *args, **kwargs):
        result = real_stat(path, *args, **kwargs)
        if path == token:
            fields = list(result)
            fields[4:6] = [65532, 65532]
            return type(result)(fields)
        return result
    with patch.object(pathlib.Path, 'stat', token_stat):
        assert operations['memory_token_path'](str(env)) == str(token)
        original = env.read_bytes()
        with contextlib.redirect_stdout(io.StringIO()) as out:
            operations['provision_memory_token'](str(env))
        assert out.getvalue() == 'deploy step=memory-token status=present\n'
        assert token.read_text() == 'synthetic-token'
        assert env.read_bytes() == original + ('ZOE_MEMORY_API_TOKEN_FILE='+str(token)+'\n').encode()
    with patch.object(pathlib.Path, 'stat', lambda path, *a, **kw: real_stat(path, *a, **kw)):
        if token.stat().st_uid != 65532 or token.stat().st_gid != 65532:
            try:
                operations['memory_token_path'](str(env))
            except SystemExit as error:
                assert 'owner/group' in str(error)
        else:
            raise AssertionError('wrong token ownership accepted')
    token.unlink()
    env.write_bytes(original)
    def install_token(args, **kwargs):
        assert args[:8] == ['install', '-m', '0600', '-o', '65532', '-g', '65532', '--']
        source, ready = map(pathlib.Path, args[8:])
        assert source.parent == secrets_dir and not token.exists()
        ready.write_bytes(source.read_bytes())
        ready.chmod(0o600)
    with patch.object(subprocess, 'run', side_effect=install_token), contextlib.redirect_stdout(io.StringIO()) as out:
        operations['provision_memory_token'](str(env))
    assert out.getvalue() == 'deploy step=memory-token status=created\n'
    assert __import__('re').fullmatch('[0-9a-f]{64}', token.read_text())
    assert token.stat().st_mode & 0o777 == 0o600
    assert not list(secrets_dir.glob('.zoe-memory-token-*'))
print('Qdrant deployment path fixtures passed')
PY
if [[ "${1:-}" == --logs-only ]]; then coverage_result; exit 0; fi
[[ $EUID -eq 0 ]] || { echo 'run test-deploy.sh as root (CI uses sudo)'; exit 1; }
task_root="$(mktemp -d)"
trap 'rm -rf -- "$task_root"' EXIT
old=1111111111111111111111111111111111111111
new=2222222222222222222222222222222222222222
export OLD_SHA="$old" NEW_SHA="$new" REPO_ROOT="$repo_root"
fixture() {
  # CASE controls injected behavior; each invocation owns an independent host.
  CASE="$1"
  FIXTURE="$(mktemp -d "$task_root/$CASE.XXXXXX")"
  export FIXTURE CASE
  python3 - <<'PY'
import hashlib, io, json, os, pathlib, re, shutil, sqlite3, tarfile
p = pathlib.Path(os.environ['FIXTURE']); repo = pathlib.Path(os.environ['REPO_ROOT'])
old, new = os.environ['OLD_SHA'], os.environ['NEW_SHA']
for name in ['releases', 'units', 'bin', 'shims', 'archives', 'data']:
    (p/name).mkdir()
(p/'secrets').mkdir()
memory_token = p/'secrets/memory-api-token'
memory_token.write_text('fixture-memory-token\n')
memory_token.chmod(0o600)
os.chown(memory_token, 65532, 65532)
env = {'ZOE_SERVER_IMAGE': 'ghcr.io/test/zoe-server@sha256:'+'1'*64,
       'ZOE_DEFAULT_PROFILE_ID': 'worker', 'ZOE_MAX_RUNS': '12',
       'ZOE_SERVER_DATA_ROOT': str(p/'data/server'),
       'ZOE_MEMORY_DATA_ROOT': str(p/'data/memory'),
       'ZOE_MEM0_LLM_API_KEY_FILE': str(p/'secrets/mem0-llm-api-key'),
       'ZOE_MEM0_LLM_BASE_URL': 'https://memory-model.invalid/v1',
       'ZOE_MEM0_LLM_MODEL': 'fixture-memory-model',
       'ZOE_RUNNER_TOKEN_FILE': str(p/'secrets/runner-token'),
       'ZOE_MEMORY_WORKER_IMAGE': 'ghcr.io/test/zoe-memory-worker@sha256:'+'3'*64,
       'ZOE_RUNNER_IMAGE': 'ghcr.io/test/zoe-runner@sha256:'+'4'*64,
       'ZOE_VOICE_UPSTREAM_ROUTE': 'avas', 'ZOE_STEWARD_TIMEOUT_MS': '30000', 'ZOE_UI_CONTROLLER_TIMEOUT_MS': '60000',
       'ZOE_VOICE_CLASSIFICATION_TIMEOUT_MS': '35000', 'ZOE_REGISTER_TIMEOUT_MS': '30000',
       'ZOE_MEMORY_BATCH_INTERVAL_HOURS': '24',
       'ZOE_MEMORY_API_URL': 'http://zoe-memory-worker:8788',
       'ZOE_MEMORY_API_TOKEN_FILE': str(memory_token),
       'ZOE_REGISTER_BASE_URL': 'https://route.invalid', 'ZOE_REGISTER_MODEL': 'route',
       'ZOE_REGISTER_API_KEY_FILE': '/run/secrets/register_api_key',
       'ZOE_COMPACT_BASE_URL': 'https://compact.invalid', 'ZOE_COMPACT_MODEL': 'compact',
       'ZOE_COMPACT_API_KEY_FILE': '/run/secrets/compact_api_key',
       'ZOE_COMPACT_TIMEOUT_MS': '600000', 'ZOE_COMPACT_MAX_CHARACTERS': '8000',
        'ZOE_COMPACT_INTERVAL_MINUTES': '60', 'ZOE_REGISTER_RAW_WINDOW_MINUTES': '60',
        'ZOE_UI_CONTROLLER_BASE_URL': 'https://ui.invalid', 'ZOE_UI_CONTROLLER_MODEL': 'ui',
         'ZOE_UI_CONTROLLER_API_KEY_FILE': '/run/secrets/ui_controller_api_key'}
# Mirror writeDeploymentFiles/defaultProfileValues in internal/ops/lifecycle.go.
env['ZOE_RUNTIME_PROFILES'] = json.dumps({name: {
    'id': name, 'harness': 'opencode', 'runtime_route_id': 'opencode',
    'model_route_id': 'default', 'role': 'super' if name == 'zoe-super' else 'worker',
    'agent': 'zoe-worker' if name == 'worker' else name,
    'permission_tier': 'full' if name == 'zoe-steward' else 'external_paths',
    'isolation_policy': 'opencode-workspace-write-v1',
    'capability_ids': ['workspace.read', 'workspace.write', 'external.read'],
    'scopes': ['workspace'],
} for name in ['worker', 'zoe-worker', 'zoe-steward', 'zoe-super']}, separators=(',', ':'))
# Required inputs come from the real product Compose, not the simplified shim.
# Never auto-fill new names: an added requirement must receive a reviewed value.
required = {name for name, suffix in re.findall(
    r'\$\{([A-Za-z_][A-Za-z_0-9]*)([^{}]*)\}',
    (repo/'deploy/linux/docker-compose.yml').read_text())
    if not suffix.startswith((':-', '-', ':+', '+'))}
missing = required - env.keys()
assert not missing, 'fixture missing required Compose inputs: '+','.join(sorted(missing))
assert all(env[name] for name in required), 'fixture has empty required Compose input'
if os.environ['CASE'] == 'happy':
    for prefix in ['ZOE_REGISTER', 'ZOE_COMPACT', 'ZOE_UI_CONTROLLER']:
        for suffix in ['BASE_URL', 'MODEL', 'API_KEY_FILE']:
            env.pop(prefix+'_'+suffix)
if os.environ['CASE'] in ['token-create', 'token-dry-run']:
    env.pop('ZOE_MEMORY_API_TOKEN_FILE')
    memory_token.unlink()
if os.environ['CASE'] == 'token-empty':
    memory_token.write_text('')
if os.environ['CASE'] == 'token-mode':
    memory_token.chmod(0o644)
(p/'stack.env').write_text(''.join(k+'='+v+'\n' for k,v in env.items()))
(p/'stack.env').chmod(0o640)
server_env = {k:v for k,v in env.items() if not k.endswith('_IMAGE')}
server_env['ZOE_MEMORY_API_TOKEN_FILE'] = '/run/secrets/memory_api_token'
# For every fixture scenario, keep required running names in sync with the verifier.
for name in (repo/'deploy/linux/expected-env.txt').read_text().splitlines():
    if name and not name.startswith('#'):
        server_env.setdefault(name, '')
config = {'secrets': {'memory_api_token': {'file': str(memory_token)}},
          'services': {'zoe-server': {'environment': server_env,
                                    'secrets': [{'source': 'memory_api_token'}],
                                    'volumes': [{'type': 'bind', 'source': str(p/'data')}]}}}
config['services']['qdrant'] = {'volumes': [{'type': 'bind', 'source': str(p/'data/qdrant'), 'target': '/qdrant/storage'}]}
config['services']['zoe-memory-worker'] = {'environment': {
    'ZOE_MEMORY_API_TOKEN_FILE': '/run/secrets/memory_api_token'},
    'secrets': [{'source': 'memory_api_token'}],
    'volumes': [{'type': 'bind', 'source': str(p/'data'), 'target': '/var/lib/zoe/memory'}]}
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
    compose = 'services:\n  zoe-server:\n    image: ${ZOE_SERVER_IMAGE}\n    environment:\n'
    compose += '      ZOE_RUNTIME_PROFILES: ${ZOE_RUNTIME_PROFILES}\n'
    optional_names = {prefix+'_'+suffix for prefix in ['ZOE_REGISTER', 'ZOE_COMPACT', 'ZOE_UI_CONTROLLER']
                      for suffix in ['BASE_URL', 'MODEL', 'API_KEY_FILE']}
    for name, value in server_env.items():
        if name not in optional_names and name != 'ZOE_RUNTIME_PROFILES':
            compose += '      '+name+': '+json.dumps(value)+'\n'
    for prefix, secret in [('ZOE_REGISTER', 'register_api_key'), ('ZOE_COMPACT', 'compact_api_key'), ('ZOE_UI_CONTROLLER', 'ui_controller_api_key')]:
        for suffix in ['BASE_URL', 'MODEL']:
            name = prefix+'_'+suffix
            compose += '      '+name+': ${'+name+':-}\n'
        compose += '      '+prefix+'_API_KEY_FILE: ${'+prefix+'_API_KEY_FILE:+/run/secrets/'+secret+'}\n'
    compose += ('    secrets:\n      - source: memory_api_token\n'
                '  qdrant:\n    image: qdrant/qdrant:v1.19.1\n    volumes:\n'
                '      - type: bind\n        source: '+str(p/'data/qdrant')+'\n        target: /qdrant/storage\n'
                '  zoe-memory-worker:\n    image: ${ZOE_MEMORY_WORKER_IMAGE}\n'
                '    environment:\n      ZOE_MEMORY_API_TOKEN_FILE: /run/secrets/memory_api_token\n'
                '    secrets:\n      - source: memory_api_token\n    volumes:\n'
                '      - type: bind\n        source: '+str(p/'data')+'\n        target: /var/lib/zoe/memory\n')
    compose += ('secrets:\n  register_api_key:\n    file: ${ZOE_REGISTER_API_KEY_FILE:-/dev/null}\n'
                 '  compact_api_key:\n    file: ${ZOE_COMPACT_API_KEY_FILE:-/dev/null}\n'
                 '  ui_controller_api_key:\n    file: ${ZOE_UI_CONTROLLER_API_KEY_FILE:-/dev/null}\n'
                '  memory_api_token:\n    file: ${ZOE_MEMORY_API_TOKEN_FILE:?set memory API token file}\n')
    (root/'deploy-linux/docker-compose.yml').write_text(compose)
root = p/'source'
case = os.environ['CASE']
if case == 'runner-unchanged-error':
    shutil.copy(p/'releases'/old/'bin/zoe-runner', root/'bin/zoe-runner')
if case == 'happy':
    config['secrets'].update({'register_api_key': {'file': '/dev/null'}, 'compact_api_key': {'file': '/dev/null'}, 'ui_controller_api_key': {'file': '/dev/null'}})
    (p/'config.json').write_text(json.dumps(config))
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
if case.startswith('staged-'):
    configs = {name: json.dumps({'architecture':'amd64','os':'linux','config':{'Env':[name]},
               'rootfs':{'type':'layers','diff_ids':['sha256:'+hashlib.sha256(name.encode()).hexdigest()]}}).encode() for name in images}
    images = {name: 'sha256:'+hashlib.sha256(configs[name]).hexdigest() for name in images}
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
if case.startswith('staged-'):
    stage = p/'archives'
    staged_images = {}
    for name, identity in images.items():
        archive = name+'.tar'
        with tarfile.open(stage/archive, 'w') as tar:
            for filename, content in [('config.json', configs[name]), ('manifest.json', json.dumps([{'Config':'config.json','RepoTags':['fixture/'+name+':test'],'Layers':[]}]).encode())]:
                entry = tarfile.TarInfo(filename); entry.size = len(content)
                tar.addfile(entry, io.BytesIO(content))
        staged_images[name] = {'archive':archive,'id':identity,'sha256':hashlib.sha256((stage/archive).read_bytes()).hexdigest()}
    (stage/'SOURCE_REVISION').write_text(new+'\n' if case != 'staged-sha' else old+'\n')
    files = {f.name:hashlib.sha256(f.read_bytes()).hexdigest() for f in sorted(stage.iterdir())}
    (stage/'SHA256SUMS').write_text(''.join(value+'  '+name+'\n' for name,value in files.items()))
    files['SHA256SUMS'] = hashlib.sha256((stage/'SHA256SUMS').read_bytes()).hexdigest()
    (stage/'build-machine-manifest.json').write_text(json.dumps({'schema':1,'source_revision':new,'builder_hostname':'fixture','go_version':'fixture','docker_version':'fixture','files':files,'images':staged_images}))
    stage.chmod(0o700)
    for f in stage.iterdir(): f.chmod(0o600)
    if case == 'staged-tamper': (stage/'zoe-server.tar').write_text('tampered')
    if case == 'staged-mode': stage.chmod(0o770)
(p/'releases/current').symlink_to(old)
(p/'bin/zoe').symlink_to(p/'releases'/old/'bin/zoe')
for unit in ['zoe-compose', 'zoe-runner', 'zoe-opencode']:
    shutil.copy(repo/'deploy/linux'/f'{unit}.service', p/'units')
db = sqlite3.connect(p/'db.sqlite')
db.executescript('create table voice_sessions(state text); create table voice_turns(id integer); '
                 'create table tasks(status text); create table schema_migrations(namespace text,version integer); '
                 "insert into schema_migrations values('core',1); insert into voice_turns values(1); insert into tasks values('done');")
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
def epoch():
    return int((p/'clock').read_text()) if (p/'clock').exists() else 1700000000
def restarted(unit):
    value = epoch()+10
    (p/'clock').write_text(str(value))
    (p/(unit+'-restart-epoch')).write_text(str(value))
def record():
    with (p/'calls').open('a') as out: out.write(cmd+' '+ ' '.join(args)+'\n')
def healthy():
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
if cmd == 'scp':
    assert len(args) == 6 and args[:4] == ['-r','-oBatchMode=yes','-oStrictHostKeyChecking=yes','--']
    assert args[4] in ['builder-nb03hk:/root/Agents/zoe-release-build/artifact-fixture',
                       'builder-vm02hk:/root/Agents/zoe-release-build/artifact-fixture']
    destination = pathlib.Path(args[5])
    assert destination.stat().st_uid == 0 and destination.stat().st_mode & 0o777 == 0o700
    (p/'pull-destination').write_text(str(destination))
    if case == 'staged-unreachable': sys.exit(255)
    shutil.copytree(p/'archives', destination/'artifact-fixture')
elif cmd == 'gh':
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
        if args[0] == 'restart' and current == new:
            restarted(args[1])
        if args[:2] == ['restart','zoe-compose'] and current == new and not (p/'restarted').exists():
            (p/'restarted').touch()
            with sqlite3.connect(p/'db.sqlite') as db:
                db.executescript("insert into schema_migrations values('voice',200); insert into voice_turns values(2); insert into tasks values('done');")
elif cmd == 'curl': print('200' if healthy() else '503',end='')
elif cmd == 'journalctl':
    value=since_value()
    # Deployment's portable systemd v219 subset; RFC3339 T/Z is NOT accepted.
    if not re.fullmatch(r'@[0-9]+', value): sys.exit('unsupported journal timestamp')
    assert args == ['-u','zoe-runner','--since',value,'--no-pager']
    record()
    cutoff = int(value[1:])
    runner_epoch = int((p/'zoe-runner-restart-epoch').read_text()) if (p/'zoe-runner-restart-epoch').exists() else epoch()
    lines = [(runner_epoch, 'level=info runner startup status=ready')]
    prefix = 'runner iteration failed; retrying after idle interval: '
    if case == 'runner-old-error':
        lines.append((runner_epoch-1, 'level=error '+prefix+'*url.Error'))
    if case in ['runner-new-error', 'runner-unchanged-error']:
        lines.append((runner_epoch+1, 'level=error '+prefix+'*url.Error'))
    if case in ['happy', 'runner-new-warn']:
        lines.append((runner_epoch, 'level=warn '+prefix+'error_kind=connection_refused'))
    for timestamp, line in sorted(lines):
        if timestamp >= cutoff:
            print(line)
elif cmd == 'date':
    assert args == ['+%s']
    print(epoch())
elif cmd == 'sleep': pass
elif cmd == 'install':
    if args == ['-d','-m','0700','-o','1000','-g','1000','--',str(p/'data/qdrant')]:
        assert not (p/'data/qdrant').exists()
        record()
        (p/'data/qdrant').mkdir(mode=0o700)
    elif len(args) == 9 and args[:7] == ['-m','0600','-o','65532','-g','65532','--']:
        source, target = map(pathlib.Path, args[7:])
        assert source.parent == p/'secrets' and target == pathlib.Path(str(source)+'.ready')
        assert re.fullmatch('[0-9a-f]{64}', source.read_text())
        assert (p/('stack.env.pre-'+new[:12])).read_bytes() == (p/'initial-env').read_bytes()
        assert (p/'stack.env').read_bytes() == (p/'initial-env').read_bytes()
        record()
        shutil.copyfile(source, target)
        target.chmod(0o600)  # Record requested ownership without chowning in the shim.
    else:
        # CLI installation still needs the host utility; never recurse into this shim.
        path = os.pathsep.join(entry for entry in os.environ['PATH'].split(os.pathsep)
                               if pathlib.Path(entry).resolve() != (p/'shims').resolve())
        real_install = shutil.which('install', path=path)
        if real_install is None:
            sys.exit('real install not found outside fixture shims')
        os.execv(real_install, [real_install, *args])
elif cmd == 'docker':
    if args[0] == 'compose':
        if 'config' in args and '--format' in args: print((p/'config.json').read_text())
        elif 'ps' in args:
            assert args[-3:] == ['ps','-q','zoe-server']
            print('server-container')
    elif args[0] in ['pull', 'load']:
        record()
        if args[0] == 'load' and case == 'staged-load-error':
            sys.exit('secret=DO_NOT_LEAK_SENTINEL')
    elif args[0] == 'inspect':
        assert args == ['inspect','server-container']
        env=json.loads((p/'config.json').read_text())['services']['zoe-server']['environment']
        identity = json.loads((p/'source/release-manifest.json').read_text())['images']['zoe-server'] if case.startswith('staged-') else 'sha256:'+'2'*64
        print(json.dumps([{'Image':identity,'Config':{'Env':[k+'='+v for k,v in env.items()]}}]))
    elif args[:2] == ['image','inspect']:
        if case.startswith('staged-'):
            import tarfile
            for name, identity in json.loads((p/'source/release-manifest.json').read_text())['images'].items():
                if args[2] not in [identity, 'fixture/'+name+':test']: continue
                with tarfile.open(p/'archives'/(name+'.tar')) as tar:
                    config=json.load(tar.extractfile('config.json'))
                layers=config['rootfs']['diff_ids'] if case != 'staged-image' else []
                print(json.dumps([{'Id':identity,'Architecture':'amd64','Os':'linux',
                                  'Config':config['config'],'RootFS':{'Type':'layers','Layers':layers}}]))
                break
            else: sys.exit('No such image')
        else:
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
        if case == 'verify-failure': print('panic secret=DO_NOT_LEAK_SENTINEL')
    else: sys.exit(2)
else: sys.exit(2)
PY
  chmod 0755 "$FIXTURE/shims/shim"
  for tool in docker systemctl gh scp curl journalctl sleep date install; do ln -s shim "$FIXTURE/shims/$tool"; done
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
if grep -Eq 'rollback|previous-release|previous-cli|armed=' "$repo_root/deploy/linux/deploy.sh"; then
  echo 'removed deployment restoration code remains'; exit 1
fi
fixture staged-dry-run
deploy --artifact-dir "$FIXTURE/archives" --dry-run
unchanged
grep -q 'deploy summary=dry-run' "$FIXTURE/logs/$new.log"
[[ ! -e "$FIXTURE/releases/$new" && ! -e "$FIXTURE/releases/$new.dl" ]]
for scenario in success tamper sha unreachable; do
  fixture "staged-$scenario"
  if deploy --from builder-nb03hk:artifact-fixture; then
    [[ "$scenario" == success && -f "$FIXTURE/logs/$new.done" ]]
    [[ "$(grep -c '^docker load ' "$FIXTURE/calls")" == 3 ]]
    if grep -q '^docker pull ' "$FIXTURE/calls"; then exit 1; fi
  else
    [[ "$scenario" != success && -f "$FIXTURE/logs/$new.failed" ]]
    unchanged
  fi
  [[ ! -e "$(cat "$FIXTURE/pull-destination")" ]]
done
fixture staged-pull-dry-run
deploy --from builder-vm02hk:artifact-fixture --dry-run
unchanged
[[ ! -e "$(cat "$FIXTURE/pull-destination")" ]]
for source in builder-nb03hk:../escape builder-nb03hk:a/b builder-nb03hk:a..b builder-nb03hk:. builder-nb03hk: builder-other:artifact-fixture; do
  fixture "staged-invalid-${source//[^A-Za-z0-9]/_}"
  if deploy --from "$source"; then echo "invalid pull source accepted: $source"; exit 1; fi
  unchanged
  [[ ! -e "$FIXTURE/pull-destination" ]]
done
fixture staged-exclusive
for flag in --artifact-dir --run-id --cli-run-id; do
  if deploy --from builder-nb03hk:artifact-fixture "$flag" 42; then
    echo "conflicting pull source accepted: $flag"; exit 1
  fi
done
unchanged
for scenario in success tamper sha image mode load-error; do
  fixture "staged-$scenario"
  if PATH="$FIXTURE/shims:$PATH" bash "$repo_root/deploy/linux/deploy.sh" --sha "$new" --artifact-dir "$FIXTURE/archives"; then
    [[ "$scenario" == success && -f "$FIXTURE/logs/$new.done" ]]
    [[ "$(grep -c '^docker load ' "$FIXTURE/calls")" == 3 ]]
    if grep -q '^docker pull ' "$FIXTURE/calls"; then exit 1; fi
  else
    [[ "$scenario" != success && -f "$FIXTURE/logs/$new.failed" ]]
    if [[ "$scenario" == load-error ]]; then
      grep -q 'error=CalledProcessError command=docker load' "$FIXTURE/logs/$new.log"
      if grep -q DO_NOT_LEAK_SENTINEL "$FIXTURE/logs/$new.log"; then exit 1; fi
    elif [[ "$scenario" != image ]]; then
      unchanged
    else
      grep -q 'loaded image layers mismatch' "$FIXTURE/logs/$new.log"
    fi
  fi
done
for failure in active missing-unit-path undefined-variable checksum; do
  fixture "$failure"
  if deploy; then echo "expected refusal: $failure"; exit 1; fi
  unchanged
done
fixture dry-run
deploy --dry-run
unchanged
[[ ! -e "$FIXTURE/releases/$new" && ! -e "$FIXTURE/releases/$new.dl" ]]
fixture token-dry-run
deploy --dry-run
unchanged
[[ ! -e "$FIXTURE/secrets/memory-api-token" ]]
for failure in token-empty token-mode; do
  fixture "$failure"
  if deploy; then echo "expected token refusal: $failure"; exit 1; fi
  unchanged
  grep -q 'memory token file' "$FIXTURE/logs/$new.log"
done
fixture token-create
deploy
python3 - "$FIXTURE" <<'PY'
import pathlib, re, sys
p = pathlib.Path(sys.argv[1]); token = p/'secrets/memory-api-token'
value = token.read_bytes()
assert re.fullmatch(b'[0-9a-f]{64}', value)
assert token.stat().st_mode & 0o777 == 0o600
assert (p/'stack.env.pre-222222222222').read_bytes() == (p/'initial-env').read_bytes()
assert 'ZOE_MEMORY_API_TOKEN_FILE='+str(token)+'\n' in (p/'stack.env').read_text()
before = (p/'stack.env.pre-222222222222').stat(); after = (p/'stack.env').stat()
assert after.st_mode & 0o777 == 0o640
assert (after.st_uid, after.st_gid) == (before.st_uid, before.st_gid)
calls = (p/'calls').read_text()
assert calls.count('install -m 0600 -o 65532 -g 65532 -- ') == 1
assert calls.index('install -m 0600') < calls.index('systemctl restart zoe-compose')
for log in (p/'logs').glob('*'):
    if log.is_file():
        assert value not in log.read_bytes(), 'token leaked into fixture log'
(p/'token-before').write_bytes(value)
PY
cp "$FIXTURE/calls" "$FIXTURE/calls-before"
deploy
cmp -s "$FIXTURE/calls" "$FIXTURE/calls-before"
cmp -s "$FIXTURE/token-before" "$FIXTURE/secrets/memory-api-token"
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
grep -q 'deploy step=memory-token status=present' "$FIXTURE/logs/$new.log"
[[ "$(cat "$FIXTURE/secrets/memory-api-token")" == fixture-memory-token ]]
[[ ! -e "$FIXTURE/releases/$new.dl" ]]
[[ "$(grep -c '^docker pull ' "$FIXTURE/calls")" == 1 ]]
[[ "$(stat -c '%a' "$FIXTURE/stack.env.pre-${new:0:12}")" == 640 ]]
python3 - "$FIXTURE" <<'PY'
import pathlib, re, sys
p=pathlib.Path(sys.argv[1]); calls=(p/'calls').read_text()
journal=re.search(r'^journalctl -u zoe-runner --since @([0-9]+) --no-pager$',calls,re.M)
assert journal, 'deployment did not pass portable epoch to journalctl'
assert journal[1] == (p/'zoe-runner-restart-epoch').read_text(), 'runner cutoff predates its restart'
assert re.search(r'^docker logs --since 1700000000 server-container$',calls,re.M), 'server cutoff changed'
install = 'install -d -m 0700 -o 1000 -g 1000 -- '+str(p/'data/qdrant')
assert calls.splitlines().count(install) == 1
assert calls.index(install) < calls.index('systemctl restart zoe-compose')
assert (p/'data/qdrant').stat().st_mode & 0o777 == 0o700
PY
cp "$FIXTURE/calls" "$FIXTURE/calls-before"
deploy
cmp -s "$FIXTURE/calls" "$FIXTURE/calls-before"
fixture incomplete
mkdir "$FIXTURE/releases/.deploy-$new"
if deploy; then echo 'incomplete transaction was accepted'; exit 1; fi
grep -q 'incomplete transaction' "$FIXTURE/logs/$new.log"
unchanged
for success in runner-old-error runner-new-warn; do
  fixture "$success"
  # Existing storage must retain its contents, ownership and mode, not converge.
  mkdir "$FIXTURE/data/qdrant"
  chmod 0750 "$FIXTURE/data/qdrant"
  touch "$FIXTURE/data/qdrant/keep"
  before_owner="$(stat -c '%u:%g' "$FIXTURE/data/qdrant")"
  deploy
  [[ -f "$FIXTURE/data/qdrant/keep" ]]
  [[ "$(stat -c '%a:%u:%g' "$FIXTURE/data/qdrant")" == "750:$before_owner" ]]
  if grep -q '^install ' "$FIXTURE/calls"; then echo 'existing Qdrant storage was changed'; exit 1; fi
  [[ -f "$FIXTURE/releases/.deploy-$new/runner-changed" && -f "$FIXTURE/logs/$new.done" ]]
  grep -q 'deploy step=verify status=ok' "$FIXTURE/logs/$new.log"
  runner_epoch="$(cat "$FIXTURE/zoe-runner-restart-epoch")"
  grep -q "^journalctl -u zoe-runner --since @$runner_epoch --no-pager$" "$FIXTURE/calls"
done
for failure in verify-failure inactive-service runner-new-error runner-unchanged-error; do
  fixture "$failure"
  if deploy; then echo "expected verification failure: $failure"; exit 1; fi
  [[ "$(readlink -f "$FIXTURE/releases/current")" == "$FIXTURE/releases/$new" ]]
  [[ "$("$FIXTURE/bin/zoe" --source-revision)" == "$new" ]]
  [[ -f "$FIXTURE/logs/$new.failed" && -f "$FIXTURE/db.sqlite.pre-${new:0:12}" ]]
  if [[ "$failure" == runner-new-error || "$failure" == runner-unchanged-error ]]; then
    grep -q 'deploy step=verify status=fail' "$FIXTURE/logs/$new.log"
    grep -q '^log failures count=1$' "$FIXTURE/logs/$new.log"
    if [[ "$failure" == runner-unchanged-error ]]; then
      [[ ! -f "$FIXTURE/releases/.deploy-$new/runner-changed" ]]
      [[ ! -f "$FIXTURE/zoe-runner-restart-epoch" ]]
      grep -q '^journalctl -u zoe-runner --since @1700000000 --no-pager$' "$FIXTURE/calls"
      if grep -q '^systemctl restart zoe-runner$' "$FIXTURE/calls"; then exit 1; fi
    else
      [[ -f "$FIXTURE/releases/.deploy-$new/runner-changed" ]]
      runner_epoch="$(cat "$FIXTURE/zoe-runner-restart-epoch")"
      grep -q "^journalctl -u zoe-runner --since @$runner_epoch --no-pager$" "$FIXTURE/calls"
    fi
  fi
  if [[ "$failure" == inactive-service ]]; then
    grep -q 'deploy step=verify status=fail' "$FIXTURE/logs/$new.log"
    grep -q '^systemctl is-active --quiet zoe-opencode$' "$FIXTURE/calls"
  fi
  if grep -q DO_NOT_LEAK_SENTINEL "$FIXTURE/logs/$new.log"; then exit 1; fi
  if grep -q 'DATA LOSS:' "$FIXTURE/logs/$new.log"; then exit 1; fi
done
python3 - "$task_root" <<'PY'
import pathlib, re, sys
root = pathlib.Path(sys.argv[1])
values = [path.read_bytes() for pattern in ['*/secrets/memory-api-token', '*/token-before']
          for path in root.glob(pattern) if re.fullmatch(b'[0-9a-f]{64}', path.read_bytes())]
assert values
for log in root.glob('*/logs/*'):
    if log.is_file():
        assert all(value not in log.read_bytes() for value in values), 'token leaked into fixture logs'
PY
echo 'host deployment transaction tests passed'
coverage_result
