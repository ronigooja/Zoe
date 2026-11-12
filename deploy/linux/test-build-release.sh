#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp_root="$(mktemp -d)"
trap 'rm -rf -- "$tmp_root"' EXIT

fake_bin="$tmp_root/fake-bin"
mkdir -p "$fake_bin"
fake_go="$fake_bin/go"
fake_docker="$fake_bin/docker"

cat >"$fake_go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == env && "${2:-}" == GOARCH ]]; then
  printf '%s\n' "${ZOE_FAKE_GOARCH:-arm64}"
  exit 0
fi
out=""
while (($#)); do
  if [[ "$1" == "-o" ]]; then
    out="${2:?missing output path}"
    shift 2
  else
    shift
  fi
done
if [[ -n "${ZOE_FAKE_GO_FAIL_OUTPUT:-}" && "$out" == *"$ZOE_FAKE_GO_FAIL_OUTPUT"* ]]; then
  exit 31
fi
mkdir -p "$(dirname "$out")"
printf 'synthetic executable: %s\n' "$(basename "$out")" >"$out"
EOF

cat >"$fake_docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$ZOE_FAKE_DOCKER_LOG"
case "${ZOE_FAKE_DOCKER_FAIL_FILE:-}" in
  server) if [[ "$*" == *Dockerfile.server* ]]; then exit 32; fi ;;
  memory) if [[ "$*" == *Dockerfile.memory* ]]; then exit 32; fi ;;
  runner) if [[ "$*" == *Dockerfile.runner* ]]; then exit 32; fi ;;
esac
if [[ -n "${ZOE_FAKE_DOCKER_BARRIER:-}" && "$*" == *Dockerfile.runner* ]]; then
  : >"$ZOE_FAKE_DOCKER_BARRIER/ready.$PPID"
  for _ in {1..500}; do
    if [[ "$(find "$ZOE_FAKE_DOCKER_BARRIER" -maxdepth 1 -name 'ready.*' -type f | wc -l)" -ge 2 ]]; then
      exit 0
    fi
    sleep 0.01
  done
  echo "concurrent test barrier timed out" >&2
  exit 33
fi
EOF
chmod 0755 "$fake_go" "$fake_docker"

run_release() {
  local tag="$1" release_root="$2" build_root="$3" docker_log="$4"
  shift 4
  env \
    GO="$fake_go" \
    ZOE_DOCKER_BINARY="$fake_docker" \
    ZOE_BUILD_ROOT="$build_root" \
    ZOE_RELEASE_ROOT="$release_root" \
    ZOE_FAKE_DOCKER_LOG="$docker_log" \
    ZOE_FAKE_GOARCH="${ZOE_FAKE_GOARCH:-arm64}" \
    "$@" \
    bash "$repo_root/deploy/linux/build-release.sh" "$tag"
}

release_root="$tmp_root/releases"
build_root="$tmp_root/builds"
mkdir -p "$release_root" "$build_root"

# A Go failure must discard its private build/release stages so the same
# release tag can be retried after correcting the local build issue.
if run_release go-retry-tag "$release_root" "$build_root" "$tmp_root/go-fail.log" ZOE_FAKE_GO_FAIL_OUTPUT=zoe-runner; then
  echo "expected synthetic Go failure" >&2
  exit 1
fi
[[ ! -e "$release_root/go-retry-tag" && ! -L "$release_root/go-retry-tag" ]]
if find "$release_root" -maxdepth 1 -name '.go-retry-tag.staging.*' -print -quit | grep -q .; then
  echo "Go failure left its release staging directory" >&2
  exit 1
fi
if find "$build_root" -maxdepth 1 -name '.go-retry-tag.build.*' -print -quit | grep -q .; then
  echo "Go failure left its build scratch directory" >&2
  exit 1
fi
run_release go-retry-tag "$release_root" "$build_root" "$tmp_root/go-retry.log"
[[ -x "$release_root/go-retry-tag/bin/zoe-runner" ]]
grep -q -- '--platform linux/arm64' "$tmp_root/go-retry.log"

# A Docker failure must discard only its private stage so the same immutable
# release tag can be retried successfully.
if run_release retry-tag "$release_root" "$build_root" "$tmp_root/docker-fail.log" ZOE_FAKE_DOCKER_FAIL_FILE=memory; then
  echo "expected synthetic Docker failure" >&2
  exit 1
fi
[[ ! -e "$release_root/retry-tag" && ! -L "$release_root/retry-tag" ]]
if find "$release_root" -maxdepth 1 -name '.retry-tag.staging.*' -print -quit | grep -q .; then
  echo "failed build left its release staging directory" >&2
  exit 1
fi
run_release retry-tag "$release_root" "$build_root" "$tmp_root/docker-retry.log"
[[ -x "$release_root/retry-tag/bin/zoe" ]]
[[ "$(cat "$release_root/retry-tag/bin/zoe")" == "synthetic executable: zoe" ]]
[[ -x "$release_root/retry-tag/bin/zoe-runner" ]]
[[ -x "$release_root/retry-tag/bin/zoe-secret-entrypoint" ]]
[[ "$(cat "$release_root/retry-tag/SOURCE_REVISION")" == "$(git -C "$repo_root" rev-parse HEAD)" ]]
(cd "$release_root/retry-tag" && sha256sum -c SHA256SUMS)
python3 - "$release_root/retry-tag/release-manifest.json" <<'PY'
import json, sys
manifest = json.load(open(sys.argv[1]))
paths = {artifact['path'] for artifact in manifest['artifacts']}
assert {'bin/zoe', 'bin/zoe-server', 'bin/zoe-runner', 'bin/zoe-secret-entrypoint', 'SOURCE_REVISION'} <= paths
PY
[[ -f "$release_root/retry-tag/deploy-linux/build-release.sh" ]]
[[ "$(cat "$release_root/retry-tag/bin/zoe-runner")" == "synthetic executable: zoe-runner" ]]
grep -q -- '--platform linux/arm64' "$tmp_root/docker-retry.log"
[[ "$(stat -c '%a' "$release_root/retry-tag")" == 755 ]]
if find "$release_root" -maxdepth 1 -name '.retry-tag.staging.*' -print -quit | grep -q .; then
  echo "successful publish left its staging directory" >&2
  exit 1
fi
if find "$build_root" -maxdepth 1 -name '.retry-tag.build.*' -print -quit | grep -q .; then
  echo "successful build left its build scratch directory" >&2
  exit 1
fi

# Existing immutable destinations of every relevant path type are untouched,
# and the script refuses before invoking the fake image builder.
mkdir "$release_root/existing-dir"
printf 'keep\n' >"$release_root/existing-dir/marker"
printf 'keep-file\n' >"$release_root/existing-file"
printf 'keep-target\n' >"$tmp_root/link-target"
ln -s "$tmp_root/link-target" "$release_root/existing-link"
for tag in existing-dir existing-file existing-link; do
  log="$tmp_root/$tag.log"
  if run_release "$tag" "$release_root" "$build_root" "$log"; then
    echo "expected existing destination refusal for $tag" >&2
    exit 1
  fi
  [[ ! -e "$log" ]]
done
[[ "$(cat "$release_root/existing-dir/marker")" == keep ]]
[[ "$(cat "$release_root/existing-file")" == keep-file ]]
[[ -L "$release_root/existing-link" && "$(readlink "$release_root/existing-link")" == "$tmp_root/link-target" ]]
[[ "$(cat "$tmp_root/link-target")" == keep-target ]]

# Two same-tag builds may both finish their Docker work, but exactly one may
# atomically publish the final release directory.
mkdir "$tmp_root/concurrent-barrier"
set +e
run_release concurrent-tag "$release_root" "$build_root" "$tmp_root/concurrent-a.log" ZOE_FAKE_DOCKER_BARRIER="$tmp_root/concurrent-barrier" >"$tmp_root/concurrent-a.out" 2>&1 &
pid_a=$!
run_release concurrent-tag "$release_root" "$build_root" "$tmp_root/concurrent-b.log" ZOE_FAKE_DOCKER_BARRIER="$tmp_root/concurrent-barrier" >"$tmp_root/concurrent-b.out" 2>&1 &
pid_b=$!
wait "$pid_a"
status_a=$?
wait "$pid_b"
status_b=$?
set -e
successes=0
failures=0
for status in "$status_a" "$status_b"; do
  if ((status == 0)); then
    ((successes += 1))
  else
    ((failures += 1))
  fi
done
[[ "$successes" == 1 && "$failures" == 1 ]]
[[ -x "$release_root/concurrent-tag/bin/zoe-runner" ]]
published_messages=0
grep -q '^Built Linux release concurrent-tag ' "$tmp_root/concurrent-a.out" && ((published_messages += 1)) || true
grep -q '^Built Linux release concurrent-tag ' "$tmp_root/concurrent-b.out" && ((published_messages += 1)) || true
[[ "$published_messages" == 1 ]]
if find "$release_root" -maxdepth 1 -name '.concurrent-tag.staging.*' -print -quit | grep -q .; then
  echo "concurrent publish left a losing staging directory" >&2
  exit 1
fi

echo "build-release staging tests passed"
