#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
printf 'candidate artifact\n' > "$tmp/server.bin"
sha="$(sha256sum "$tmp/server.bin" | awk '{print $1}')"
cat > "$tmp/manifest.json" <<JSON
{"source_revision":"0123456789012345678901234567890123456789","architecture":"amd64","artifacts":[{"name":"server","path":"server.bin","sha256":"$sha","digest":"sha256:$sha"}],"images":{"zoe-server":{"ref":"ghcr.io/ronigooja/zoe-server:0123456789012345678901234567890123456789","digest":"sha256:$sha"}}}
JSON
"$root/deploy/linux/deployment-candidate.sh" "$tmp/manifest.json" --output "$tmp/plan.json" >/dev/null
python3 - "$tmp/plan.json" <<'PY'
import json,sys
p=json.load(open(sys.argv[1])); assert p['checks']['manifest_sha256']; assert not p['checks']['host_mutation']; assert 'rollback' not in p
PY
if sed 's/"sha256":"[0-9a-f]*/"sha256":"deadbeef/' "$tmp/manifest.json" > "$tmp/bad.json" && "$root/deploy/linux/deployment-candidate.sh" "$tmp/bad.json" >/dev/null 2>&1; then
  echo 'tampered manifest unexpectedly passed' >&2; exit 1
fi
echo 'deployment-candidate checks passed'
