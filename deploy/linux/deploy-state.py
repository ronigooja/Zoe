#!/usr/bin/env python3
"""Private host deployment state operations; never emit environment values."""
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
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


def memory_token_path(env):
    values = {}
    for line in Path(env).read_text().splitlines():
        match = re.match(r'^\s*(?:export\s+)?(ZOE_(?:MEMORY_API_TOKEN|RUNNER_TOKEN)_FILE)\s*=(.*)$', line)
        if match:
            if match[1] in values:
                fail('duplicate memory token path variable name=' + match[1])
            # Parse literal paths only, never evaluate shell substitutions.
            parts = shlex.split(match[2], comments=True)
            if len(parts) != 1 or any(c in parts[0] for c in '$`\n\r'):
                fail('memory token requires a literal file path')
            values[match[1]] = parts[0]
    key = 'ZOE_MEMORY_API_TOKEN_FILE'
    if key in values:
        path = Path(values[key])
    else:
        directories = {str(Path(values[k]).parent) for k in
                       ('ZOE_RUNNER_TOKEN_FILE',) if k in values}
        if len(directories) != 1:
            fail('cannot determine memory token secrets directory from runner token paths')
        path = Path(directories.pop()) / 'memory-api-token'
    if not path.is_absolute() or path.parent.resolve() != path.parent or not path.parent.is_dir() or path.is_symlink():
        fail('memory token requires an existing canonical secrets directory and regular file')
    if path.exists():
        info = path.stat()
        if not stat.S_ISREG(info.st_mode) or info.st_size == 0:
            fail('memory token file must be regular and non-empty')
        if stat.S_IMODE(info.st_mode) != 0o600 or (info.st_uid, info.st_gid) != (65532, 65532):
            fail('memory token file requires mode 0600 and owner/group 65532:65532')
    return str(path)


def provision_memory_token(env):
    token = Path(memory_token_path(env))
    created = not token.exists()
    if created:
        fd, temporary = tempfile.mkstemp(dir=token.parent, prefix='.zoe-memory-token-')
        try:
            with os.fdopen(fd, 'w') as out:
                out.write(secrets.token_hex(32))
                out.flush()
                os.fsync(out.fileno())
            # Install onto transaction-owned scratch, never onto a live token.
            subprocess.run(['install', '-m', '0600', '-o', '65532', '-g', '65532',
                            '--', temporary, temporary + '.ready'], check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if token.exists() or token.is_symlink():
                fail('memory token appeared during provisioning; refusing overwrite')
            os.rename(temporary + '.ready', token)
        finally:
            for name in (temporary, temporary + '.ready'):
                if os.path.exists(name):
                    os.unlink(name)
    path = Path(env)
    if 'ZOE_MEMORY_API_TOKEN_FILE' not in env_names_file(env):
        # Append in place: preserve ownership, mode and every existing byte.
        with path.open('ab') as out:
            original = path.read_bytes()
            out.write((b'\n' if original and not original.endswith(b'\n') else b'') +
                      ('ZOE_MEMORY_API_TOKEN_FILE=' + shlex.quote(str(token)) + '\n').encode())
            out.flush()
            os.fsync(out.fileno())
    print('deploy step=memory-token status=' + ('created' if created else 'present'))


def env_names_file(path):
    return {m[1] for line in Path(path).read_text().splitlines()
            if (m := re.match(r'^\s*(?:export\s+)?([A-Za-z_][A-Za-z_0-9]*)\s*=', line))}


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
    resolved_path = Path(release) / 'host-image-ids.json'
    resolved = read(resolved_path) if resolved_path.exists() else {}
    result = {}
    for service, key in [('zoe-server', 'ZOE_SERVER_IMAGE'), ('zoe-memory-worker', 'ZOE_MEMORY_WORKER_IMAGE'),
                         ('zoe-runner', 'ZOE_RUNNER_IMAGE')]:
        image = manifest.get('images', {}).get(service)
        if image is None:
            if service == 'zoe-server':
                fail('manifest missing server image')
            continue
        if isinstance(image, str) and re.fullmatch(r'sha256:[0-9a-f]{64}', image):
            host = resolved.get(service)
            if host is not None:
                if host.get('config_id') != image or not re.fullmatch(r'sha256:[0-9a-f]{64}', host.get('host_id', '')):
                    fail('invalid host image identity: ' + service)
                image = host['host_id']
            result[key] = image
            continue
        if not isinstance(image, dict) or not re.fullmatch(r'sha256:[0-9a-f]{64}', image.get('digest', '')):
            fail('manifest requires immutable image digest: ' + service)
        ref = image['ref'].rsplit(':', 1)[0] + '@' + image['digest']
        result[key] = ref
    return result


def masked(ref):
    return ref.split('@')[-1].removeprefix('sha256:')[:12] if '@' in ref else 'unpinned'


def validate(release, env, units, current_prefix, prompt_dir):
    root = Path(release)
    prompt_path = Path(prompt_dir)
    if not prompt_path.is_absolute() or '..' in prompt_path.parts:
        fail('prompt directory must be absolute without parent traversal')
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
                prompts = root / 'prompts'
                if (service == 'zoe-server' and volume['source'] == prompt_dir and
                        volume.get('target') == prompt_dir and volume.get('read_only') is True and
                        prompts.is_dir() and any(p.is_file() and p.stat().st_size > 0 for p in prompts.iterdir())):
                    continue
                fail('missing bind source service=' + service)
    token = memory_token_path(env) if 'memory_api_token' in config.get('secrets', {}) else ''
    for kind in ('secrets', 'configs'):
        for name, spec in config.get(kind, {}).items():
            # Unconfigured optional secrets use the empty character device.
            if kind == 'secrets' and spec.get('file') == '/dev/null':
                continue
            if spec.get('file') and not Path(spec['file']).is_file():
                if kind == 'secrets' and name == 'memory_api_token' and spec['file'] == token:
                    continue  # Only this validated token is provisioned after backup.
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


def file_sha256(path):
    import hashlib
    digest = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(block)
    return digest.hexdigest()


def staged_copy(source, sha, destination):
    # Flat, root-owned input only; pin the directory and each file descriptor so
    # an SFTP uploader cannot swap a checked path during the private snapshot.
    root_fd = os.open(source, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        def secure(info):
            if info.st_uid != 0 or info.st_mode & 0o022:
                fail('staged input requires root ownership and no group/world write')
        secure(os.fstat(root_fd))
        target = Path(destination)
        target.mkdir(mode=0o700)
        for name in os.listdir(root_fd):
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=root_fd)
            with os.fdopen(fd, 'rb') as stream:
                info = os.fstat(stream.fileno())
                secure(info)
                if not stat.S_ISREG(info.st_mode):
                    fail('staged input must contain regular files only')
                with (target / name).open('xb') as out:
                    import shutil
                    shutil.copyfileobj(stream, out)
        manifest = read(target / 'build-machine-manifest.json')
        if manifest.get('schema') != 1 or manifest.get('source_revision') != sha:
            fail('build-machine manifest revision/schema mismatch')
        if not all(isinstance(manifest.get(k), str) and manifest[k] for k in ('builder_hostname', 'go_version', 'docker_version')):
            fail('missing builder provenance')
        files = manifest.get('files', {})
        actual = {p.name for p in target.iterdir()} - {'build-machine-manifest.json'}
        if set(files) != actual:
            fail('build-machine manifest must cover every payload file')
        for name, digest in files.items():
            if not re.fullmatch(r'[0-9a-f]{64}', digest) or file_sha256(target / name) != digest:
                fail('staged checksum mismatch')
        if (target / 'SOURCE_REVISION').read_text().strip() != sha:
            fail('staged SOURCE_REVISION mismatch')
        required = {'server-linux-amd64.tar.gz', 'zoe-linux-amd64.tar.gz', 'SOURCE_REVISION', 'SHA256SUMS'}
        if not required <= actual:
            fail('missing staged payload')
        # Require exact checksum coverage, not just whatever entries were supplied.
        expected = ''.join(files[name] + '  ' + name + '\n' for name in sorted(actual - {'SHA256SUMS'}))
        if (target / 'SHA256SUMS').read_text() != expected:
            fail('staged SHA256SUMS coverage mismatch')
    finally:
        os.close(root_fd)


def image_config_matches(saved, loaded):
    """Allow daemon-added empty defaults, never additional runtime settings."""
    if not isinstance(saved, dict) or not isinstance(loaded, dict):
        return False

    def empty(value):
        return value is None or value is False or value == '' or value == [] or value == {}

    for key, value in saved.items():
        if key not in loaded:
            return False
        actual = loaded[key]
        # JSON's false and zero compare equal in Python; only empty values
        # normalize, and all non-empty values retain their JSON types.
        if empty(value) and empty(actual):
            continue
        if type(actual) is not type(value) or actual != value:
            return False
    return all(key in saved or empty(value) for key, value in loaded.items())


def staged_images(staged, release, action):
    import hashlib
    root = Path(staged)
    manifest = read(root / 'build-machine-manifest.json')
    images = manifest.get('images', {})
    services = {'zoe-server', 'zoe-memory-worker', 'zoe-runner'}
    if set(images) != services:
        fail('staged manifest requires all three images')
    candidate = read(Path(release) / 'release-manifest.json')
    saved = {}
    for name, image in images.items():
        archive, identity = image.get('archive'), image.get('id')
        if archive != name + '.tar' or not re.fullmatch(r'sha256:[0-9a-f]{64}', identity or ''):
            fail('invalid staged image metadata')
        digest = file_sha256(root / archive)
        if image.get('sha256') != digest or manifest['files'].get(archive) != digest:
            fail('image tar checksum mismatch')
        if candidate.get('images', {}).get(name) != identity:
            fail('release/build-machine image identity mismatch')
        # Inspect the saved config itself; a pre-existing daemon image must not
        # mask a tarball which actually imports a different image (or extras).
        with tarfile.open(root / archive) as tar:
            entries = json.load(tar.extractfile('manifest.json'))
            if len(entries) != 1:
                fail('image archive must contain exactly one image')
            config = tar.extractfile(entries[0]['Config']).read()
            if 'sha256:' + hashlib.sha256(config).hexdigest() != identity:
                fail('saved image ID mismatch')
            config = json.loads(config)
            diff_ids = config.get('rootfs', {}).get('diff_ids')
            if (config.get('rootfs', {}).get('type') != 'layers' or not isinstance(diff_ids, list) or
                    any(not isinstance(item, str) or not re.fullmatch(r'sha256:[0-9a-f]{64}', item) for item in diff_ids)):
                fail('invalid saved image layers')
            saved[name] = (config, entries[0].get('RepoTags', []))
    if action == 'load':
        resolved = {}
        for name, image in images.items():
            loaded_output = output(['docker', 'load', '--input', str(root / image['archive'])])
            config, tags = saved[name]
            references = tags or re.findall(r'^Loaded image(?: ID)?: (\S+)$', loaded_output, re.M)
            if not references:
                fail('loaded image reference missing')
            host_id = None
            # A tag only locates the image; it never proves its identity. Compare
            # runtime config and content-addressed uncompressed layers, then
            # re-inspect by the host ID to close a mutable-tag race.
            for ref in references:
                for target in (ref, host_id):
                    target = target or host_id
                    loaded = json.loads(output(['docker', 'image', 'inspect', target]))
                    if len(loaded) != 1:
                        fail('loaded image count mismatch')
                    spec = loaded[0]
                    identity = spec.get('Id', '')
                    if not re.fullmatch(r'sha256:[0-9a-f]{64}', identity) or (host_id and identity != host_id):
                        fail('loaded image ID mismatch')
                    if (spec.get('Architecture') != config.get('architecture') or
                            spec.get('Os') != config.get('os') or
                            spec.get('Architecture') != 'amd64' or spec.get('Os') != 'linux' or
                            spec.get('Variant', '') != config.get('variant', '')):
                        fail('loaded image platform mismatch')
                    if (spec.get('RootFS', {}).get('Type') != 'layers' or
                            spec.get('RootFS', {}).get('Layers') != config['rootfs']['diff_ids']):
                        fail('loaded image layers mismatch')
                    if not image_config_matches(config.get('config'), spec.get('Config')):
                        fail('loaded image config mismatch')
                    host_id = identity
            resolved[name] = {'config_id': image['id'], 'host_id': host_id}
        atomic(Path(release) / 'host-image-ids.json', json.dumps(resolved, sort_keys=True) + '\n')
    elif action != 'verify':
        fail('invalid staged image action')


command, *args = sys.argv[1:]
try:
    if command == 'staged-copy':
        staged_copy(*args)
    elif command == 'staged-images':
        staged_images(*args)
    elif command == 'snapshot':
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
    elif command == 'memory-token-path':
        print(memory_token_path(*args))
    elif command == 'memory-token':
        provision_memory_token(*args)
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
        if not desired.startswith('sha256:') and desired not in image.get('RepoDigests', []):
            fail('running server digest not in immutable image metadata')
        running = dict(entry.split('=', 1) for entry in spec['Config']['Env'] if '=' in entry)
        rendered = compose(release, env)['services']['zoe-server']['environment']
        for key in Path(expected).read_text().splitlines():
            if not key or key.startswith('#'):
                continue
            if key not in running:
                fail('missing running environment name=' + key)
            if key in ('ZOE_VOICE_UPSTREAM_ROUTE', 'ZOE_STEWARD_TIMEOUT_MS', 'ZOE_UI_CONTROLLER_TIMEOUT_MS', 'ZOE_UI_CONTROLLER_MAX_RETRIES', 'ZOE_VOICE_CLASSIFICATION_TIMEOUT_MS'):
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
    else:
        fail('unknown deployment state operation')
except (OSError, ValueError, KeyError, sqlite3.Error, subprocess.SubprocessError, tarfile.TarError) as error:
    # Exceptions can include commands, SQL, paths or environment values.
    detail = ''
    if isinstance(error, subprocess.CalledProcessError):
        # Never echo argv, paths, environment or daemon stderr. Only known
        # executable/subcommand names are safe diagnostic labels.
        argv = error.cmd if isinstance(error.cmd, list) else []
        safe = {'docker', 'image', 'inspect', 'load', 'compose', 'config', 'systemctl', 'cat', 'install'}
        detail = ' command=' + ' '.join(part for part in argv[:3] if part in safe)
        detail += ' exit=' + str(error.returncode)
    print('deployment state operation failed: ' + command + ' error=' + type(error).__name__ + detail, file=sys.stderr)
    sys.exit(2)
