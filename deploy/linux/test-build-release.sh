#!/usr/bin/env bash
# Focused simulation: fake Go/Docker, real detached Git checkout; no real builds.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
task="$(mktemp -d)"
trap 'rm -rf -- "$task"' EXIT
mkdir "$task/shims"
# Keep dirty input fixtures out of the developer's working tree.
git clone --quiet --shared "$repo" "$task/input"
cp "$repo/deploy/linux/build-release.sh" "$task/input/deploy/linux/"
mkdir -p "$task/input/deploy/prompts"
for name in ui_controller register compact steward task_ui_display voice_instruction task_summary memory_ingest memory_write memory_custom; do
  printf 'fixture %s\n' "$name" > "$task/input/deploy/prompts/$name.txt"
done
git -C "$task/input" add deploy/prompts deploy/linux/build-release.sh
git -C "$task/input" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm 'fixture release prompts'
FIXTURE_SHA="$(git -C "$task/input" rev-parse HEAD)"
FIXTURE_TREE="$(git -C "$task/input" rev-parse "$FIXTURE_SHA^{tree}")"
export FIXTURE_SHA FIXTURE_TREE FIXTURE_CALLS="$task/calls"
printf '\nDIRTY INPUT MUST NOT BUILD\n' >> "$task/input/deploy/linux/Dockerfile.memory"
chmod 0600 "$task/input/services/__init__.py"
printf 'untracked\n' > "$task/input/untracked-build-input"
grep -q '^RUN chmod -R a+rX /app/services$' "$repo/deploy/linux/Dockerfile.memory"
cat > "$task/shims/go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == version ]]; then echo 'go fixture'; exit; fi
[[ "$(git write-tree)" == "$FIXTURE_TREE" && ! -e untracked-build-input ]]
cmp deploy/linux/Dockerfile.memory <(git show "$FIXTURE_SHA:deploy/linux/Dockerfile.memory")
[[ "$(python3 -c 'import os; print(oct(os.stat("services/__init__.py").st_mode & 0o777))')" == 0o644 ]]
while (($#)); do
  if [[ "$1" == -o ]]; then
    output="$2"; shift 2
  else shift; fi
done
printf '#!/bin/sh\necho %s\n' "$FIXTURE_SHA" > "$output"
chmod 0755 "$output"
SH
cat > "$task/shims/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >> "$FIXTURE_CALLS"
case "$1" in
  image) echo '[{"Id":"sha256:2222222222222222222222222222222222222222222222222222222222222222","Architecture":"amd64","Os":"linux","Config":{"User":"65532:65532"}}]' ;;
  create) echo fixture-container ;;
  export) python3 - <<'PY'
import io, os, sys, tarfile
with tarfile.open(fileobj=sys.stdout.buffer, mode='w|') as tar:
    for name, mode, directory in [('app', 0o755, True), ('app/services', 0o755, True),
                                  ('app/services/worker.py', 0o600 if os.environ.get('FIXTURE_MODE_FAIL') == 'file' else 0o644, False),
                                  ('app/private', 0o700 if os.environ.get('FIXTURE_MODE_FAIL') == 'dir' else 0o755, True),
                                  ('usr/local/bin/zoe-server', 0o755, False)]:
        entry=tarfile.TarInfo(name); entry.mode=mode
        if directory: entry.type=tarfile.DIRTYPE
        tar.addfile(entry, None if directory else io.BytesIO(b''))
PY
    ;;
  rm) : ;;
  save) printf 'synthetic image\n' > "$3" ;;
  version) echo 'docker fixture' ;;
  buildx) [[ "${FIXTURE_BUILD_FAIL:-false}" != true || "$2" != build ]] ;;
esac
SH
chmod 0755 "$task/shims/"*
mkdir "$task/wrong-cwd"
(cd "$task/wrong-cwd"; umask 077; PATH="$task/shims:$PATH" bash "$task/input/deploy/linux/build-release.sh" --sha "$FIXTURE_SHA" --artifact-dir "$task/output")
python3 - "$task/output" "$FIXTURE_SHA" "$FIXTURE_TREE" <<'PY'
import hashlib,json,pathlib,sys,tarfile
root=pathlib.Path(sys.argv[1]); m=json.loads((root/'build-machine-manifest.json').read_text())
assert m['source_revision']==sys.argv[2]
assert m['source_tree']==sys.argv[3]
assert len(m['images'])==3
assert set(m['files'])=={p.name for p in root.iterdir()}-{'build-machine-manifest.json'}
for name,digest in m['files'].items(): assert hashlib.sha256((root/name).read_bytes()).hexdigest()==digest
with tarfile.open(root/'server-linux-amd64.tar.gz') as tar:
    manifest=json.load(tar.extractfile('./release-manifest.json'))
    assert len(manifest['images'])==3
    sums=tar.extractfile('./SHA256SUMS').read().decode()
    for name in ['ui_controller', 'register', 'compact', 'steward', 'task_ui_display', 'voice_instruction', 'task_summary', 'memory_ingest', 'memory_write', 'memory_custom']:
        path='prompts/'+name+'.txt'; data=tar.extractfile('./'+path).read()
        digest=hashlib.sha256(data).hexdigest()
        assert digest+'  '+path in sums.splitlines()
        assert any(item['path']==path and item['sha256']==digest for item in manifest['artifacts'])
PY
if PATH="$task/shims:$PATH" bash "$repo/deploy/linux/build-release.sh" --sha "$FIXTURE_SHA" --artifact-dir "$task/output"; then exit 1; fi
if PATH="$task/shims:$PATH" FIXTURE_BUILD_FAIL=true bash "$task/input/deploy/linux/build-release.sh" --sha "$FIXTURE_SHA" --artifact-dir "$task/failed"; then exit 1; fi
[[ ! -e "$task/failed" ]]
for mode in file dir; do
  if PATH="$task/shims:$PATH" FIXTURE_MODE_FAIL="$mode" bash "$task/input/deploy/linux/build-release.sh" --sha "$FIXTURE_SHA" --artifact-dir "$task/failed-$mode"; then exit 1; fi
  [[ ! -e "$task/failed-$mode" ]]
done
[[ "$(grep -c '^create ' "$FIXTURE_CALLS")" == 5 && "$(grep -c '^rm -f fixture-container$' "$FIXTURE_CALLS")" == 5 ]]
echo 'build-machine release layout and failure fixtures passed'
