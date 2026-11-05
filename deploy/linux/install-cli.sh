#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
realpath_existing() {
  python3 - "$1" <<'PY'
import os, sys
path = sys.argv[1]
if not os.path.exists(path):
    raise SystemExit(1)
print(os.path.realpath(path))
PY
}
input="${1:-$script_dir/..}"
if [[ $# -gt 1 ]]; then
  echo "usage: $0 [OPERATOR_ARCHIVE.tar.gz|EXTRACTED_RELEASE]" >&2
  exit 2
fi
releases_dir="${ZOE_RELEASES_DIR:-/opt/zoe/releases}"
bin_dir="${ZOE_BIN_DIR:-/usr/local/bin}"
release_dir=""
temporary_release=""
cleanup() { [[ -n "$temporary_release" ]] && rm -rf -- "$temporary_release" || true; }
trap cleanup EXIT

if [[ "$input" == *.tar.gz ]]; then
  archive="$(realpath_existing "$input")"
  temporary_release="$(mktemp -d "${TMPDIR:-/tmp}/zoe-cli.XXXXXX")"
  tar -xzf "$archive" -C "$temporary_release"
  if [[ -f "$temporary_release/SOURCE_REVISION" ]]; then
    release_dir="$temporary_release"
  else
    candidates=("$temporary_release"/*)
    if [[ ${#candidates[@]} -ne 1 || ! -f "${candidates[0]}/SOURCE_REVISION" ]]; then
      echo "archive must contain SOURCE_REVISION at its root" >&2
      exit 1
    fi
    release_dir="${candidates[0]}"
  fi
  if [[ ! -f "$release_dir/SHA256SUMS" ]] || ! (cd "$release_dir" && sha256sum -c SHA256SUMS >/dev/null); then
    echo "release checksum verification failed" >&2
    exit 1
  fi
else
  release_dir="$(realpath_existing "$input")"
fi
if [[ ! -f "$release_dir/SOURCE_REVISION" ]]; then
  echo "release must contain SOURCE_REVISION" >&2
  exit 1
fi
revision="$(tr -d '[:space:]' < "$release_dir/SOURCE_REVISION")"
if [[ ! "$revision" =~ ^[0-9a-fA-F]{7,40}$ ]]; then
  echo "SOURCE_REVISION must be a git revision" >&2
  exit 1
fi
short_revision="${revision:0:12}"
if [[ ! -f "$release_dir/bin/zoe" || ! -x "$release_dir/bin/zoe" ]]; then
  echo "release must contain an executable bin/zoe" >&2
  exit 1
fi
embedded_revision="$($release_dir/bin/zoe --source-revision 2>/dev/null || true)"
if [[ "$embedded_revision" != "$revision" ]]; then
  echo "CLI source revision mismatch: embedded=${embedded_revision:-unknown}, release=$revision" >&2
  exit 1
fi
install -d -m 0755 -- "$bin_dir" "$releases_dir"
if [[ -e "$bin_dir/zoe" && ! -L "$bin_dir/zoe" ]]; then
  echo "refusing to replace a non-symlink zoe executable" >&2
  exit 1
fi
target="$release_dir"
if [[ -n "$temporary_release" ]]; then
  target="$releases_dir/zoe-cli-$short_revision"
  if [[ -e "$target" && ! -d "$target" ]]; then
    echo "refusing to replace non-directory release $target" >&2
    exit 1
  fi
  if [[ ! -e "$target/bin/zoe" ]]; then
    stage_release="$(mktemp -d "$releases_dir/.zoe-release.XXXXXX")"
    cp -a "$release_dir"/. "$stage_release"/
    mv -- "$stage_release" "$target"
  fi
fi
target_bin="$(realpath_existing "$target/bin/zoe")"
if [[ -L "$bin_dir/zoe" && "$(realpath_existing "$bin_dir/zoe")" == "$target_bin" ]]; then
  echo "zoe CLI already installed at $target_bin"
else
  stage="$(mktemp -d "$bin_dir/.zoe-install.XXXXXX")"
  trap 'rm -rf -- "$stage"; cleanup' EXIT
  ln -s -- "$target/bin/zoe" "$stage/zoe"
  python3 - "$stage/zoe" "$bin_dir/zoe" <<'PY'
import os, sys
os.replace(sys.argv[1], sys.argv[2])
PY
  echo "Installed $bin_dir/zoe -> $target/bin/zoe"
fi
if [[ -n "$temporary_release" ]]; then
  for old in "$releases_dir"/zoe-cli-*; do
    [[ -d "$old" && "$old" != "$target" ]] || continue
    rm -rf -- "$old"
  done
fi
