# Deployment operations

This runbook describes the local Linux installation and the macOS client on
`zhuwei@macair`. For the distinction between documentation and acceptance, see
the [evidence register](../acceptance/evidence.md).

For client error investigation, authenticated diagnostic uploads, bounded
SQLite inspection, and voice pool isolation checks, see the
[client diagnostics runbook](diagnostics.md).

## Linux services and storage

### Voice delegation instruction

When creating a voice session, `ZOE_VOICE_INSTRUCTION_TEXT` optionally overrides
the built-in Chinese delegation rule (valid UTF-8, at most 6000 bytes). An unset
or empty value uses the default: upstream answers greetings, thanks, casual chat
and opinions directly, delegates research, actions, task progress, past-call
recall and personal-memory requests, and waits for Zoe rather than inventing a
result or claiming records are unavailable. Literal `off` disables the push.
Use maintenance menu item **21) Configure voice instruction** to view the effective
default, custom text or disabled state and save a single-line replacement; empty
input removes the key to restore the default. The atomic stack-env writer escapes
quotes, newlines and dollar signs for literal Compose parsing. Apply saved changes
with `zoe update`, which preserves custom values. Zoe sends
the rule with the fixed prefix `Standing rule for you, do not read this aloud or acknowledge it: ` before returning SDP, records
delivery per session/media generation/text hash, and retries a failed send once
on connection without failing call creation. Successful transport does not prove
silence or compliance: whether upstream obeys must be verified in live calls.

### Live-call held-task controls

When deploying spoken task controls, apply voice migration 216 at normal server
startup. `ZOE_VOICE_CONFIRMATION_WINDOW_SECONDS` bounds same-call held questions
(default 600; valid range 1-3600). Configure the existing stateless triage model;
without a classifier, speech does not authorize a held request. `zoe triage test`
can return `progress`, `recall`, `memory_write`, `memory_read`, `task`, or `cancel`; the standalone probe
has no pending question, so `confirm` and `decline` are rejected there.

When validating this route, check a held task and answer in the same live call,
then verify task/interaction state and runner claims. Admission consent uses
`task:admit`, never a runtime tool grant. Cancel is limited to the current owner,
session and generation; running work stops only after runner acknowledgement.
For binding, timeout, retry and scope rules, see
[ADR 0012 held-task controls](../architecture/decisions/0012-voice-conversation-layer.md#held-task-controls).
Local component tests do not establish real-provider or spoken-delivery acceptance.

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
on Compose hosts. Memory configuration uses Compose; triage and recall save
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
`ZOE_VOICE_UPSTREAM_ROUTE` must match the gateway's `VOICE_UPSTREAM_ROUTE` (default `wm`); if they differ, Zoe calls opening-context on an AVAS gateway, receives 501, records `unsupported`, and the call still works but the opening context is not delivered.

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

Use the [Linux deployment templates](../../deploy/linux/) and [release workflow](../../.github/workflows/server-release.yml) for the server/runner release. The reviewed release must contain `bin/zoe`, `bin/zoe-secret-entrypoint`, and `deploy-linux/` with the Compose and systemd files. The embedded CLI source revision must match `SOURCE_REVISION`; do not compile on the control host.

The operator release installation procedure below installs only the maintenance CLI. It does not install server images or configure a complete deployment. Run `zoe status` before `zoe setup`, `zoe provider configure`, `zoe provider test`, `zoe memory configure`, or `zoe memory test`; these operations are also available from the grouped interactive menu. The memory model key is a separate credential issued by the model gateway and entered through `zoe`; Zoe never creates it. For an already deployed external voice gateway, use `zoe voice configure --url HTTPS_URL --token-file PRIVATE_KEY_FILE`, then `zoe voice test` and `zoe voice status`. This writes only the Zoe-side endpoint and secret reference; it does not deploy or modify the gateway. Follow [the CLI implementation](../../cmd/zoe/) for current flags and [deployment configuration](../../internal/ops/deployment_config.go) for detection and rollback behavior.

### Complete Linux deployment order

Copy the reviewed `zoe-linux-<arch>.tar.gz` package to the host, then run the
single bootstrap entry point:

```sh
sudo bash deploy-linux/install-cli.sh /path/to/zoe-linux-<arch>.tar.gz
sudo zoe install [--start] --release /path/to/release
```

   The bootstrap verifies `SHA256SUMS`, installs the CLI atomically, and keeps one
   immutable CLI release. `zoe install` validates the release and host before
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

Use `zoe` with no arguments for the numbered operator menu, or configure endpoints directly with
`zoe memory configure` and `zoe triage configure`. Triage accepts an HTTPS (or local HTTP) base URL; it first tries `/responses` and falls back to `/chat/completions` when that route is unavailable.
Use `--key-stdin` to install a mode-0600 key without placing it in shell history.
Triage is disabled when `ZOE_TRIAGE_BASE_URL` is empty. Apply a saved change with `zoe update`.

For committed voice turns, `ZOE_TRIAGE_TIMEOUT_MS` sets the HTTP timeout (default
3000 ms, range 200–10000). `ZOE_VOICE_CLASSIFICATION_TIMEOUT_MS` bounds total
classification and the handoff wait (default 6000 ms, range 100-30000). One retry
shares the remaining classification budget; there is no separate 1500 ms cutoff.
For task classification, `ZOE_STEWARD_TIMEOUT_MS` bounds the HTTP call and the
whole Steward decision, including API-style negotiation (default 3500 ms, range
200-10000). Triage and Steward share the total budget, not additive guarantees;
raise the total explicitly when both calls may consume their full timeouts.
Exhausted failures retain the slow path and log only bounded classification
diagnostics, including `steward_ms`, `reason=triage_timeout`, `triage_error`,
`steward_timeout` or `steward_error`, and `attempts`.

For triage requests, `ZOE_TRIAGE_THINKING` and `ZOE_TRIAGE_REASONING_EFFORT` (CLI `--thinking` and `--effort`) control the following wire fields. Effort accepts `none`, `low`, `medium`, `high`, `max`, or empty; `minimal` is rejected by the configured OpenAI-style gateway. Values are sent unchanged, not mapped locally.

| Thinking value | `/responses` | `/chat/completions` |
| --- | --- | --- |
| `disabled` | Sends `{"reasoning":{"effort":"none"}}` for every model, overriding configured effort. | Sends `{"thinking":{"type":"disabled"}}`; separately sends top-level `reasoning_effort` when configured. |
| `enabled` | Sends `{"reasoning":{"effort":"<configured effort>"}}` for every model when effort is set; otherwise omits `reasoning` and uses the provider default. | Sends `{"thinking":{"type":"enabled"}}`; separately sends top-level `reasoning_effort` when configured. |
| Empty | Keeps existing model detection: names starting with `gpt-6`, `gpt-5.6`, `o1`, or `o3` send configured effort, or `low` when empty; other models omit `reasoning`. | Omits `thinking`; sends top-level `reasoning_effort` only when configured. |

Responses requests never send the chat `thinking` field. Explicit thinking settings suppress `temperature` on Responses; otherwise existing model-based temperature behavior remains unchanged. Chat requests retain `temperature: 0`. An explicit effort of `none` is still sent unchanged with `enabled`, so avoid that combination when enabling thinking is intended.

When running `zoe triage test`, only the categories in the [triage routing contract](../architecture/decisions/0012-voice-conversation-layer.md#routing-stateless-triage-decided-2026-11-06) pass; removed or unknown model output reports FAIL. The `chat` API style refers only to Chat Completions transport, not a routing category.

[DeepSeek thinking mode](https://api-docs.deepseek.com/guides/thinking_mode) defaults to enabled with effort `high`; its Responses `none` disables thinking. DeepSeek maps `minimal` to `low`, `medium` and `xhigh` to `high`, and supports `max`; Zoe accepts `medium` but rejects `minimal` and `xhigh` for gateway compatibility. See its [Responses API guide](https://api-docs.deepseek.com/guides/responses_api). Memory effort validation uses the same accepted values; its existing empty-effort fallback remains `medium`.

### Call-recall model endpoints

When testing delegated-turn triage with `zoe triage test`, valid categories are
`progress`, `recall`, `memory_write`, `memory_read`, `task`, and `cancel`. The legacy
`memory` result is invalid and follows the unclassified slow task fallback at runtime.
The standalone probe has no pending question and rejects `confirm` and `decline`;
those categories are available only for a bound held-task answer in a live call.
Memory reads reuse `ZOE_ANSWER_*` and the existing owner-scoped memory context
assembler; no additional model is configured. Memory writes currently report
save unavailability without a Worker task; the ingestion contract is blocked
as recorded in [ADR 0012](../architecture/decisions/0012-voice-conversation-layer.md#personal-memory-routes).

For call-history recall, configure independent endpoints through the existing Models
menu or `zoe summary configure` and `zoe answer configure`. Both accept `--base-url`,
`--model`, `--effort` (empty, none, low, medium, high, max), `--thinking` (empty, enabled,
disabled), `--key-stdin`, and `--disable`. Keys are written atomically with mode
0600 and server ownership 65532:65532 to `/etc/zoe/secrets/summary-api-key` and `/etc/zoe/secrets/answer-api-key`.
Empty base URLs disable each endpoint independently; updates preserve their settings.
Environment names and secret references are in the [stack example](../../deploy/linux/zoe-stack.env.example).
The summary timeout defaults to 120000 ms (range 5000–600000); the answer timeout
defaults to 8000 ms (range 500–30000). Both use Responses first and remember a
Chat Completions fallback after HTTP 404, 405, or 501. The thinking/effort wire
contract is the same as the triage table above, including temperature suppression.
Existing keys must be readable by the server user before saving or probing;
status and update warn about unreadable configured keys, and update stops before
service changes. `zoe summary test` and `zoe answer test` invoke runtime
clients with synthetic evidence and report PASS/FAIL, API style, and latency;
`zoe status` reports configuration without credential contents. Apply with `zoe update`.

The summary client groups a full numbered call transcript into validated contiguous
topic ranges. The answer client uses candidate summaries first and requests known
transcript IDs only for missing wording or detail. These clients are integration
building blocks: this configuration does not schedule post-call work or connect
live recall to voice. Synthetic probes do not establish live-provider quality,
latency, deployment, or end-to-end acceptance.

The complete host installation therefore includes the Zoe-owned OpenCode runtime image,
Zoe server image, trusted runner, memory worker image, Qdrant service, Docker,
and the three Zoe systemd services. An external model gateway is not installed
by this procedure. Its URL, selected model, and API-key file are deployment
configuration applied to the OpenCode runtime and memory worker after the host components
are installed; leaving them unset keeps the daemons available but prevents
model-backed task execution.

### Upgrade and removal

Use `zoe update --release /path/to/release` to stage and pull
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

The Linux workflow's manual release option builds the `zoe` operator CLI for
Linux `amd64` and `arm64` on GitHub-hosted runners after its complete checks pass;
see [CI verification](../development/verification.md#completion-gate-and-manual-checks).
Each `operator-linux-<arch>-<revision>` artifact contains
a tarball with `bin/zoe`, `deploy-linux/install-cli.sh`, `SOURCE_REVISION`, and
`SHA256SUMS`. Select the host architecture, extract into a new immutable release
directory, and verify `sha256sum -c SHA256SUMS` from that directory. This operator
archive does not contain the server images or a complete service deployment.

For a complete source-built release, `deploy/linux/build-release.sh RELEASE_TAG`
also installs `bin/zoe` beside the runner in `/opt/zoe/releases/RELEASE_TAG`.
Builds must run in CI or an authorized worker, not on the controller. After
reviewing the release, install its CLI on the standard host PATH with:

```sh
sudo bash /opt/zoe/releases/RELEASE_TAG/deploy-linux/install-cli.sh /opt/zoe/releases/RELEASE_TAG
zoe
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

On the host, apply the reviewed plan manually. Keep old immutable release
 directories and image tags. Rollback changes only the `current` selector and
Compose image tags, then repeats `/healthz`, `/readyz`, runner/OpenCode/Qdrant, and
independent task checks. Record integrity, installation, readiness, connectivity,
and end-to-end results separately; see the [acceptance evidence register](../acceptance/evidence.md)
for the acceptance boundary. This does not modify the external CLIProxyAPI deployment.
