# ADR 0010: Host-installed OpenCode runtime

Status: accepted implementation decision. This supersedes the runtime and isolation portions of [ADR 0007](0007-zoe-owned-opencode-runtime.md); ADR 0007 remains historical for ownership and migration context.

Each host runs one dedicated, host-installed OpenCode V2 server in an independent `zoe-opencode.service`. It owns `/var/lib/zoe/opencode`, its port, password file, and configuration. The runner is only an authenticated HTTP client, so restarting `zoe-runner` does not kill sessions. CLIProxyAPI remains an unchanged external provider.

The runner creates a session with `POST /api/session` using `location.directory` set to the private per-run workspace `<ZOE_OPENCODE_WORKSPACE_ROOT>/<run-id>`. New journal rows store only the session ID; legacy container/password columns remain readable for compatibility. The create request includes `{action:"*", resource:"*", effect:"allow"}`. The service configuration must provide an equivalent non-interactive allow baseline, with narrower deny rules ordered after it under OpenCode's last-match semantics. Zoe admission, budgets, fences, audit, and stop policy remain authoritative.

Completion polls messages: idle `succeeded` is a result, `failed` is failure including its error detail, `interrupted` is stopped, and missing outcome is not terminal. Stop interrupts the session, then retries `/api/session/active` until the session is absent; that absence is the termination proof.

Unresolved create failures become `effect_unknown` and are never replayed automatically. Host-session reconciliation replaces runtime-container reconciliation; capability-container deployment and reconciliation remain Docker-backed and unchanged.

## Environment contract

The runner requires `ZOE_OPENCODE_BASE_URL` (absolute HTTP(S) URL), `ZOE_OPENCODE_PASSWORD_FILE` (absolute private Basic Auth password file), `ZOE_OPENCODE_MODEL` (server/provider model route), and `ZOE_OPENCODE_WORKSPACE_ROOT` (absolute private parent for per-run directories). The separate service owns its data directory, port, password, provider configuration, and permission configuration. Provider credentials are service-only secrets and never enter runner logs.

The accepted host lifecycle keeps the pinned Zoe-managed OpenCode binary in its
versioned host path, or explicitly validates an operator override during install
and update, and renders the service's `permissions` configuration as an ordered
allow-all baseline. The service is intentionally not systemd-filesystem-hardened;
its configured user must be able to write the authorized host workspaces and
OpenCode state.

The accepted implementation pins the operator fork release in the Zoe Linux
release artifact. Install and update download and checksum-verify the matching
Linux asset into `/opt/zoe/opencode/<tag>/opencode`, unless the operator supplies
`--opencode-binary` or `ZOE_OPENCODE_BINARY`; the managed path is recorded in the
runner environment and rendered into the systemd unit.

## Version caveat

Directory, permission, and completion behavior was checked against local source `1aece25b7e` (process-daemon-v0.3.3-37, tags through v2.2.1) and the v2 API/OpenAPI documentation. Production is v2.2.5; the deployment task must recheck these endpoints and permission semantics against that binary before rollout.

## Consequences

Host permissions, filesystem ownership, and OpenCode configuration replace container isolation. Zoe's safety floors and control-plane validation are unchanged. A service restart may preserve persisted sessions but never authorizes replay of an uncertain create.
