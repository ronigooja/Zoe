#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
task_root="$(mktemp -d)"
trap 'rm -rf -- "$task_root"' EXIT
export ZOE_BIN_DIR="$task_root/path/bin"
export ZOE_RELEASES_DIR="$task_root/releases"
for release in first second; do
  mkdir -p "$task_root/$release/bin"
  if [[ "$release" == first ]]; then revision=0123456789abcdef0123456789abcdef01234567; else revision=abcdef0123456789abcdef0123456789abcdef01; fi
  printf '#!/bin/sh\ncase "$1" in --source-revision) printf "%%s\\n" "%s";; *) printf "%%s\\n" "%s";; esac\n' "$revision" "$release" > "$task_root/$release/bin/zoe"
  chmod 0755 "$task_root/$release/bin/zoe"
  printf '%s\n' "$revision" > "$task_root/$release/SOURCE_REVISION"
  (cd "$task_root/$release" && sha256sum bin/zoe > SHA256SUMS)
  (cd "$task_root/$release" && tar -czf "$task_root/$release.tar.gz" bin SOURCE_REVISION SHA256SUMS)
done
bash "$repo_root/deploy/linux/install-cli.sh" "$task_root/first.tar.gz"
[[ "$(PATH="$ZOE_BIN_DIR:$PATH" zoe)" == first ]]
bash "$repo_root/deploy/linux/install-cli.sh" "$task_root/second.tar.gz"
[[ "$(PATH="$ZOE_BIN_DIR:$PATH" zoe)" == second ]]
[[ "$(readlink "$ZOE_BIN_DIR/zoe")" == "$ZOE_RELEASES_DIR/zoe-cli-abcdef012345/bin/zoe" ]]
before="$(readlink "$ZOE_BIN_DIR/zoe")"
bash "$repo_root/deploy/linux/install-cli.sh" "$task_root/second.tar.gz" | grep -q 'already installed'
[[ "$(readlink "$ZOE_BIN_DIR/zoe")" == "$before" ]]
[[ "$(find "$ZOE_RELEASES_DIR" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')" == 1 ]]
cp "$task_root/second.tar.gz" "$task_root/bad.tar.gz"
bad_extract="$task_root/bad-extract"
mkdir "$bad_extract"
(cd "$bad_extract" && tar -xzf "$task_root/bad.tar.gz")
printf broken > "$bad_extract/bin/zoe"
(cd "$bad_extract" && tar -czf "$task_root/bad.tar.gz" bin SOURCE_REVISION SHA256SUMS)
if bash "$repo_root/deploy/linux/install-cli.sh" "$task_root/bad.tar.gz"; then
  echo "expected checksum refusal" >&2; exit 1
fi
rm "$ZOE_BIN_DIR/zoe"
printf 'operator-owned file\n' > "$ZOE_BIN_DIR/zoe"
if bash "$repo_root/deploy/linux/install-cli.sh" "$task_root/first.tar.gz"; then
  echo "expected refusal to overwrite regular executable" >&2; exit 1
fi
[[ "$(cat "$ZOE_BIN_DIR/zoe")" == 'operator-owned file' ]]
echo 'CLI PATH installation tests passed'
