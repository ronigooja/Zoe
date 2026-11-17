#!/usr/bin/env bash
# Synthetic Linux build-worker contract: fake Go/Docker, real detached Git checkout.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
task="$(mktemp -d)"
trap 'rm -rf -- "$task"' EXIT
mkdir "$task/shims"
FIXTURE_SHA="$(git -C "$repo" rev-parse HEAD)"
export FIXTURE_SHA FIXTURE_CALLS="$task/calls"
cat > "$task/shims/go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == version ]]; then echo 'go fixture'; exit; fi
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
  image) echo '[{"Id":"sha256:2222222222222222222222222222222222222222222222222222222222222222","Architecture":"amd64","Os":"linux"}]' ;;
  save) printf 'synthetic image\n' > "$3" ;;
  version) echo 'docker fixture' ;;
  buildx) [[ "${FIXTURE_BUILD_FAIL:-false}" != true || "$2" != build ]] ;;
esac
SH
chmod 0755 "$task/shims/"*
PATH="$task/shims:$PATH" bash "$repo/deploy/linux/build-release.sh" --sha "$FIXTURE_SHA" --artifact-dir "$task/output"
python3 - "$task/output" "$FIXTURE_SHA" <<'PY'
import hashlib,json,pathlib,sys,tarfile
root=pathlib.Path(sys.argv[1]); m=json.loads((root/'build-machine-manifest.json').read_text())
assert m['source_revision']==sys.argv[2]
assert len(m['images'])==3
assert set(m['files'])=={p.name for p in root.iterdir()}-{'build-machine-manifest.json'}
for name,digest in m['files'].items(): assert hashlib.sha256((root/name).read_bytes()).hexdigest()==digest
with tarfile.open(root/'server-linux-amd64.tar.gz') as tar:
    manifest=json.load(tar.extractfile('./release-manifest.json'))
    assert len(manifest['images'])==3
PY
if PATH="$task/shims:$PATH" bash "$repo/deploy/linux/build-release.sh" --sha "$FIXTURE_SHA" --artifact-dir "$task/output"; then exit 1; fi
if PATH="$task/shims:$PATH" FIXTURE_BUILD_FAIL=true bash "$repo/deploy/linux/build-release.sh" --sha "$FIXTURE_SHA" --artifact-dir "$task/failed"; then exit 1; fi
[[ ! -e "$task/failed" ]]
echo 'build-machine release layout and failure fixtures passed'
