#!/usr/bin/env python3
"""Private host deployment state operations; never emit environment values."""
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import sqlite3
import stat
import subprocess
import sys
import tarfile
import tempfile
from urllib.parse import quote


def fail(message):
    raise SystemExit(message)


def read(path):
    return json.loads(Path(path).read_text())


def atomic(path, data, mode=0o600):
    path = Path(path)
    fd, name = tempfile.mkstemp(dir=path.parent, prefix='.zoe-deploy-')
    try:
        with os.fdopen(fd, 'w') as out:
            out.write(data)
            out.flush()
            os.fsync(out.fileno())
        os.chmod(name, mode)
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def db_open(path):
    return sqlite3.connect('file:' + quote(str(Path(path).resolve())) + '?mode=ro', uri=True)


def snapshot(path):
    with db_open(path) as db:
        db.execute('BEGIN')
        maxima = dict(db.execute('select namespace,max(version) from schema_migrations group by namespace'))
        versions = {}
        for namespace, version in db.execute('select namespace,version from schema_migrations order by namespace,version'):
            versions.setdefault(namespace, []).append(version)
        # Core 9 belongs to instance_auth; 10 was never registered. Other
        # namespaces use separate ranges (voice starts at 200, for example).
        for namespace, items in versions.items():
            excluded = {9, 10} if namespace == 'core' else set()
            expected = [v for v in range(min(items), max(items) + 1) if v not in excluded]
            if len(set(items)) != len(items) or items != expected:
                fail('migration gap namespace=' + namespace)
        return {'migrations': maxima,
                'voice_turns': db.execute('select count(*) from voice_turns').fetchone()[0],
                'tasks': dict(db.execute('select status,count(*) from tasks group by status'))}


def output(command):
    return subprocess.check_output(command, stderr=subprocess.DEVNULL, text=True)


def compose(release, env, file=None):
    return json.loads(output(['docker', 'compose', '--env-file', env, '-f',
                              str(file or Path(release) / 'deploy-linux/docker-compose.yml'), 'config', '--format', 'json']))


def env_names(path):
    names = set(os.environ)
    for line in Path(path).read_text().splitlines():
        match = re.match(r'^\s*(?:export\s+)?([A-Za-z_][A-Za-z_0-9]*)\s*=', line)
        if match:
            names.add(match[1])
    return names


def qdrant_storage(release, env, config=None):
    services = (config if config is not None else compose(release, env))['services']
    if 'qdrant' not in services:
        return ''  # Older releases may not include memory services.
    mounts = [v for v in services['qdrant'].get('volumes', []) if v.get('target') == '/qdrant/storage']
    if len(mounts) != 1 or mounts[0].get('type') != 'bind':
        fail('Qdrant requires durable bind storage')
    path = Path(mounts[0]['source'])
    memory_mounts = services.get('zoe-memory-worker', {}).get('volumes', [])
    roots = [Path(v['source']) for v in memory_mounts
             if v.get('type') == 'bind' and v.get('target') == '/var/lib/zoe/memory']
    if len(roots) != 1 or path != roots[0] / 'qdrant' or not path.is_absolute():
        fail('Qdrant storage must be under the memory data root')
    if not path.parent.is_dir() or path.parent.resolve() != path.parent or path.is_symlink():
        fail('Qdrant storage requires an existing canonical memory root')
    if path.exists() and not path.is_dir():
        fail('Qdrant storage must be a directory')
    return str(path)


def image_refs(release):
    manifest = read(Path(release) / 'release-manifest.json')
    result = {}
    for service, key in [('zoe-server', 'ZOE_SERVER_IMAGE'), ('zoe-memory-worker', 'ZOE_MEMORY_WORKER_IMAGE'),
                         ('zoe-runner', 'ZOE_RUNNER_IMAGE')]:
        image = manifest.get('images', {}).get(service)
        if image is None:
            if service == 'zoe-server':
                fail('manifest missing server image')
            continue
        if not isinstance(image, dict) or not re.fullmatch(r'sha256:[0-9a-f]{64}', image.get('digest', '')):
            fail('manifest requires immutable image digest: ' + service)
        ref = image['ref'].rsplit(':', 1)[0] + '@' + image['digest']
        result[key] = ref
    return result


def masked(ref):
    return ref.split('@')[-1].removeprefix('sha256:')[:12] if '@' in ref else 'unpinned'


def validate(release, env, units, current_prefix):
    root = Path(release)
    for name in ('zoe', 'zoe-server', 'zoe-runner', 'zoe-secret-entrypoint'):
        path = root / 'bin' / name
        if not path.is_file() or path.is_symlink() or not os.access(path, os.X_OK):
            fail('missing executable bin/' + name)
    names = env_names(env)
    text = (root / 'deploy-linux/docker-compose.yml').read_text()
    for name, suffix in re.findall(r'\$\{([A-Za-z_][A-Za-z_0-9]*)([^{}]*)\}', text):
        if not suffix.startswith((':-', '-', ':+', '+')) and name not in names:
            fail('undefined Compose variable name=' + name)
    for unit in ('zoe-compose', 'zoe-runner', 'zoe-opencode'):
        path = Path(units) / (unit + '.service')
        if not path.is_file():
            fail('missing installed unit name=' + unit)
        # systemctl cat includes drop-ins and their effective path references.
        text = output(['systemctl', 'cat', unit])
        workdirs = re.findall(r'^WorkingDirectory=(.+)$', text, re.M)
        for rel in re.findall(re.escape(current_prefix.rstrip('/') + '/') + r'([^\s;"\x27]+)', text):
            candidate = root / rel
            if '..' in Path(rel).parts or not candidate.exists():
                fail('missing unit path unit=' + unit + ' path=' + rel)
            if rel.startswith('bin/') and candidate.is_file() and not os.access(candidate, os.X_OK):
                fail('non-executable unit path unit=' + unit + ' path=' + rel)
        for line in text.splitlines():
            if line.startswith(('ExecStart=', 'ExecStop=', 'ExecStartPre=')):
                # Relative Compose -f paths are relative to the unit working dir.
                tokens = shlex.split(line.split('=', 1)[1])
                if tokens and tokens[0].lstrip('-+!:@').startswith(current_prefix.rstrip('/') + '/'):
                    executable = tokens[0].lstrip('-+!:@')[len(current_prefix.rstrip('/')) + 1:]
                    if not os.access(root / executable, os.X_OK):
                        fail('non-executable unit start path unit=' + unit)
                for i, token in enumerate(tokens[:-1]):
                    if token == '-f' and not tokens[i + 1].startswith('/'):
                        if not workdirs or current_prefix.rstrip('/') not in workdirs[-1]:
                            fail('unresolved relative Compose path unit=' + unit)
                        if not (root / tokens[i + 1]).is_file():
                            fail('missing relative Compose path unit=' + unit)
    config = compose(release, env)
    # Only the validated durable Qdrant directory may be created after preflight.
    storage = qdrant_storage(release, env, config)
    for service, spec in config['services'].items():
        for volume in spec.get('volumes', []):
            if volume.get('type') == 'bind' and not Path(volume['source']).exists():
                if service == 'qdrant' and volume.get('target') == '/qdrant/storage' and volume['source'] == storage:
                    continue
                fail('missing bind source service=' + service)
    # Includes the shared memory_api_token: missing files fail before deployment.
    for kind in ('secrets', 'configs'):
        for name, spec in config.get(kind, {}).items():
            # Unconfigured optional secrets use the empty character device.
            if kind == 'secrets' and spec.get('file') == '/dev/null':
                continue
            if spec.get('file') and not Path(spec['file']).is_file():
                fail('missing ' + kind + ' source name=' + name)
    image_refs(release)


def extract(archive, target):
    with tarfile.open(archive) as tar:
        for member in tar.getmembers():
            if member.issym() or member.islnk() or not (member.isfile() or member.isdir()):
                fail('archive contains non-regular entry')
            if Path(member.name).is_absolute() or '..' in Path(member.name).parts:
                fail('archive contains unsafe path')
        # Only regular files/directories passed the checks above (Python 3.10+).
        tar.extractall(target)


command, *args = sys.argv[1:]
try:
    if command == 'snapshot':
        print(json.dumps(snapshot(args[0]), sort_keys=True))
    elif command == 'idle':
        with db_open(args[0]) as connection:
            if connection.execute("select count(*) from voice_sessions where state in ('active','allocating')").fetchone()[0]:
                fail('active or allocating voice sessions; deployment blocked')
    elif command == 'backup':
        source, destination = args
        if Path(destination).exists():
            fail('backup already exists; refusing overwrite')
        with db_open(source) as source_db, sqlite3.connect(destination) as target:
            source_db.backup(target)
            if target.execute('pragma integrity_check').fetchone()[0] != 'ok':
                fail('backup integrity check failed')
        os.chmod(destination, stat.S_IMODE(os.stat(source).st_mode))
        os.chown(destination, os.stat(source).st_uid, os.stat(source).st_gid)
    elif command == 'restore-db':
        source, destination = args
        before, after = snapshot(source), snapshot(destination)
        lost_turns = max(0, after['voice_turns'] - before['voice_turns'])
        lost_tasks = max(0, sum(after['tasks'].values()) - sum(before['tasks'].values()))
        with db_open(source) as src, sqlite3.connect(destination) as dst:
            if src.execute('pragma integrity_check').fetchone()[0] != 'ok':
                fail('backup integrity check failed')
            src.backup(dst)
        print(f'DATA LOSS: data written after deploy started is lost; voice_turns={lost_turns} tasks={lost_tasks}')
    elif command == 'migrated':
        before = read(args[0])['migrations']
        with db_open(args[1]) as connection:
            after = dict(connection.execute('select namespace,max(version) from schema_migrations group by namespace'))
        sys.exit(0 if any(version > before.get(ns, 0) for ns, version in after.items()) else 1)
    elif command == 'db-verify':
        before, after = read(args[0]), snapshot(args[1])
        if any(after['migrations'].get(ns, -1) < value for ns, value in before['migrations'].items()):
            fail('migration maxima decreased')
        if after['voice_turns'] < before['voice_turns']:
            fail('voice_turns count decreased')
        print('database counts=' + json.dumps(after, sort_keys=True))
    elif command == 'extract':
        extract(*args)
    elif command == 'validate':
        validate(*args)
    elif command == 'images':
        for key, ref in image_refs(args[0]).items():
            old = ''
            for line in Path(args[1]).read_text().splitlines():
                if re.match(r'^\s*' + key + r'\s*=', line):
                    old = line.split('=', 1)[1].strip().strip('\"\x27')
            if old.split('@')[-1] == ref.split('@')[-1]:
                continue
            print(key + '=' + ref)
    elif command == 'edit-images':
        release, env = args
        path = Path(env)
        lines = path.read_text().splitlines(keepends=True)
        for key, ref in image_refs(release).items():
            matching = [i for i, line in enumerate(lines) if re.match(r'^\s*' + key + r'\s*=', line)]
            if len(matching) > 1:
                fail('duplicate image variable name=' + key)
            old = lines[matching[0]].split('=', 1)[1].strip().strip('\"\x27') if matching else ''
            if old.split('@')[-1] == ref.split('@')[-1]:
                continue
            print('image name=' + key + ' before=' + masked(old) + ' after=' + masked(ref))
            if matching:
                lines[matching[0]] = key + '=' + ref + '\n'
            else:
                if lines and not lines[-1].endswith('\n'):
                    lines[-1] += '\n'
                lines.append(key + '=' + ref + '\n')
        uid, gid = path.stat().st_uid, path.stat().st_gid
        atomic(path, ''.join(lines), stat.S_IMODE(path.stat().st_mode))
        os.chown(path, uid, gid)
    elif command == 'image-summary':
        for line in Path(args[0]).read_text().splitlines():
            if line.startswith('ZOE_SERVER_IMAGE='):
                print('current server image=' + masked(line.split('=', 1)[1].strip('\"\x27')))
    elif command == 'container-verify':
        release, env, container, expected = args
        spec = json.loads(output(['docker', 'inspect', container]))[0]
        desired = image_refs(release)['ZOE_SERVER_IMAGE']
        image = json.loads(output(['docker', 'image', 'inspect', desired]))[0]
        if spec['Image'] != image['Id']:
            fail('running server image ID mismatch')
        if desired not in image.get('RepoDigests', []):
            fail('running server digest not in immutable image metadata')
        running = dict(entry.split('=', 1) for entry in spec['Config']['Env'] if '=' in entry)
        rendered = compose(release, env)['services']['zoe-server']['environment']
        for key in Path(expected).read_text().splitlines():
            if not key or key.startswith('#'):
                continue
            if key not in running:
                fail('missing running environment name=' + key)
            if key in ('ZOE_VOICE_UPSTREAM_ROUTE', 'ZOE_STEWARD_TIMEOUT_MS', 'ZOE_VOICE_CLASSIFICATION_TIMEOUT_MS'):
                if running[key] != str(rendered[key]):
                    fail('running environment mismatch name=' + key)
            print('container environment name=' + key + ' verified')
    elif command == 'logs':
        path, kind = args
        text = Path(path).read_text(errors='replace')
        bad = [line for line in text.splitlines() if re.search(r'panic|fatal|migration', line, re.I)]
        if kind == 'runner':
            # Structured retry diagnostics may mention error_kind without being errors.
            # Keep the content-based fallback for legacy, unlevelled runner logs.
            bad = [line for line in text.splitlines() if
                   re.search(r'panic|fatal', line, re.I) or
                   re.search(r'\blevel=(?:error|fatal|panic)\b', line, re.I) or
                   (not re.search(r'\blevel=', line, re.I) and
                    re.search(r'error', line, re.I))]
        if bad:
            print('log failures count=' + str(len(bad)))
            # Never echo arbitrary log content: only allowlisted diagnostic words.
            for line in bad[:5]:
                print('sanitized log: ' + ' '.join(re.findall(r'(?i)\b(?:panic|fatal|migration|error|failed|database|refused)\b', line)))
            fail('service log verification failed')
        if kind == 'server' and not re.search(r'listening', text, re.I):
            fail('server listening log missing')
    elif command == 'compose':
        print(json.dumps(compose(*args)))
    elif command == 'qdrant-storage':
        print(qdrant_storage(*args))
    elif command == 'atomic-copy':
        source, destination = map(Path, args)
        atomic(destination, source.read_text(), stat.S_IMODE(source.stat().st_mode))
        os.chown(destination, source.stat().st_uid, source.stat().st_gid)
    else:
        fail('unknown deployment state operation')
except (OSError, ValueError, KeyError, sqlite3.Error, subprocess.SubprocessError, tarfile.TarError):
    # Exceptions can include commands, SQL, paths or environment values.
    print('deployment state operation failed: ' + command, file=sys.stderr)
    sys.exit(2)
