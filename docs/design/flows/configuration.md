# Connection, model configuration, and deployment contract

## Purpose and boundary

Approved target, 2026-11-16, with complementary fresh-instance authentication obligations; not implementation, deployment, or acceptance evidence. The [approved overview](../data-flows.md) establishes precedence. This contract owns instance-key/connection rules, independent model consumers, atomic operator configuration, and retained deployment identity/state. [Execution](execution.md) owns runtime/effect lifetime; [operations](../../operations.md) owns procedures. No external service or existing CLIProxyAPI configuration change is authorized.

## Inputs and outputs

| Input | Consumer and transformation |
|---|---|
| Client server URL and shared instance key | Normalize/validate address/key, authenticate snapshot, establish fixed owner/workspace connection. |
| Operator selected provider/model, effort, secret references | Probe, then atomically save/reload environment/routes, OpenCode provider/agent configuration, and session selection. |
| Register, Compact, Steward, UI_Controller, memory, Worker configuration | Preserve independent per-consumer settings and effective configuration identity. |
| Release manifest, network configuration, key references, persistent data | Deploy immutable release while retaining configuration/data, backup/rollback references, and network reachability. |

## Required behavior

### Instance key and native connection

Fresh deployment uses one canonical 32-byte unpadded base64url instance key (43 characters), granting fixed principal `owner` in workspace `default`. Server stores only its hash and never prints the key during ordinary startup. Runner credentials, memory-worker HTTP Bearer token, and transcript-ingestion service credential stay separate. All key holders have owner access; retain explicit future owner/workspace/permission boundaries without adding team UI or historical password/device-session compatibility.

A fresh server without instance-auth row is pending: health remains available, every `/v1` request returns `401` until explicit `zoe key init`; `GET /readyz` reads database each request and returns `503` with `instance_key_not_initialized` while pending, or `200` with `{"status":"ready"}` after initialization. Instance restart/container rebuild never initializes or rotates a key. Only explicit CLI `key init`/`key rotate` changes that authority.

Clients collect server URL and key. Canonical credential link is `zoe://connect?server=<URL-encoded>#key=<key>`; entire link/QR is secret. External installation links stage values for review and require Connect. Explicit QR image import/camera scanning connects after validation; HTTP address requires confirmation before transmitting key. iPhone/iPad requests camera permission only on Scan QR code; selected-image import needs no camera access, and camera frames decode locally without saving/uploading.

Accept only canonical 43-character keys. Normalize URL scheme/host case, default ports, trailing slashes, and retained deployment path. Reject user information, query/fragment, traversal, encoded separators, and invalid ports. Keychain is the only client credential store, scoped to normalized URL. Authenticated snapshot validates owner/workspace. No password/session endpoint, approved-device list, fixed endpoint, or saved-server list is part of this design.

Replacement validates the new key before applying and retains current address. Different server requires explicit clearing/fresh connection. Clearing/replacement discards old local durable queue, cached snapshot, exports, and drafts; clearing neither revokes shared key nor stops server work. Operator rotation blocks future old-key access and requires redistribution; key loss requires operator replacement, not password recovery. Rejected credentials close workspace access; ordinary transport errors preserve validated state with stale feedback. Push registration is scoped by [separate installation identity](results.md#notices).

### Model consumers and atomic configuration

Probe proposed provider/model/effort before saving. Atomically update corresponding environment/routes, OpenCode provider/agent configuration, and session selection; every Worker layer resolves the same model+effort. Retain previous known configuration and applied version. Probe/save/reload failure rolls back the whole change, never partially selects a route. Verify effective runtime/session configuration, not menu success alone.

Register, Compact, Steward, UI_Controller, memory, and Worker have separate configurations; target only chosen consumer without silently changing/falling back to another. A working consumer does not prove another configured; report truthful failure. Keep provider secrets in protected service-side files/deployment references, never source/logs/fixtures or runtime-visible prompt/configuration evidence. Record route identity without secret material. CLIProxyAPI owns its account/quota routing; Zoe must not duplicate or mutate it.

#### Register configuration and routing deadlines

Register and Compact use independent `ZOE_REGISTER_{BASE_URL,MODEL,API_KEY_FILE,REASONING_EFFORT,THINKING,TIMEOUT_MS}` and `ZOE_COMPACT_{BASE_URL,MODEL,API_KEY_FILE,REASONING_EFFORT,THINKING,TIMEOUT_MS}` groups, with no Steward/cross-model fallback. Both groups are optional at startup; enabling register requires each group's `BASE_URL`, `MODEL`, and readable nonempty `API_KEY_FILE`. Unset/partial groups permit startup but disable register, with one `register status=disabled reason=not_configured missing=<comma-separated variable names>` log line. Disabled routing admits no task or newly delegated recall, runs no register compaction, and contributes no register opening context; retain fixed processing-error speech, not another consumer's fallback.

A complete group's unreadable/empty key file fails startup by variable name even when the other group is incomplete, without secret values. Invalid explicitly set tunables likewise fail startup by variable name. Compose accepts missing host variables and uses `/dev/null` for unset independent secret files. Register/Compact effort and thinking default empty. Secret-file defaults are `/etc/zoe/secrets/register-api-key` and `/etc/zoe/secrets/compact-api-key`, mode `0600`, server ownership `65532:65532` as for Steward. Operator editing/restart steps remain in [operations](../../operations.md), not this contract.

| Variable | Default | Inclusive valid range |
|---|---|---|
| `ZOE_REGISTER_TIMEOUT_MS` | 30000 ms | 500–60000 ms |
| `ZOE_COMPACT_TIMEOUT_MS` | 600000 ms | 5000–600000 ms |
| `ZOE_COMPACT_MAX_CHARACTERS` | 8000 | 500–100000 |
| `ZOE_COMPACT_INTERVAL_MINUTES` | 60 minutes | 1–1440 minutes |
| `ZOE_REGISTER_RAW_WINDOW_MINUTES` | 60 minutes | 5–1440 minutes |
| `ZOE_STEWARD_TIMEOUT_MS` | 30000 ms | 200–60000 ms |
| `ZOE_VOICE_CLASSIFICATION_TIMEOUT_MS` | 35000 ms | 100–60000 ms |

Classification bounds register plus Steward routing and handoff wait; component budgets are not additive guarantees. Steward timeout includes API-style negotiation; its probe invokes `Choose`. Register/Compact probes each issue one real request through `internal/modelclient`, only to the selected group's endpoint/key, using `/responses` relative to configured base URL; a failed route fails the probe, not a second request. Missing required variables fail Test by variable name; any current Test wording claiming server startup failure does not override optional-group startup semantics. Configure alone does not restart services. No Register, Compact, or Steward CLI subcommands/flags are supplied. Triage, call-summary, and answer-model Configure/Test entries remain unavailable.

Raw-window width determines compact right edge, but raw starts at the retained compact edge after rebuild failure. [Memory](memory.md) owns contiguous coverage, ingestion cadence, and payload semantics.

#### Model wire settings

Go model-client reasoning effort accepts `none`, `low`, `medium`, `high`, `max`, or empty; reject `minimal` and `xhigh` for configured gateway compatibility. Send accepted values unchanged, without local remapping. Memory validates the same values and retains its existing empty-effort fallback of `medium`.

| Thinking value | `/responses` | `/chat/completions` |
|---|---|---|
| `disabled` | Send `{"reasoning":{"effort":"none"}}` for every model, overriding configured effort. | Send `{"thinking":{"type":"disabled"}}`; separately send top-level `reasoning_effort` when configured. |
| `enabled` | Send `{"reasoning":{"effort":"<configured effort>"}}` for every model when effort is set; otherwise omit `reasoning` and use provider default. | Send `{"thinking":{"type":"enabled"}}`; separately send top-level `reasoning_effort` when configured. |
| Empty | For names starting `gpt-6`, `gpt-5.6`, `o1`, or `o3`, send configured effort or `low` when empty; other models omit `reasoning`. | Omit `thinking`; send top-level `reasoning_effort` only when configured. |

Responses never sends chat `thinking`. Explicit thinking suppresses Responses `temperature`; otherwise retain existing model-based temperature behavior. Chat retains `temperature: 0`. Explicit effort `none` with `enabled` is still sent unchanged, not converted into enabled reasoning. Provider behavior is separate: DeepSeek defaults thinking enabled/effort `high`, Responses `none` disables it, and its mappings (`minimal` to `low`, `medium`/`xhigh` to `high`, with `max` supported) do not change Zoe's accepted values or unchanged wire values.

Host runner requires `ZOE_OPENCODE_BASE_URL` (absolute HTTP(S)), `ZOE_OPENCODE_PASSWORD_FILE` (absolute private Basic Auth file), `ZOE_OPENCODE_MODEL` (server/provider route), and `ZOE_OPENCODE_WORKSPACE_ROOT` (absolute private parent). Dedicated service owns data, port, password, provider and permissions; provider keys never enter runner logs. Pin Zoe-managed operator-fork OpenCode release in Linux release artifact. Install/update checksum-verifies matching Linux asset into `/opt/zoe/opencode/<tag>/opencode`, unless operator explicitly supplies `--opencode-binary`/`ZOE_OPENCODE_BINARY`; validate overrides and record selected path in runner environment/rendered unit. [Execution](execution.md#host-opencode-lifecycle) owns lifecycle/permission semantics. Historical endpoint checks against source are not proof deployed binary conforms; recheck deployed V2 endpoints and permission behavior before rollout.

### Client diagnostics

All native error surfaces show short actionable state/retry or recovery guidance, never detailed infrastructure/runtime/transport/account/queue-capacity/local exceptions. Auth/user-input failures may show minimum recovery detail, not secrets. Redacted details are encrypted in a bounded local queue and best-effort uploaded only through an authenticated Zoe connection to `POST /v1/diagnostics/client`. Authenticated `GET /v1/client/policy` controls log level and poll interval; default policy/queue synchronization is every 60 seconds. Upload never blocks interaction, changes authority, or alters receipt/retry semantics.

Diagnostic events may identify voice stage, request path/status/response size, client request ID, app/OS versions, and installation-ID hash; exclude credentials, headers, response bodies, transcripts, audio, SDP/ICE, and secret-bearing values. Server keeps independent short-retention storage with batch/event-size/per-device-rate limits. Detailed errors belong to protected operator diagnostics/audit, not native surfaces.

### Deployment and retained data

Retain network reachability, keys, and persistent data. Image-only gateway updates do not reset configuration/state. Keep immutable release identity, retained configuration/data inventory, and explicit backup/rollback references. Reset, backup, and restore are explicit operations: never silently regenerate keys or destroy data to make startup pass. Health/startup is not end-to-end success.

One active control server per database. Migration/restore pauses dispatch, isolates old authority, restores consistent database/files/artifacts/configuration/required service volumes/deletion manifests, and reconciles services/uncertain effects before reopening execution. Preserve linked identities, versions, sources/effective times, runtime/workspace associations, and enough service/task state to resume; native processes need not migrate. Recover secrets separately or reauthorize integrations. Memory's [fresh-only and export/backup limitations](memory.md#product-scope-and-current-exclusions) apply; general migration obligations do not invent old-memory-schema restore support.

Restore preserves independently maintained current deletion generation/head/hash as trust root: backup-embedded head alone cannot prove freshness/later deletion coverage. Restore-only gate blocks activation and business reads/writes until actual adapters/checkpoints reconcile; missing evidence stays closed, not manually asserted success. Inventory retention/deletion includes derived stores, caches, exports, replicas, service volumes, and backups. Preserve worker history/Qdrant independently of Go export. Daily backup, RPO 24 hours, and RTO two hours are unverified targets; report vector-index rebuild separately from core recovery. [Operations](../../operations.md) owns exact procedures, not this contract.

## Persistence and idempotency

Retain effective per-consumer configuration identity and previous known applied version for rollback. Keychain identity is normalized-server scoped; server hash remains stable across ordinary restarts/updates. Durable command/receipt identity and generation protections remain under [execution](execution.md#persistence-and-idempotency) and [screen recovery](screen.md#event-and-full-state-recovery), unaffected by diagnostic upload or configuration UI. Keep release inventory and backup/rollback references without credentials.

## Failure and recovery

Probe, save, or reload failure restores whole prior model configuration. Unavailable provider/credentials/unsupported route fails closed with truthful evidence, not another consumer/model fallback. Keychain denial, rotated/rejected keys, and invalid imports must not replace a validated connection with an unvalidated one. Ordinary transport failures keep stale validated state. Unknown accepted commands reconcile original receipts, not fresh commands; connection changes clear only the old local data, not server authority/tasks. Missing restore/deletion or uncertain-effect evidence keeps activation blocked.

## Implementation locations

[Operator configuration](../../../cmd/zoe/configure.go), [menu/operations](../../../cmd/zoe/operations.go), [runner](../../../internal/runner/engine.go), [Linux deployment](../../../deploy/linux/), [voice deployment](../../../deploy/voice/README.md), [Apple client](../../../clients/apple/Packages/ZoeKit/Sources/), [worker configuration](../../../services/memory/config.py).

## Verification obligations

Verify key/address canonicalization, external-link review versus explicit QR connection, HTTP confirmation, image/camera privacy, authenticated snapshot scope, Keychain denial, replacement/clearing, rotation, concurrent installations, pending auth/readiness, and real protected native interaction. Verify probe/save/reload failure atomic rollback; effective Worker route/provider/agent/session model+effort equality; consumer separation/partial-group/invalid-setting behavior; every routing/compaction default and range boundary, shared deadline behavior, probe endpoint/no-second-request behavior, effort validation/unchanged values, thinking wire matrix, temperature rules, and memory's empty-effort fallback; diagnostic redaction, limits, and best-effort independence; retained network/key/data; explicit reset/backup/restore; gate integrity; and measured recovery independently of health.

Report verification using [evidence and acceptance](../../development.md#evidence-and-acceptance); this contract establishes no test, deployment, or acceptance result.
