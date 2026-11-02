#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/deploy/scrapling/install-remote.sh"
[[ "${1:-}" == --dry-run ]] || { echo 'use --dry-run'; exit 2; }
grep -q 'python3-venv' "$script"
grep -q 'MemoryMax=900M' "$script"
grep -q 'SCRAPLING_MCP_AUTH_TOKEN' "$script"
grep -q 'scrapling-mcp --http' "$script"
echo 'remote Scrapling installer dry-run checks passed'
