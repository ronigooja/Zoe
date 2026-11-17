#!/usr/bin/env bash
# Applies to a completed build directory on NB03HK; uses forced internal-sftp only.
set -euo pipefail
umask 077
[[ $# == 2 && "$1" == --artifact-dir ]] || { echo 'usage: push-release.sh --artifact-dir DIR' >&2; exit 2; }
directory="$(cd "$2" && pwd)"
sha="$(cat "$directory/SOURCE_REVISION")"
[[ "$sha" =~ ^[0-9a-f]{40}$ && "$directory" != *[\"\\]* && "$directory" != *$'\n'* ]]
batch="$(mktemp "${TMPDIR:-/tmp}/zoe-sftp.XXXXXX")"
trap 'rm -f -- "$batch"' EXIT
# Publish the SHA directory only after the batch transfer completes; no shell.
{
  printf 'mkdir /var/lib/zoe/release-staging/%s.uploading\n' "$sha"
  printf 'put -p "%s/*" /var/lib/zoe/release-staging/%s.uploading/\n' "$directory" "$sha"
  printf 'rename /var/lib/zoe/release-staging/%s.uploading /var/lib/zoe/release-staging/%s\n' "$sha" "$sha"
} > "$batch"
sftp -oBatchMode=yes -b "$batch" zoe-release
