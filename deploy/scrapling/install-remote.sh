#!/usr/bin/env bash
set -euo pipefail

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
read_version() { if [[ -r "$release_file" ]]; then version=$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$release_file" | head -1); fi; }
install() {
  read_version
  command -v python3 >/dev/null || { echo 'python3 is required' >&2; exit 1; }
  dpkg-query -W -f='${Status}' python3-venv 2>/dev/null | grep -q 'install ok installed' || { apt-get update; DEBIAN_FRONTEND=noninteractive apt-get install -y python3-venv; }
  mkdir -p "$root/$version" "$etc"
  if [[ ! -x "$venv/bin/python" ]]; then python3 -m venv "$venv"; fi
  "$venv/bin/pip" install --disable-pip-version-check --no-cache-dir "scrapling[ai]==$version"
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
status() { systemctl --no-pager --full status scrapling-mcp.service || true; echo "version=$version port=$port"; }
print_token() { [[ "${1:-}" == --yes ]] || { echo 'print-token requires --yes' >&2; exit 2; }; sed -n 's/^SCRAPLING_MCP_AUTH_TOKEN=//p' "$env_file"; }
uninstall() { systemctl disable --now scrapling-mcp.service 2>/dev/null || true; rm -f "$unit"; systemctl daemon-reload; rm -rf "$root" "$etc"; }
case "${1:-}" in install) install;; status) status;; print-token) print_token "${2:-}";; uninstall) uninstall;; *) usage; exit 2;; esac
