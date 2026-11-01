#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  echo "run as root to prepare the Zoe host layout" >&2
  exit 1
fi

docker_bin="${ZOE_DOCKER_BINARY:-docker}"
if ! "$docker_bin" info >/dev/null 2>&1; then
  echo "the Docker Engine must be available before preparing the Zoe host layout" >&2
  exit 1
fi

if ! getent group docker >/dev/null; then
  echo "the Docker Engine docker group is required for the trusted runner" >&2
  exit 1
fi
if ! getent group zoe-runner >/dev/null; then
  groupadd --system zoe-runner
fi
if ! id zoe-runner >/dev/null 2>&1; then
  useradd --system --gid zoe-runner --home-dir /var/lib/zoe/runner --shell /usr/sbin/nologin zoe-runner
fi
usermod --append --groups docker zoe-runner

# Compose treats this as a deployment-owned external network so another
# local gateway can join without being re-created by the Zoe stack.
if ! "$docker_bin" network inspect zoe >/dev/null 2>&1; then
  "$docker_bin" network create zoe >/dev/null
fi

install -d -o root -g root -m 0755 /opt/zoe/releases
install -d -o root -g root -m 0750 /var/cache/zoe/builds
install -d -o root -g zoe-runner -m 0750 /var/lib/zoe
install -d -o 65532 -g 65532 -m 0700 /var/lib/zoe/server/data
install -d -o 65532 -g 65532 -m 0700 /var/lib/zoe/memory
install -d -o zoe-runner -g zoe-runner -m 0700 /var/lib/zoe/runner
install -d -o zoe-runner -g zoe-runner -m 0700 /var/lib/zoe/runner-authority
install -d -o root -g root -m 0700 /etc/zoe/secrets
install -d -o root -g zoe-runner -m 0750 /etc/zoe/runner-secrets
install -d -o root -g root -m 0700 /var/lib/zoe/opencode
for dir in xdg-config/opencode xdg-data xdg-state xdg-cache; do
  install -d -o root -g root -m 0700 "/var/lib/zoe/opencode/$dir"
done

authority=/var/lib/zoe/runner-authority/runner-deployment-authority.db
if [[ -L "$authority" ]]; then
  echo "runner deployment authority must not be a symlink" >&2
  exit 1
fi
if [[ ! -e "$authority" ]]; then
  install -o zoe-runner -g zoe-runner -m 0600 /dev/null "$authority"
fi
if [[ ! -f "$authority" ]]; then
  echo "runner deployment authority must be a regular file" >&2
  exit 1
fi
chown zoe-runner:zoe-runner "$authority"
chmod 0600 "$authority"

install -D -o root -g root -m 0644 "$(dirname "$0")/zoe-runner-tmpfiles.conf" /etc/tmpfiles.d/zoe-runner.conf
systemd-tmpfiles --create /etc/tmpfiles.d/zoe-runner.conf
systemctl daemon-reload

cat <<'EOF'
Host layout prepared. Supply runner and worker secret files under /etc/zoe/secrets with
mode 0400 and owner 65532:65532. Supply runner-token and runtime-token under
/etc/zoe/runner-secrets with mode 0400 and owner zoe-runner:zoe-runner. The
secret entrypoint rejects group- or world-readable files. Copy the example
environment files and set deployment-specific paths, profiles, model routes,
and existing external URLs.
EOF
