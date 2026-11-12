# Deployment operations

This runbook describes the local Linux installation and the macOS client on
`zhuwei@macair`. For the distinction between documentation and acceptance, see
the [evidence register](../acceptance/evidence.md).

When operating voice routing, use the register configuration below and [ADR 0013](../architecture/decisions/0013-context-register.md) for main. Historical triage, alias-roster Steward, held-task controls and fallback descriptions are **deployed on `bb07671`, removed on main**; never apply those historical commands or env keys to a new installation. Replacement deployment and live acceptance remain unverified.

For client error investigation, authenticated diagnostic uploads, bounded
SQLite inspection, and voice pool isolation checks, see the
[client diagnostics runbook](diagnostics.md).

## Linux services and storage

### Voice delegation instruction

When creating a voice session, `ZOE_VOICE_INSTRUCTION_TEXT` optionally overrides
the built-in Chinese delegation rule (valid UTF-8, at most 6000 bytes). An unset
or empty value uses the default, which states identity and tone and the split of
work: actions and tasks go to the backend, upstream answers directly only what
needs no information, does not claim it cannot do things, treats backend results
as authoritative, and does not state a task stopped or finished unless told. It
does not carry the cancel, progress, and personal-question delegation rule; that
rule is sent as the first `initial_items` entry (see below). Literal `off` omits `instructions`.
Use maintenance menu item **21) Configure voice instruction** to view the effective
default, custom text or disabled state and save a single-line replacement; empty
input removes the key to restore the default. The atomic stack-env writer escapes
quotes, newlines and dollar signs for literal Compose parsing. Apply saved changes
with `zoe update`, which preserves custom values. For every gateway create-session
request, including reconnects, Zoe sends the resolved rule verbatim in the top-level
`instructions` field alongside `opening_context`. The gateway writes it unchanged
into AVAS `session.instructions`. No prefix, post-create context push, or connected
retry is used. Migration 215 and its historical delivery rows remain intact; this
path no longer writes separate delivery rows. Successful transport does not prove
silence or compliance: whether upstream obeys must be verified in live calls.

On the AVAS route the same request also carries `opening_context` items, which the
gateway forwards as upstream `initial_items`: first the fixed delegation rule
(cancel, progress, and personal questions are delegated), then at most five
shallow memory facts and the current tasks. The rule is not configurable by
`ZOE_VOICE_INSTRUCTION_TEXT`. When inspecting delivery in `voice_sessions.opening_context_delivery`:
`delivered` means the gateway acknowledged `applied`; `unverified` means it did not
acknowledge or reported `silent: "unverified"`; `unsupported` means nothing was
sent or the gateway said it is unsupported. A route mismatch (see
`ZOE_VOICE_UPSTREAM_ROUTE` below) leaves the AVAS items unsent. Zoe never pushes
opening context after creation on AVAS.
The register-based source and mid-call additions in [ADR 0013](../architecture/decisions/0013-context-register.md)
are **Accepted, not implemented**; they do not change these deployed instructions.
The gateway must return `opening_context: {"applied": true}` (with `silent`
omitted, null, or true) for `delivered`; `silent: "unverified"` remains unverified.
Return top-level `context_chars` to report the gateway's applied character count;
an omitted count decodes as zero. These fields describe opening context, not the
separate `instructions` system prompt.

### Register routing configuration

When deploying the context-register wiring, independently configure routing with
`ZOE_REGISTER_{BASE_URL,MODEL,API_KEY_FILE,REASONING_EFFORT,THINKING,TIMEOUT_MS}`
and compaction with `ZOE_COMPACT_{BASE_URL,MODEL,API_KEY_FILE,REASONING_EFFORT,THINKING,TIMEOUT_MS}`.
Both groups are optional at startup. The register is enabled only when each
prefix has `BASE_URL`, `MODEL`, and `API_KEY_FILE`. If either group is unset or
partial, startup succeeds with the register disabled and one
`register status=disabled reason=not_configured missing=<comma-separated variable names>`
log line. Every delegated sentence then creates no task and holds the fixed
processing-error speech; there is no Steward or other fallback. No register
compaction runs or register opening context is included; recall uses memory alone.
Configure both groups using the `zoe` menu items **Register model** and
**Compact model**, then redeploy or restart. A complete group's unreadable or
empty key file still fails startup, naming the variable without exposing its
content, even if the other group is incomplete. Compose accepts missing host
variables and uses `/dev/null` for unset independent secret files. Effort and
thinking default to empty. Routing timeout defaults to 30000 ms (500-60000);
compaction timeout defaults to 600000 ms (5000-600000). Both routing calls share
the existing 35-second classification budget.

When tuning compaction, `ZOE_COMPACT_MAX_CHARACTERS` defaults to 8000 (500-100000),
`ZOE_COMPACT_INTERVAL_MINUTES` defaults to 60 (1-1440), and
`ZOE_REGISTER_RAW_WINDOW_MINUTES` defaults to 60 (5-1440). Invalid values fail
startup with the variable name. The raw-window width sets the compact right edge;
raw starts at the retained compact edge even after a failed rebuild.

For this wiring, register failures and Steward failures create no task and hold
the fixed processing-error speech. Memory writes are staged silently once per
committed turn; recall retains its holding and answer delivery. Task reports use
Steward text; active attachments append to the same task without stopping it.
No high-impact confirmation hold or confirm/decline routing is performed.

### Deployment layout

- `/opt/zoe/releases/current` selects the immutable server/runner release.
- `zoe-compose.service` supervises the server, memory worker, and Qdrant.
- `zoe-runner.service` runs the trusted execution runner.
- `zoe-opencode.service` runs the Zoe-managed OpenCode server as root,
  bound to `127.0.0.1:4096`, with private state under `/var/lib/zoe/opencode`.
- `zoe-macair-tunnel.service` maintains private SSH forwarding to the Mac.
- `/etc/zoe` contains deployment configuration and secret references.
- `/var/lib/zoe` contains authority databases and persistent service data.

When discovering a deployment, stack mode and runner supervision are independent:
a Compose stack can have systemd-managed runner, OpenCode, and Compose units.
The following precedence applies to stack discovery (highest first).

| Priority | Evidence | Result |
| --- | --- | --- |
| 1 | Explicit `ZOE_DEPLOYMENT_MODE=systemd` or `compose` | Override stack mode; retain runner supervision metadata. |
| 2 | Existing regular Compose file selected by environment, stack env, then persisted path | Compose stack, regardless of systemd supervision or persisted mode. |
| 3 | Loaded runner unit or runner environment file, without a Compose file | Systemd stack. |
| 4 | No stack evidence; `/run/systemd/system` exists | Systemd fallback; otherwise unknown. |
| 5 | Persisted descriptor | Path defaults only; stale mode never overrides discovery. |

Discovery self-heals the resolved descriptor without writing during inspection;
the next successful provider configuration persists the corrected descriptor.
Systemd unit status and runner/provider restarts always use systemctl, including
on Compose hosts. Memory configuration uses Compose; Steward and recall save
their model settings in the stack environment without a mode gate.

The installer creates the external Docker network `zoe` when it is missing. The
existing remote CLIProxyAPI deployment remains outside Zoe's lifecycle.

Voice account-pool behavior and the fail-closed media boundary are defined in
[ADR 0005](../architecture/decisions/0005-voice-task-and-lease-boundaries.md).
`ZOE_VOICE_ACCOUNT_MEDIA_ENABLED=true` pairs with the gateway's
`VOICE_RELAY_ENABLED=true`; set it with `zoe voice configure --enable-account-media`.
If the gateway runs client-direct, keep it `false`.

`ZOE_VOICE_HEARTBEAT_TIMEOUT_SECONDS` (default 60, clamped to 30–600) controls
server-side voice reaping: active sessions with no recent heartbeat are reclaimed
through the normal release flow; unverified termination remains quarantined and
keeps the voice pool blocked.
`ZOE_VOICE_UPSTREAM_ROUTE` must match the gateway's `VOICE_UPSTREAM_ROUTE`. The Linux compose file passes it to the server with default `avas`; the server binary itself falls back to `wm` when the variable is absent. If they differ, Zoe builds the wrong opening context (on `wm` it calls opening-context on an AVAS gateway, receives 501, and records `unsupported`); the call still works but the AVAS `initial_items` are not sent.

Check service status without printing environment files or secret values:

```bash
systemctl is-active zoe-compose zoe-runner zoe-macair-tunnel
 curl --fail 'http://127.0.0.1:${ZOE_SERVER_PORT:-8787}/healthz'
 curl --fail 'http://127.0.0.1:${ZOE_SERVER_PORT:-8787}/readyz'
```

The previously recorded installation binds port 8787 on both host loopback and the authorized
LAN interface. For deployments exposing the LAN interface, the address is deployment-specific
and must be supplied as the numeric `ZOE_SERVER_LAN_ADDRESS` value in the host environment.
An address change while the service is already running requires restarting the Zoe service to
republish it.
The deployment override `/etc/zoe/control.lan.compose.yaml` adds this binding;
`/etc/systemd/system/zoe-compose.service.d/lan.conf` includes the override in
the service's Compose commands. The source template is
`deploy/linux/docker-compose.lan.yml`; set `ZOE_SERVER_LAN_ADDRESS` to the
host LAN address. Update that setting if the host address changes.
OpenCode runs in the dedicated host-installed service. The runner uses its authenticated local HTTP API; health/readiness responses do not prove model execution, memory processing, or task completion. Set `ZOE_MAX_RUNS` on the server to an integer from 1 through 64 (default 12); it counts active, dispatching, waiting-user, and uncertain runs. Set `ZOE_WAIT_LIMIT_MINUTES` to an integer from 1 through 1440 (default 30) to bound retained Worker-question and permission-request sessions.

## Installation and configuration

### Resident Worker permissions

For resident Worker runs, the stable profile and agent ID `zoe-steward` selects
the `full` tier. Its generated agent configuration and session permissions allow
all actions and resources without approval prompts, including edits, V2 `shell`
(legacy `bash`), web fetches, external directories, Code Mode, and every MCP
tool. MCP checks use `<server>_<tool>` actions (for example
`scrapling_browser_evaluate`), not the displayed `tools.scrapling.*` name or a
single `mcp` gate; the final `*` allow covers all current and future servers.
Only this profile with the resident Worker identity may select `full`; task
scopes cannot promote other profiles to it. `zoe-worker` and `zoe-super` retain
`external_paths`, and the rotation summarizer retains deny-all.

For nonresident profiles, `read_only_research` permits reads, HTTP/browser
research and MCP resource inspection; `workspace_read_write` additionally permits
edits, shell, subagents, browser actions/evaluation and state persistence.
`external_paths` additionally permits only explicitly granted external
directories. All three retain deny-all for unknown tools and MCP servers.

For each run, the runner uses `ZOE_OPENCODE_WORKSPACE_ROOT/<run-id>` when configured,
otherwise the claim's workspace, as the OpenCode Location. Paths outside that
Location and its project worktree require `external_directory` as well as read
or edit permission. Scrapling is reachable only when configured in remote mode
with a working endpoint/token reference, or local mode with its installed binary;
permission allows do not install or connect MCP servers.

For the shipped Linux unit, no additional sandbox relaxation is necessary:
OpenCode already runs as root without `ProtectSystem`, `ReadWritePaths`,
`ProtectHome`, or `NoNewPrivileges`. Consequently the full Worker can read/write
root-accessible host files, execute shell processes, use host networking, and
reach `/etc/zoe/secrets`, the Zoe database, and the Docker socket if present.
This change adds no unit access grants and does not isolate those resources;
filesystem permissions, mandatory OS policies, and remote MCP authorization
still apply. Treat this tier as host-root authority, not a workspace sandbox.

Runs rotated by releases before this fix are rebound on recovery from the journal chain.

Use the [Linux deployment templates](../../deploy/linux/) and [release workflow](../../.github/workflows/server-release.yml) for the server/runner release. `server-release-<full-sha>` contains `server-linux-amd64.tar.gz`; extract it before deployment. Its real, executable `bin/zoe-server`, `bin/zoe-runner`, `bin/zoe-secret-entrypoint`, and `bin/zoe` are produced directly by the workflow's Go builds, not host-created symlinks. The tar preserves executable modes across artifact download. `SHA256SUMS` and `release-manifest.json` record these `bin/` paths; `deploy-linux/` contains Compose and systemd templates. The embedded CLI source revision must match `SOURCE_REVISION`; do not compile on the control host. The same run publishes the matching [operator CLI archive](#operator-release-installation).

The operator release installation procedure below installs only the maintenance CLI. It does not install server images or configure a complete deployment. Run `zoe status` before `zoe setup`, `zoe provider configure`, `zoe provider test`, `zoe memory configure`, or `zoe memory test`; these operations are also available from the grouped interactive menu. The memory model key is a separate credential issued by the model gateway and entered through `zoe`; Zoe never creates it. For an already deployed external voice gateway, use `zoe voice configure --url HTTPS_URL --token-file PRIVATE_KEY_FILE`, then `zoe voice test` and `zoe voice status`. This writes only the Zoe-side endpoint and secret reference; it does not deploy or modify the gateway. Follow [the CLI implementation](../../cmd/zoe/) for current flags and [deployment configuration](../../internal/ops/deployment_config.go) for detection and rollback behavior.

### Complete Linux deployment order

For a new host only, obtain and checksum-verify the reviewed server and CLI
archives described in [operator release installation](#operator-release-installation)
on the host, extract them into private staging directories, then run:

```sh
sudo bash /path/to/operator/deploy-linux/install-cli.sh /path/to/zoe-linux-amd64.tar.gz
sudo zoe install [--start] --release /path/to/server-release
```

   The bootstrap verifies `SHA256SUMS`, installs the CLI atomically, and keeps prior
   CLI releases for rollback. `zoe install` validates the release and host before
    writing files, verifies the host-installed OpenCode binary, stages the release, pins images by digest, pulls them, installs
   the systemd units, preserves existing tokens and operator environment keys, and creates
    deployment-owned runner tokens only when absent. It creates the OpenCode password once with mode 0600 and never prints or regenerates it. It does not create an instance
   key or a model gateway key. It leaves services stopped by default; `--start`
    enables OpenCode, waits for authenticated `GET /api/info`, then starts the runner and waits for the health probe. It prints the next steps in
   order: `zoe key init`, `zoe memory configure`, then `zoe provider configure` for
     systemd/runner deployments. Unless overridden with `--opencode-binary` or `ZOE_OPENCODE_BINARY`, install and update download and checksum-verify the pinned OpenCode asset into `/opt/zoe/opencode/<tag>/opencode`. Until key init, `zoe status` shows
   `Instance key: pending (run zoe key init)`. Provider and memory keys come from
   the model gateway and are entered through `zoe`; they are never generated here.

After the stack starts, steps 1 and 2 may be done in either order; step 3 comes last:

1. Run `zoe key init --database PATH --server HTTPS_URL --output PRIVATE_JSON`
   (or `zoe key rotate` for an existing instance). The operator keeps the private
   JSON output for client connection. The server uses the credential hash stored
   in its database; the installer must not create, inject, or persist a plaintext
   instance-key startup secret. When root creates the database before the Compose
   service has, set ownership of the database and any existing `-wal` and `-shm`
   sidecars to `65532:65532`, retaining mode `0600`. Do not recursively change
   the directory's private JSON exports.
2. Configure the memory model with `zoe memory configure`, then configure the
   task model with `zoe provider configure` for systemd/runner deployments.
3. Verify service state, `/healthz`, and `/readyz`. `/healthz` establishes process
   liveness; `/readyz` is `200 {"status":"ready"}` only after the instance-auth
   row exists and otherwise remains `503` pending. These checks establish
   installation and process readiness; model execution, memory processing, and
end-to-end task completion require a separate acceptance run.

### Routing deadlines and model wire settings

When routing committed voice turns, `ZOE_VOICE_CLASSIFICATION_TIMEOUT_MS`
(default 35000 ms, range 100-60000) bounds register plus Steward routing and the
handoff wait. `ZOE_STEWARD_TIMEOUT_MS` bounds the Steward decision, including
API-style negotiation (default 30000 ms, range 200-60000). Both models share the
total budget, not additive guarantees. Steward's operator probe invokes `Choose`.
Register or Steward failure creates no task and sends fixed processing-error speech.

When configuring the answer client's thinking and reasoning effort, use the wire
settings below. Effort accepts `none`, `low`, `medium`, `high`, `max`, or empty;
`minimal` is rejected by the configured OpenAI-style gateway. Values are sent
unchanged, not mapped locally.

| Thinking value | `/responses` | `/chat/completions` |
| --- | --- | --- |
| `disabled` | Sends `{"reasoning":{"effort":"none"}}` for every model, overriding configured effort. | Sends `{"thinking":{"type":"disabled"}}`; separately sends top-level `reasoning_effort` when configured. |
| `enabled` | Sends `{"reasoning":{"effort":"<configured effort>"}}` for every model when effort is set; otherwise omits `reasoning` and uses the provider default. | Sends `{"thinking":{"type":"enabled"}}`; separately sends top-level `reasoning_effort` when configured. |
| Empty | Keeps existing model detection: names starting with `gpt-6`, `gpt-5.6`, `o1`, or `o3` send configured effort, or `low` when empty; other models omit `reasoning`. | Omits `thinking`; sends top-level `reasoning_effort` only when configured. |

Responses requests never send the chat `thinking` field. Explicit thinking settings suppress `temperature` on Responses; otherwise existing model-based temperature behavior remains unchanged. Chat requests retain `temperature: 0`. An explicit effort of `none` is still sent unchanged with `enabled`, so avoid that combination when enabling thinking is intended.

[DeepSeek thinking mode](https://api-docs.deepseek.com/guides/thinking_mode) defaults to enabled with effort `high`; its Responses `none` disables thinking. DeepSeek maps `minimal` to `low`, `medium` and `xhigh` to `high`, and supports `max`; Zoe accepts `medium` but rejects `minimal` and `xhigh` for gateway compatibility. See its [Responses API guide](https://api-docs.deepseek.com/guides/responses_api). Memory effort validation uses the same accepted values; its existing empty-effort fallback remains `medium`.

### Call-recall model endpoints

For memory and conversation recall, configure the Memory answer endpoint through the Models
menu or `zoe answer configure`. It accepts `--base-url`,
`--model`, `--effort` (empty, none, low, medium, high, max), `--thinking` (empty, enabled,
disabled), `--key-stdin`, and `--disable`. Keys are written atomically with mode
0600 and server ownership 65532:65532 to `/etc/zoe/secrets/answer-api-key`.
An empty base URL disables the endpoint; updates preserve its settings.
Environment names and secret references are in the [stack example](../../deploy/linux/zoe-stack.env.example).
The answer timeout defaults to 8000 ms (range 500–30000). The client uses Responses first and remembers a
Chat Completions fallback after HTTP 404, 405, or 501. The thinking/effort wire
contract is the same as the wire table above, including temperature suppression.
Existing keys must be readable by the server user before saving or probing;
status and update warn about unreadable configured keys, and update stops before
service changes. `zoe answer test` invokes the runtime
client with synthetic evidence and reports PASS/FAIL, API style, and latency;
`zoe status` reports configuration without credential contents. Apply with `zoe update`.

When the register routes recall, the answer client receives owner-scoped memory items
and bounded register conversation context (compact plus raw recent turns) in one model
call. Without a register it uses memory alone. The call-summary pipeline and its
Configure/Test commands are removed.
Synthetic probes do not establish live-provider quality, latency, deployment, or
end-to-end acceptance.

The complete host installation therefore includes the Zoe-owned OpenCode runtime image,
Zoe server image, trusted runner, memory worker image, Qdrant service, Docker,
and the three Zoe systemd services. An external model gateway is not installed
by this procedure. Its URL, selected model, and API-key file are deployment
configuration applied to the OpenCode runtime and memory worker after the host components
are installed; leaving them unset keeps the daemons available but prevents
model-backed task execution.

### Upgrade and removal

For every server upgrade, use the [host deployment transaction](#host-deployment-transaction).
The following describes the existing CLI lifecycle implementation for diagnosis,
not an alternative standard deployment procedure. `zoe update --release /path/to/release` stages and pulls
the new release, switch `current`, restart Compose and the runner, and verify
health; after health succeeds it also atomically switches the host CLI to the
release's immutable `bin/zoe` when present. A release without `bin/zoe` prints
`CLI not updated: release has no bin/zoe` and does not fail the server update.
If CLI switching fails, the healthy server remains updated, the old CLI remains
in place, and the command exits non-zero. It also prepares missing OpenCode state and credentials before rendering
the units. Update refreshes release-owned environment keys, including image
references and paths derived from the configured roots, while preserving
operator-owned keys and unknown lines. A failed update restores the prior
selector and environment files byte-for-byte and reports the failing command's
bounded output; data and secrets are not touched.
The managed OpenCode path is versioned; updates retain the current and immediately previous versions and prune older ones. Plain server/runner upgrades do not restart `zoe-opencode.service`. It is
restarted or started when its unit, generated configuration, or discovered binary
version changes and no active OpenCode sessions exist; otherwise the restart is
skipped and reported.
Use `zoe uninstall` for reviewed removal. It
first prints the exact Compose-project containers, `io.zoe.managed=true` runtime
containers, and local IDs for the four Zoe image repositories. It removes the
Compose project with `docker compose down --remove-orphans`, removes only those
queried IDs, keeps the shared Qdrant image, and never runs Docker prune. It
requires typing `yes` unless `--yes` is supplied; non-interactive use must supply
    `--yes`. It stops and disables `zoe-opencode.service`, removes its Zoe-owned unit,
    configuration, and `/var/lib/zoe/opencode`, and never touches `/root/.opencode`
    or `/root/.config/opencode`. By default it keeps `/var/lib/zoe` and all `/etc/zoe`. Add `--purge-data`
to remove data and Zoe-generated configuration/tokens while explicitly keeping
the three gateway credentials; add `--purge-credentials` to remove those too.
It removes `/opt/zoe`, including the installed CLI and all releases, only after
service, container, image, network, and systemd cleanup. If a step fails it
reports completed work and leaves the release root for recovery; rerun
`zoe uninstall` after fixing the failed step. A later install then needs a fresh
operator package.

## Server and worker settings

The Linux control server reads deployment settings from its process environment. Follow the repository's [credential handling rule](../../AGENTS.md) for deployment-managed secrets; the server does not print them. Bind to loopback or a private trusted interface and put external TLS termination behind the deployment's authenticated network boundary.

Configure `ZOE_OPENCODE_BASE_URL`, `ZOE_OPENCODE_PASSWORD_FILE`, `ZOE_OPENCODE_MODEL`, and `ZOE_OPENCODE_WORKSPACE_ROOT` in the runner environment. The separate host service owns its OpenCode configuration and provider secret. CLIProxyAPI remains external and unchanged. The service deliberately does not use `ProtectSystem` or `ProtectHome`, because its configured user must be able to write OpenCode state and authorized workspaces.

For a reconcile transient unit, provide the OpenCode password through the systemd
credential file rather than the unit environment:
`ZOE_OPENCODE_PASSWORD_FILE=/run/credentials/<unit>.service/opencode-password`.
The credential is private to that unit and must not be copied into logs, source,
or persistent deployment metadata.

### Configure the OpenCode runtime

### Scrapling web access

Workers may use `webfetch` and, when enabled, only tools from the `scrapling` MCP server. `websearch` is disabled in every permission tier because this deployment has no search provider.

Set `ZOE_SCRAPLING_MODE` to `off` (default), `remote`, or `local`. If the mode is unset and `ZOE_SCRAPLING_URL` is present, remote is selected; local never uses a URL. Remote URLs must be `http` or `https`, include a host, and contain no credentials. Set `ZOE_SCRAPLING_TOKEN_FILE` to the root-readable, non-world-readable token file (default `/etc/zoe/runner-secrets/scrapling-mcp-token`). OpenCode receives a `{file:path}` reference, not the token value. The operator selected token-only authentication over plain HTTP, so protect the network path and rotate the shared token by restarting Scrapling.

For local mode, `zoe install`/`zoe update` may install pinned Scrapling `0.5` with the `ai` extra under `/opt/zoe/scrapling/0.5/venv`; browser dependencies are installed with `scrapling install`. Scrapling 0.5 exposes no documented configurable browser/session concurrency limit, so there is no `ZOE_SCRAPLING_MAX_BROWSERS` setting; capacity remains Scrapling's own behavior. A failed optional local install is warned, omitted from generated MCP configuration, and does not fail the Zoe update; status reports the omission.

For a remote host, copy the release package and run `sudo bash deploy/scrapling/install-remote.sh install`; the script installs `python3-venv` when absent, provisions `/opt/scrapling/0.5/venv`, verifies the pinned Scrapling wheel before installation, installs browser dependencies, writes `/etc/scrapling/env` mode 0600, and enables `scrapling-mcp.service` on port 8000 with `MemoryMax=900M`. The Scrapling wheel is checksum-pinned, but transitive dependencies are intentionally version-unpinned because no generated hash-pinned lock is committed; review the resolved dependency set when upgrading the pin. Use `status` for service checks and `print-token --yes` only when explicitly copying the secret to Zoe; never put it in logs or source control. The operator's selected endpoint may therefore be `http://HOST:8000/mcp`.

The runner creates one OpenCode session per run and rotates when the latest assistant message reports input + output + cache read + cache write tokens above `limit.context * ZOE_AGENT_CONTEXT_WINDOW_RATIO`, where the window comes from OpenCode `GET /api/model` for the selected `providerID/modelID`. The ratio defaults to `0.5` and must be in `(0,1)`. If the model is missing, has no limit, or the request fails, the runner does not rotate. `ZOE_AGENT_CONTEXT_TOKEN_BUDGET` is an optional absolute cap (unset/0 means no cap); a claim's `MaxContextTokens` can only lower the threshold. `ZOE_AGENT_MAX_ROTATIONS` defaults to 3. If the first measured assistant usage in a fresh session is at or above the threshold, it ends with `context_threshold_below_baseline`; it does not rotate repeatedly. Optional `ZOE_AGENT_ROTATION_SUMMARY=on` requests a separate all-deny summarizer session; it defaults to off and failures fall back to deterministic context. Runtime profiles select the OpenCode provider/model route; clients cannot override execution routes or grants.

The workspace path must be absolute, private, and available to the runner-created container.



A missing or invalid workspace fails closed even when server and runner health checks pass. Do not share a workspace across concurrent runs without an explicit ownership grant.

Required settings:

- `ZOE_DATABASE_PATH`: path to the SQLite database. The server acquires a single-writer process lock before opening it.
- The operator initializes or rotates the instance credential with `zoe key init --database PATH --server https://... [--output PRIVATE_JSON]` or `zoe key rotate ...`. The command displays the key and installation link once (and renders a terminal QR using its built-in encoder). The deployment must not generate, inject, or persist a plaintext startup credential; the server authenticates against the database hash. `ZOE_BOOTSTRAP_CODE` is removed; use a fresh database for a new deployment. An ordinary service restart or container recreation does not generate or rotate an instance key. After an explicit `key rotate`, import the newly exported private key into every client before reconnecting; clients retaining the prior key receive an authentication failure.

Optional settings:

- `ZOE_LISTEN_ADDR`: defaults to `127.0.0.1:8787`.
- `ZOE_DEFAULT_PROFILE_ID`: the server-selected profile for new tasks.
- `ZOE_RUNTIME_PROFILES`: JSON object keyed by profile ID. Install writes a working single `worker` profile by default and never overwrites an existing non-empty value. Each profile sets `id`, `runtime_route_id`, `model_route_id`, `role`, `isolation_policy`, `capability_ids`, and `scopes`. Client requests cannot override these execution routes or grants.
- `ZOE_RUNNER_TOKEN`: deployment-provisioned 32-byte base64url runner credential. When set, `ZOE_RUNNER_ID`, `ZOE_RUNNER_WORKSPACES`, and `ZOE_RUNNER_SCOPES` default to `configured-runner`, `default`, and `runner:claim,runner:observe,runner:control:read`.
- `ZOE_MEMORY_RUNNER_TOKEN_FILE`: optional path to a deployment-managed file containing a distinct 32-byte base64url memory worker credential. When enabled, `ZOE_MEMORY_RUNNER_WORKSPACES` must list exact workspace IDs; `ZOE_MEMORY_RUNNER_ID` defaults to `memory-worker`; and `ZOE_MEMORY_RUNNER_SCOPES` may only contain exactly `memory:jobs:claim,memory:jobs:complete,memory:propose`. The server stores only the credential hash and never logs the token. This credential exposes the durable derived-job API; it does not enable a Mem0 or Qdrant runtime.
- Memory-worker deployment also requires `ZOE_CONTROL_URL`, the scoped worker credential injected as `ZOE_RUNNER_TOKEN` (provisioned for the server through `ZOE_MEMORY_RUNNER_TOKEN_FILE`), `ZOE_MEM0_LLM_MODEL`, `ZOE_MEM0_LLM_BASE_URL`, and `ZOE_MEM0_LLM_API_KEY` from deployment-managed secret references. Configure the three Mem0 values with `zoe memory configure`; its secret path must remain distinct from the task-model credential. The memory model key is a separate credential issued by the model gateway and entered through `zoe`, never created by Zoe. `ZOE_MEM0_LLM_PROVIDER` must use the accounted `openai` path; `ollama` is rejected until it supports the same accounting boundary; `ZOE_MEM0_HISTORY_DB` and the model cache belong on the private persistent memory volume. Qdrant URL, optional API-key reference, collection/codec, and worker batch/poll settings are deployment configuration. The pinned multilingual embedding revision and dimensions are defined in [worker configuration](../../services/memory/config.py); changing them requires a separate collection and rebuild.
- For local execution, start the worker from the repository root with `python -m services.memory.worker`, or deploy reviewed CI-built worker images with the settings above. The Compose Qdrant service is loopback-bound and disposable; it is not a production persistence configuration. These commands describe source setup and do not establish production installation or recovery.
- `ZOE_INSTANCE_SCOPES`: comma-separated fixed-owner grants. An explicit value replaces the defaults exactly; it never adds a wildcard or restores omitted grants. Device approval/revocation scopes are not part of the client model. Project/task memory scopes must name their exact scope IDs.

## Fresh-instance operator commands

Stop the server before `zoe key init --database PATH --output PRIVATE_JSON --server URL` or `zoe key rotate --database PATH --output NEW_PRIVATE_JSON --server URL`. Output files must be new private destinations. For legacy deployments, remove instance-key startup variables, secret-entrypoint mappings, and instance-key mounts before maintenance; leave runner and worker credentials in place. The canonical server restarts against its database hash after rotation and requires no plaintext key synchronization. Keep generated keys and installation links out of logs and chat.

Data export and destruction procedures are in [data lifecycle operations](data-lifecycle.md).

## Zoe runtime uncertainty reconciliation

Unknown create outcomes keep their execution slots and must never be retried as
new OpenCode container creates. The operator command changes authority only through the normal
runner observation API; it does not delete journal rows, task/run history, or
container records.

Use a runner built from the same reviewed source as the control server. Stop `zoe-runner` to hold the deployment and journal locks, then inspect Docker by the exact labels `io.zoe.managed=true`, `io.zoe.runtime=opencode`, `io.zoe.run`, `io.zoe.attempt`, and `io.zoe.fence`. Compare two complete, stable inventories. A matching container ID is preserved and reconciled; no match is a no-effect proof only when the complete inventory is authoritative. Any ambiguity, started container, missing identity, or incomplete inventory remains `effect_unknown`; never replay the create.

An uncertain run is not evidence of either success or absence. Reconcile only a
current run after recording two complete, stable, authoritative inspections;
historical uncertain runs are not recovered from old records and no migration is
required for them. Restart the runner only after any current inspection is
recorded.

For the authorized host-removal procedure for retired AO components, see [AO host removal](ao-host-removal.md). It is a separate runbook and has not been executed.

## Voice gateway

The separately deployed gateway is the Zoe-compatible fork
[ronigooja/chatgpt-web-voice](https://github.com/ronigooja/chatgpt-web-voice).
Pin `VOICE_GATEWAY_REVISION` to an immutable fork commit for production.
See [the deployment instructions](../../deploy/voice/README.md).

- The management URL is deployment-specific; do not assume a host name or
  LAN address from this document. Keep management access on an operator-managed
  trusted path.
- Source: `/opt/zoe/voice/upstream`.
- Container: `zoe-voice-gateway`.
- Persistent data: `/var/lib/zoe/voice`.
- Compose configuration: `/etc/zoe/voice.compose.yaml`.
- Private environment: `/etc/zoe/voice.env`.
- Management login reference: `/etc/zoe/voice-admin.txt`, on Linux only.

The user must manually import a ChatGPT Web account in the gateway management
UI. Deploying the gateway does not establish voice functionality in Zoe:
trusted HTTPS, client media permissions, and voice acceptance remain required.
Account media is part of the current baseline (see
[ADR 0005](../architecture/decisions/0005-voice-task-and-lease-boundaries.md)).

## Operator CLI deployment metadata

The `zoe` operator CLI reads deployment metadata without opening secret files.
Its status view reports the detected systemd or Compose mode, service states,
provider endpoint and model, the source of provider configuration, the last
provider check category, and the secret file mode. Token contents, instance
keys, and complete installation links are never included in status output.

The canonical OpenCode provider variables are `ZOE_OPENCODE_PROVIDER_BASE_URL`, `ZOE_OPENCODE_PROVIDER_KEY_FILE`, and `ZOE_OPENCODE_MODEL`; the runtime image and bridge use `ZOE_OPENCODE_IMAGE` and `ZOE_OPENCODE_PROVIDER_SOCKET`. The default key file is `/etc/zoe/runner-secrets/cliproxyapi-api-key` with mode `0600`. Provider credentials never enter the OpenCode container environment. The memory model key file `/etc/zoe/secrets/mem0-llm-api-key` is owned by `65532:65532`, like the other Compose secrets.


Service reloads are performed by the CLI through the detected deployment
manager. For systemd it runs `daemon-reload` followed by
`reload-or-restart` for the affected units. For Compose it runs the configured
project with `docker compose up -d --no-build`. A failed command is returned to
the caller so setup can retain the previous configuration and report a
rollback-safe failure.

Run `zoe` without arguments for the interactive operator menu. The key
initialization and rotation entries ask for the server URL, then display the
key and `zoe://connect` installation link once. The built-in QR encoder
renders the same link in the terminal for App scanning; no `qrencode` host
dependency is required. Keep this terminal private because the QR grants access. Status never
prints keys, tokens, or installation links.

## Operator release installation

When deploying a server release, use the successful **Server deployment artifact**
run for the reviewed full SHA, not the optional `operator-artifacts` job in normal
Linux CI. That server run always builds both `server-release-<sha>` and
`operator-cli-<sha>` for Linux `amd64`. The sibling CLI artifact contains
`zoe-linux-amd64.tar.gz` with `bin/zoe`, `deploy-linux/install-cli.sh`,
`SOURCE_REVISION`, and `SHA256SUMS`. It is the same binary included in the server
release. The Linux workflow's optional release job still produces standalone
`operator-linux-<arch>-<revision>` archives for `amd64` and `arm64`; these are not
a substitute for matching the server release SHA.

### Host deployment transaction

For upgrades to an existing Zoe host, [deploy/linux/deploy.sh](../../deploy/linux/deploy.sh)
is the standard procedure, not a sequence of hand-applied selector changes.
Run a reviewed copy of that directory (including its Python helper, candidate
validator and expected-env file) **on the Zoe host as root**. The voice gateway
and other gateway deployment scripts are separate; this script never deploys
them or changes CLIProxyAPI. New hosts still require [bootstrap installation](#complete-linux-deployment-order).

```bash
bash deploy/linux/deploy.sh --sha FULL_40_CHARACTER_SHA --dry-run
nohup bash deploy/linux/deploy.sh --sha FULL_40_CHARACTER_SHA --run-id SERVER_RELEASE_RUN_ID < /dev/null > /var/log/zoe-deploy-launch.log 2>&1 &
# Optional: --cli-run-id ID selects a matching successful server-release run.
bash deploy/linux/deploy.sh --rollback --sha PREVIOUS_FULL_SHA
```

Without `--run-id`, the script selects a successful `server-release.yml` run
whose head SHA matches exactly, or aborts. It downloads both artifacts directly
using host `gh`, with no workstation relay. Artifacts expire after seven days.
Root must already have authenticated repository/GHCR access, Docker Compose v2,
Python 3.10+, GNU coreutils, `flock`, systemd and `curl`. The existing deployment
must be Linux amd64 with SHA-named immutable releases and a PATH symlink, not a
regular `/usr/local/bin/zoe` executable. No CLI commands, flags or menu entries
are added; the existing `--source-revision` is the noninteractive startup check.

Each phase emits `deploy step=<name> status=ok|fail`. The private append-only
`/var/log/zoe-deploy/<sha>.log` and `<sha>.done` or `<sha>.failed` marker are the
completion authority, not `nohup` launch success. Digests are shortened to 12
characters; environment values and arbitrary service log text are not printed.
The script serializes deployments with a lock. Dry-run downloads and validates
in temporary storage, writes an operational log, and changes no release,
selector, database, environment, CLI, service or completion marker.

Implemented phases:

1. **preflight**: read-only SQLite active/allocating voice-session gate, local
   health, per-namespace migration maxima, voice-turn/task counts, current
   selector and masked server image.
2. **download-verify**: checksums, matching source/embedded CLI revision,
   [candidate validation](#deployment-candidates-and-rollback), installed units
   including drop-ins, required executable/relative Compose paths, undefined
   Compose inputs, and rendered bind/secret/config sources. Missing names are
   reported without secret contents. Existing same-SHA directories must match.
3. **backup**: SQLite backup API plus integrity check, preserving file ownership
   and mode, and a mode-preserving environment backup. Both use `.pre-<12-sha>`;
   existing backups are never overwritten.
4. **images**: pull immutable manifest references; atomically update only changed
   server/memory-worker/runner image lines, preserving other environment lines.
5. **install-cli**: checksum-verified archive through `install-cli.sh`, immutable
   `zoe-cli-<12-sha>` directory, atomic PATH selector and startup revision check.
6. **switch**: atomic `current.new` rename, immediate voice-session recheck,
   Compose restart, bounded health wait (at most 150 seconds), then runner
   restart only when its binary differs.
7. **verify**: all three services active, local/optional public health, running
   image ID and immutable digest, required environment names from
   [expected-env.txt](../../deploy/linux/expected-env.txt), first-three values
   matching rendered Compose, migration/count preservation, runner journal,
   server listening/no panic/fatal/migration lines, and CLI startup.
8. **rollback** on a failed mutation/verification phase: restore environment and
   selectors/CLI; try the old server with a migrated database first. Only if
   that server cannot become healthy is the database backup restored, with Zoe
   writers stopped. The log and final summary explicitly report **DATA LOSS**
   and lost voice-turn/task counts. Manual rollback uses the saved transaction
   selected by the requested previous SHA; failed rollback needs operator
   intervention, not repeated blind deployment.
9. **cleanup**: remove only owned download/temp files; retain backups, transaction
   records, previous server and CLI versions.

Migration continuity is checked within each namespace's observed numeric range.
The known core exclusions are 9 (instance_auth) and unregistered 10; namespace
ranges may start above one. Compatibility with the old server is established by
actually starting it and checking health, not guessed from version numbers.
Completed same-SHA reruns are read-only no-ops. Interrupted transactions require
explicit rollback. After a successful rollback, the same SHA can be deployed
again: the previous state is archived as `.deploy-<sha>.attempt-<epoch>-<pid>`,
and new backups use `.pre-<12-sha>.attempt-<epoch>-<pid>`. Each transaction records
its own backup paths; original backups are never overwritten. The `.failed`
marker remains until the fresh attempt succeeds. Downloads are staged under
`<releases>/<sha>.dl` and removed on exit.

#### Host command compatibility

When verifying or rolling back a host deployment, these command contracts apply:

| Contract | Audit / implementation |
| --- | --- |
| Log cutoffs | One `date +%s` value; `journalctl --since "@<seconds>"` uses the epoch syntax documented in [systemd v219](https://github.com/systemd/systemd/blob/v219/man/systemd.time.xml). [Docker logs](https://docs.docker.com/reference/cli/docker/container/logs/#since) receives bare `<seconds>`, not `@`. Neither depends on timezone, `date -d`, BSD/GNU date parsing, or RFC3339 support in journalctl. No other since/until filters exist in the deployment helper. |
| All services active | Check each unit separately in verify and rollback: [multi-unit is-active](https://www.freedesktop.org/software/systemd/man/latest/systemctl.html#is-active%20PATTERN%E2%80%A6) succeeds if any unit is active. |
| Container selection | [Compose v2 ps -q SERVICE](https://docs.docker.com/reference/cli/docker/compose/ps/) returns IDs of running containers; require exactly one nonempty result. JSON inspection uses the default array, not Go templates or version-sensitive Compose ps JSON shapes. |
| Image identity | Parse `docker inspect` container `Image` and `Config.Env`; compare with `docker image inspect` `Id` plus pinned `RepoDigests`. This assumes registry-pulled images with repository digest metadata, not locally built image tags. |
| Database backup/restore | Python `sqlite3` URI `mode=ro` and [Connection.backup(destination)](https://docs.python.org/3/library/sqlite3.html#sqlite3.Connection.backup) use SQLite's online backup API, not shell sqlite3 flags or live file copies. Integrity checks run on backups; database writers stop before restore. |
| Checksums | GNU `sha256sum -c SHA256SUMS` runs from each extracted root; release generation and fixtures use its [hex-digest, two-space, relative-filename format](https://www.gnu.org/software/coreutils/manual/html_node/cksum-common-options.html). |
| Lock | util-linux [flock -n 9](https://man7.org/linux/man-pages/man1/flock.1.html) locks the already-open descriptor for the script lifetime; use a local filesystem supporting flock, not an unverified network mount. |
| Selector replacement | GNU [mv -T](https://www.gnu.org/software/coreutils/manual/html_node/mv-invocation.html) treats the destination as a path, not a directory. [ln -sfn](https://www.gnu.org/software/coreutils/manual/html_node/ln-invocation.html) replaces a symlink without following it; temporary selectors live beside their final selectors so rename is on the same filesystem. These are Linux/GNU prerequisites, not portable macOS/BSD commands. |

CI fixtures strictly enforce the deployment's epoch timestamp subset and emulate
systemctl's any-active result. They reject the former journal RFC3339 argument
and Docker's unsupported `@epoch`, and test rollback retries/incomplete-state
refusal. Static checks and shims do not establish actual host compatibility.

Host-path overrides for controlled installations/tests are `ZOE_RELEASES_DIR`,
`ZOE_ENV_FILE`, `ZOE_DB_PATH`, `ZOE_API_URL`, `ZOE_UNITS_DIR`, `ZOE_BIN_DIR`,
`ZOE_CURRENT_PREFIX` and `ZOE_DEPLOY_LOG_DIR`. `ZOE_PUBLIC_HEALTH_URL` adds public
health verification. Defaults are `/opt/zoe/releases`, `/etc/zoe/zoe-stack.env`,
`/var/lib/zoe/server/data/zoe.sqlite`, loopback port 8787 `/healthz`,
`/etc/systemd/system`, `/usr/local/bin`, `/opt/zoe/releases/current`, and the log
directory above. Do not pass secret values on the command line or source env
files in a shell. Units, OpenCode and deployment configuration must already be
provisioned; this transaction does not install or change systemd units.

For a complete source-built release, `deploy/linux/build-release.sh RELEASE_TAG`
also installs `bin/zoe` beside the runner in `/opt/zoe/releases/RELEASE_TAG`.
Builds must run in CI or an authorized worker, not on the controller. After
reviewing the release, install its CLI on the standard host PATH with:

```sh
sudo bash /opt/zoe/releases/RELEASE_TAG/deploy-linux/install-cli.sh /opt/zoe/releases/RELEASE_TAG
zoe --source-revision
```

The installer atomically selects the immutable CLI through `/usr/local/bin/zoe`
and refuses to overwrite an existing regular file. It does not start services
or alter external CLIProxyAPI. Run `zoe status` to inspect deployment metadata
before setup or provider changes. The release staging and PATH tests use fake
build tools and temporary paths; Go unit/integration/race checks, actual binary
builds, Apple builds, and physical-device scanning have separate evidence.
For deployment and physical-device evidence, use the [acceptance evidence register](../acceptance/evidence.md).

## Native client installation and connection

Install reviewed CI artifacts with the applicable platform signing/provisioning. Keep Mac inputs, signing/install scripts, artifacts, and logs under `~/Developer/Zoe/runs/<task-id>/`; the installed application belongs at `/Applications/Zoe.app`. A launch/signature check establishes installation only. See [Apple module instructions](../../clients/apple/README.md) and [current evidence](../acceptance/evidence.md) for tested revisions and remaining device/distribution gates.

The current client accepts an operator-selected server URL and shared instance key stored only in Keychain. The exact QR/link interaction is defined in [product](../product.md#3-client-requirements). Use HTTPS across an untrusted network; HTTP exposes the key in transit. Key initialization/rotation requires the server's exclusive database lock; stop it first. Rotation preserves the fixed owner/workspace, tasks, and separate worker credentials. Remove any legacy `ZOE_INSTANCE_KEY` or instance-key secret-map configuration before restart; the canonical Compose deployment does not inject a plaintext owner key. Restart closes old streams. Clearing a client removes its local data, not the shared key or durable tasks.

For export, backup, migration, restore, and destruction, use [data lifecycle operations](data-lifecycle.md).

## Deployment candidates and rollback

A deployment candidate is a reviewed release manifest plus the immutable artifacts
named by that manifest. Generate and inspect its plan before transferring files:

```sh
deploy/linux/deployment-candidate.sh RELEASE_MANIFEST.json \
  --compose deploy/linux/docker-compose.yml \
  --env-file /path/to/reviewed/zoe-stack.env \
  --output deployment-plan.json
```

This command is read-only. It verifies the source revision, architecture, every
artifact SHA-256 and optional image digest, and the rendered Compose configuration.
It prints an explicit order: verify hashes, install OpenCode runtime and the immutable Zoe
release, switch `/opt/zoe/releases/current`, then start and check services. The
plan records the previous immutable release and image digests as rollback targets.
It never contacts a host, changes a selector, pulls an image, or starts a service.

On the host, apply releases through the [host deployment transaction](#host-deployment-transaction),
including its recorded rollback and conditional database restore; do not switch
selectors manually. Keep old immutable release directories and image tags.
Repeat independent readiness, connectivity and task acceptance checks after
the scripted operational checks. Record integrity, installation, readiness, connectivity,
and end-to-end results separately; see the [acceptance evidence register](../acceptance/evidence.md)
for the acceptance boundary. This does not modify the external CLIProxyAPI deployment.
