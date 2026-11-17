#!/usr/bin/env bash
# Applies on an authorized Linux amd64 build host with Go, Docker/buildx and Git.
set -euo pipefail
umask 077
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
sha="" output=""
while (($#)); do
  case "$1" in
    --sha) sha="${2:?}"; shift 2 ;;
    --artifact-dir) output="${2:?}"; shift 2 ;;
    *) exit 2 ;;
  esac
done
[[ "$sha" =~ ^[0-9a-f]{40}$ && -n "$output" && "$(uname -s -m)" == 'Linux x86_64' ]] || {
  echo 'usage: build-release.sh --sha FULL_SHA --artifact-dir NEW_DIR' >&2; exit 2;
}
[[ ! -e "$output" && ! -L "$output" ]]
mkdir -p "$(dirname "$output")"
output="$(cd "$(dirname "$output")" && pwd)/$(basename "$output")"
task="$(mktemp -d "${TMPDIR:-/tmp}/zoe-release.XXXXXX")"
builder="zoe-release-$$"
cleanup() {
  docker buildx rm "$builder" >/dev/null 2>&1 || true
  git -C "$repo" worktree remove --force "$task/source" >/dev/null 2>&1 || true
  rm -rf -- "$task"
}
trap cleanup EXIT
git -C "$repo" worktree add --detach "$task/source" "$sha" >/dev/null 2>&1
cd "$task/source"
[[ "$(git rev-parse HEAD)" == "$sha" && -z "$(git status --porcelain)" ]]
export GOCACHE="$task/go-cache" GOMODCACHE="$task/go-mod" CGO_ENABLED=0 GOOS=linux GOARCH=amd64
docker buildx create --name "$builder" --driver docker-container >/dev/null
mkdir -p "$task/server/bin" "$task/server/deploy-linux" "$task/cli/bin" "$task/cli/deploy-linux" "$task/artifacts"
log="$task/build.log"
for name in zoe-server zoe zoe-runner zoe-secret-entrypoint; do
  package="./cmd/$name"; flags='-s -w'
  if [[ "$name" == zoe ]]; then flags+=" -X main.sourceRevision=$sha"; fi
  if [[ "$name" == zoe-secret-entrypoint ]]; then package=./deploy/linux/secret-entrypoint.go; fi
  if ! go build -trimpath -buildvcs=false -ldflags="$flags" -o "$task/server/bin/$name" "$package" >"$log" 2>&1; then
    tail -n 40 "$log" >&2; exit 1
  fi
done
[[ "$("$task/server/bin/zoe" --source-revision)" == "$sha" ]]
for file in docker-compose.yml zoe-compose.service zoe-runner.service zoe-opencode.service zoe-runner-tmpfiles.conf install-cli.sh deploy.sh deploy-state.py deployment-candidate.sh expected-env.txt; do
  cp "deploy/linux/$file" "$task/server/deploy-linux/"
done
cp deploy/linux/opencode-release.json "$task/server/"
# Archive payloads must remain traversable by the installed service users;
# outer staging files remain private under umask 077.
find "$task/server" -type d -exec chmod 0755 {} +
find "$task/server" -type f -exec chmod 0644 {} +
chmod 0755 "$task/server/bin/"*
for pair in server:zoe-server memory:zoe-memory-worker runner:zoe-runner; do
  dockerfile="${pair%%:*}"; name="${pair#*:}"
  # Use the exact checked-out workflow Dockerfiles, including their pinned bases.
  if ! docker buildx build --builder "$builder" --load --platform linux/amd64 -f "deploy/linux/Dockerfile.$dockerfile" -t "$name:$sha" . >"$log" 2>&1; then
    tail -n 40 "$log" >&2; exit 1
  fi
  docker save --output "$task/artifacts/$name.tar" "$name:$sha"
done
export RELEASE_TASK="$task" RELEASE_SHA="$sha"
python3 - <<'PY'
import hashlib, json, os, pathlib, subprocess
task = pathlib.Path(os.environ['RELEASE_TASK']); root = task/'server'; sha = os.environ['RELEASE_SHA']
images = {}
for name in ('zoe-server', 'zoe-memory-worker', 'zoe-runner'):
    info = json.loads(subprocess.check_output(['docker', 'image', 'inspect', f'{name}:{sha}']))[0]
    assert info['Architecture'] == 'amd64' and info['Os'] == 'linux'
    images[name] = info['Id']
(root/'SOURCE_REVISION').write_text(sha+'\n')
(root/'IMAGE_REFERENCES').write_text(''.join(value+'\n' for value in images.values()))
def digest(p):
    h = hashlib.sha256()
    with p.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''): h.update(block)
    return h.hexdigest()
paths = sorted(p for p in root.rglob('*') if p.is_file())
(root/'SHA256SUMS').write_text(''.join(digest(p)+'  '+p.relative_to(root).as_posix()+'\n' for p in paths))
paths = sorted(p for p in root.rglob('*') if p.is_file())
artifacts = [{'name': p.name, 'path': p.relative_to(root).as_posix(), 'sha256': digest(p)} for p in paths]
(root/'release-manifest.json').write_text(json.dumps({'schema':1,'release':sha,'source_revision':sha,'architecture':'amd64','platform':'linux/amd64','artifacts':artifacts,'images':images}, indent=2)+'\n')
PY
cp "$task/server/bin/zoe" "$task/cli/bin/"
cp "$task/server/deploy-linux/install-cli.sh" "$task/cli/deploy-linux/"
cp "$task/server/SOURCE_REVISION" "$task/cli/"
chmod 0755 "$task/cli" "$task/cli/bin" "$task/cli/deploy-linux" "$task/cli/bin/zoe"
(cd "$task/cli"; sha256sum bin/zoe deploy-linux/install-cli.sh SOURCE_REVISION > SHA256SUMS)
tar -C "$task/server" -czf "$task/artifacts/server-linux-amd64.tar.gz" .
tar -C "$task/cli" -czf "$task/artifacts/zoe-linux-amd64.tar.gz" .
cp "$task/server/SOURCE_REVISION" "$task/artifacts/"
python3 - <<'PY'
import hashlib, json, os, pathlib, socket, subprocess
task = pathlib.Path(os.environ['RELEASE_TASK']); root = task/'artifacts'
def digest(p):
    h = hashlib.sha256()
    with p.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''): h.update(block)
    return h.hexdigest()
files = {p.name: digest(p) for p in sorted(root.iterdir())}
(root/'SHA256SUMS').write_text(''.join(value+'  '+name+'\n' for name,value in files.items()))
files['SHA256SUMS'] = digest(root/'SHA256SUMS')
images = json.loads((task/'server/release-manifest.json').read_text())['images']
manifest = {'schema':1,'builder_hostname':socket.gethostname(),'source_revision':os.environ['RELEASE_SHA'],
            'go_version':subprocess.check_output(['go','version'],text=True).strip(),
            'docker_version':subprocess.check_output(['docker','version','--format','{{.Client.Version}} / {{.Server.Version}}'],text=True).strip(),
            'files':files,'images':{name:{'archive':name+'.tar','sha256':files[name+'.tar'],'id':identity} for name,identity in images.items()}}
(root/'build-machine-manifest.json').write_text(json.dumps(manifest,indent=2,sort_keys=True)+'\n')
PY
mv -Tn -- "$task/artifacts" "$output"
[[ ! -e "$task/artifacts" ]] || { echo 'artifact directory already published' >&2; exit 1; }
echo "Built release $sha: $output (local image tags retained)"
