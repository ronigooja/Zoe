#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" == --dry-run ]]; then
  echo "would install python3-venv if missing"
  echo "would download scrapling==0.5 into a temporary directory and verify its pinned sha256"
  echo "would create /opt/scrapling/0.5/venv and install the verified wheel with unpinned transitive dependencies"
  echo "would run scrapling install and render scrapling-mcp.service on port 8000"
  echo "would status-probe port 8000 anonymously and with the stored token"
  exit 0
fi
[[ "$(id -u)" == 0 ]] || { echo 'must run as root' >&2; exit 1; }
root=/opt/scrapling
etc=/etc/scrapling
release_file=${SCRAPLING_RELEASE_FILE:-/usr/share/zoe/deploy-linux/scrapling-release.json}
version=${SCRAPLING_VERSION:-0.5}
venv="$root/$version/venv"
env_file="$etc/env"
unit=/etc/systemd/system/scrapling-mcp.service
port=${SCRAPLING_PORT:-8000}

usage() { echo "usage: $0 {install|status|print-token|uninstall} [--yes]"; }
wheel_sha256=
read_version() {
  if [[ -r "$release_file" ]]; then
    version=$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$release_file" | head -1)
    wheel_sha256=$(sed -n 's/.*"wheel_sha256": "\([^"]*\)".*/\1/p' "$release_file" | head -1)
  fi
}
install() {
  read_version
  [[ "$version" == 0.5 && "$wheel_sha256" =~ ^[0-9a-f]{64}$ ]] || { echo 'invalid Scrapling release pin' >&2; exit 1; }
  command -v python3 >/dev/null || { echo 'python3 is required' >&2; exit 1; }
  dpkg-query -W -f='${Status}' python3-venv 2>/dev/null | grep -q 'install ok installed' || { apt-get update; DEBIAN_FRONTEND=noninteractive apt-get install -y python3-venv; }
  mkdir -p "$root/$version" "$etc"
  if [[ ! -x "$venv/bin/python" ]]; then python3 -m venv "$venv"; fi
  tmpdir=$(mktemp -d)
  trap 'rm -rf -- "$tmpdir"' RETURN
  "$venv/bin/pip" download --disable-pip-version-check --no-deps --dest "$tmpdir" "scrapling==$version"
  wheel=$(find "$tmpdir" -maxdepth 1 -type f -name 'scrapling-*.whl' -print -quit)
  [[ -n "$wheel" ]] || { echo 'Scrapling wheel download missing' >&2; exit 1; }
  actual=$(sha256sum "$wheel" | awk '{print $1}')
  [[ "$actual" == "$wheel_sha256" ]] || { echo 'Scrapling wheel checksum mismatch' >&2; exit 1; }
  "$venv/bin/pip" install --disable-pip-version-check --no-cache-dir "$wheel[ai]"
  "$venv/bin/scrapling" install
  if [[ ! -s "$env_file" ]]; then umask 077; printf 'SCRAPLING_MCP_AUTH_TOKEN=%s\n' "$(openssl rand -hex 32)" > "$env_file"; fi
  chmod 0600 "$env_file"; chown root:root "$env_file"
  cat > "$unit" <<EOF
[Unit]
Description=Scrapling MCP server
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
User=root
EnvironmentFile=$env_file
ExecStart=$venv/bin/scrapling-mcp --http --host 0.0.0.0 --port $port
Restart=on-failure
MemoryMax=900M
[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload; systemctl enable --now scrapling-mcp.service
  echo "installed Scrapling $version; authentication token is stored in $env_file"
}
status() {
  read_version
  if [[ "$version" == 0.5 ]]; then echo 'PASS version=0.5'; else echo "FAIL version=$version"; fi
  state=$(systemctl is-active scrapling-mcp.service 2>/dev/null || true)
  if [[ "$state" == active ]]; then echo 'PASS service=active'; else echo "FAIL service=$state"; fi
  if ss -ltn "sport = :$port" 2>/dev/null | tail -n +2 | grep -q .; then echo "PASS listening_port=$port"; else echo "FAIL listening_port=$port"; fi
  unauth=$(curl --connect-timeout 3 --max-time 5 -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/mcp" || true)
  if [[ "$unauth" == 401 ]]; then echo 'PASS unauthenticated=401'; else echo "FAIL unauthenticated=$unauth"; fi
  token=$(sed -n 's/^SCRAPLING_MCP_AUTH_TOKEN=//p' "$env_file" | head -1)
  auth=$(curl --connect-timeout 3 --max-time 5 -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "http://127.0.0.1:$port/mcp" || true)
  if [[ -n "$token" && "$auth" != 401 && "$auth" != 403 && "$auth" != 000 ]]; then echo "PASS authenticated=$auth"; else echo "FAIL authenticated=$auth"; fi
  [[ "$state" == active && "$unauth" == 401 && -n "$token" && "$auth" != 401 && "$auth" != 403 && "$auth" != 000 ]]
}
print_token() { [[ "${1:-}" == --yes ]] || { echo 'print-token requires --yes' >&2; exit 2; }; sed -n 's/^SCRAPLING_MCP_AUTH_TOKEN=//p' "$env_file"; }
uninstall() { systemctl disable --now scrapling-mcp.service 2>/dev/null || true; rm -f "$unit"; systemctl daemon-reload; rm -rf "$root" "$etc"; }
case "${1:-}" in install) install;; status) status;; print-token) print_token "${2:-}";; uninstall) uninstall;; *) usage; exit 2;; esac
