# Zoe architecture

When producing server or runner diagnostics, follow the [logging standard](operations/logging.md).

This document records the current architecture and its boundaries. It describes responsibility and required behavior; it is not an implementation or delivery claim—see the [acceptance evidence register](acceptance/evidence.md) and [master plan](master-plan.md). [Product requirements](product.md) define the full scope. Archived research is historical provenance rather than a competing current design.

## Result delivery

Assistant task results are durably recorded with their authoritative event. When voice account media is enabled, the voice package consumes those events with a durable cursor, creates idempotent delivery rows, and dispatches the full original text to the owner's active voice session under the [context delivery contract](architecture/decisions/0013-context-register.md#6-reports-and-latency). Without a live call they remain available through the existing notification path. Whether the voice model actually SPEAKS the appended text is UNVERIFIED pending a live-call experiment.

## System and authority

For delegated task admission on main, [ADR 0013](architecture/decisions/0013-context-register.md) uses register routing and alias-free Steward `Choose`, separate from resident OpenCode Worker execution. The [roster-aware contract](architecture/decisions/0012-voice-conversation-layer.md#deployed-roster-aware-task-manager) is historical: deployed on `bb07671`, removed on main. Replacement deployment and live acceptance remain unverified.

Zoe is a private, self-hosted Linux Go control server with an independent Linux runner, SQLite WAL with `synchronous=FULL`, and native SwiftUI clients for macOS, iPhone, and iPad. The first release is single-instance and multi-project with one fixed owner principal. User/workspace boundaries remain explicit for future evolution.

| Component | Responsibility | Authority boundary |
|---|---|---|
| Native Apple clients | Conversation, voice/text input, task progress/results, decisions, projects, notices, and local drafts | Render server records and stale/offline state; never establish business completion locally |
| Go control plane | Projects, commands, permissions, tasks, runs, receipts, events, service definitions and audit | Sole authoritative business-state writer |
| Independent runner | Claim admitted work, invoke authorized runtime/service operations, observe results, and report evidence | Separate credentials and scoped claims; cannot grant its own authority |
| Zoe runner and OpenCode | Authorized session creation and lifecycle for each run through the dedicated host OpenCode service | Zoe admits work and owns runtime/session lifecycle; the runner is the trusted host-side lifecycle authority |
| Existing remote CLIProxyAPI | Account selection, quota handling, and proxying for its configured model routes | External OpenAI-compatible provider reused unchanged; no duplicate account rotation in Zoe |
| Zoe-compatible `ronigooja/chatgpt-web-voice` fork | ChatGPT web real-time transport and voice account pool | Voice transport and spoken output do not authorize or complete business work |
| Mem0 | Owner-scoped memory extraction, storage, and retrieval | Controlled worker, never unrestricted task/permission/configuration/decision authority |
| Qdrant | Derived retrieval vectors and payloads | Rebuildable index; never the source of truth |
| SQLite and file storage | Durable authority transactions; artifacts/releases and service data in file storage | Database commit establishes acceptance; large outputs do not become database blobs |

The durable association is `Project → Task → Run → Runtime Session`. Each run records its runtime, model route, execution host, workspace, authorization, artifacts, and result evidence. Role, runtime implementation, and model route are separate choices. The six product responsibilities map onto these components; they do not require six separately deployed services. The runtime baseline is defined by [ADR 0010](architecture/decisions/0010-host-opencode-runtime.md).

There is no PWA, CAO implementation, macOS server, or official Codex desktop-app integration. Native CLI handoff is optional and bounded by supported native runtime facilities. See [ADR 0001](architecture/decisions/0001-linux-and-native-apple-clients.md) and the current runtime baseline in [ADR 0007](architecture/decisions/0007-zoe-owned-opencode-runtime.md).

## Commands, receipts, and events

The public API uses REST under `/v1` and SSE for ordered events. Implemented data contracts live in [contracts](../contracts/), particularly [HTTP contracts](../contracts/http.go); documents do not establish a second schema.

1. A native client submits a business command with an `Idempotency-Key` and, when applicable, `expected_version`.
2. The control plane checks the current principal, workspace/resource permissions, command validity, and optimistic version.
3. One transaction commits the durable command receipt, work record, and outbox event before returning acceptance.
4. The runner claims admitted work atomically. Zoe checks authority, budget, and the shared run cap before the runner creates the OpenCode session.
5. OpenCode and runner reports return observations/proposals. The control plane validates the current claim, fence, version, and evidence before committing business-state changes.
6. Event delivery retries from the outbox. Consumers deduplicate stable identities and repair missing history from the authority.

Initial command kinds include `project.create`, `task.create`, `task.append`, `task.stop`, `task.approve`, and the compatibility command `task.accept_result`. Workspace references are opaque server-owned IDs, not client-supplied host paths. A timed-out client recovers the original receipt through command ID or original key; it does not create a second command merely because the first response was lost. Drafts clear only after durable acceptance. `task.accept_result` remains only as a compatibility path for legacy clients/workflows; it is not silently reinterpreted as conversational confirmation and is not the ordinary result UI.

An authenticated snapshot gives the current workspace/subject, version, and durable event cursor. Clients resume events using `Last-Event-ID`; an invalid or expired cursor requires snapshot repair. The authenticated screen endpoint uses that same cursor as its document revision; clients refetch it when the event cursor advances. Paged task history reads authoritative messages and deduplicates by message ID. Snapshot caches and push payloads do not create alternative state authorities.

Unknown external effects are reconciled if queryable and escalated otherwise. They are never blindly retried. Receipt, admission, running work, model output, actual operation success, goal completion, and confirmed stop remain distinct. See [ADR 0001](architecture/decisions/0001-linux-and-native-apple-clients.md).

## Runtime admission and model routing

Every request, including a child Worker request from Super, uses the same controlled admission path. No automatic dispatcher may bypass it. Zoe's runner owns creation and lifecycle of every OpenCode runtime; Zoe owns task authority and accepted effects. Model selection is an OpenCode provider/model string.

The global ceiling is the `ZOE_MAX_RUNS` operator setting (default 12), counting Super, Workers, and curator work. Profiles select server-derived permission tiers and ordered OpenCode rules; see [ADR 0011](architecture/decisions/0011-profiles-permissions-and-context-hygiene.md). A waiting-user or uncertain runtime does not free a slot merely because no output is visible; `ZOE_WAIT_LIMIT_MINUTES` bounds retained waiting sessions. Ownership/fence checks and termination evidence govern release. Unknown OpenCode container creation is not automatic replay authority; explicit label reconciliation requires complete, auditable evidence. The [deployment runbook](operations/deployment.md) owns the operational procedure.

Deterministic scheduling, health, state transitions, and routine event routing make no model calls. A semantic event may call a Worker; complex coordination may call Super. Every actual model call must be attributable to an authorized task/run, route, and budget. Usage reservations and settlements must be idempotent; unknown usage remains reserved. Provider calls, tool loops, artifact bytes, and termination limits require enforcement at the actual effect boundary, not merely budget fields in a contract. Full per-provider-call enforcement and cost evidence remain acceptance work; see the master plan.

For routes supplied by CLIProxyAPI, account selection and quota handling remain in that existing deployment. Zoe uses it only as an OpenAI-compatible provider through the OpenCode bridge, with deployment-provided endpoint and scoped secret references, without changing that service. Adapter registration or a model catalog response does not prove runtime execution, artifact creation, provider usage, or successful stop.

## Identity, clients, and notices

A fresh deployment uses the shared instance-key boundary defined by [ADR 0004](architecture/decisions/0004-instance-key-authentication.md); runner credentials and the shared memory HTTP API token remain separate.

Clients authenticate with the server URL and shared key through the validation, Keychain, and rotation rules in [ADR 0004](architecture/decisions/0004-instance-key-authentication.md); local clearing is not server-side revocation.

A stable random installation UUID identifies a push registration independently of the shared owner key. One installation cannot overwrite or revoke another installation's token. APNs is only a wake-up hint to reload durable inbox state. Payloads contain no task content or notice body. Delivery retry and invalid-token handling remain server responsibilities; real APNs delivery requires separate acceptance evidence.

Authenticated Apple clients may submit a bounded, encrypted local queue of redacted runtime diagnostics to `POST /v1/diagnostics/client`. The server pushes the configured log level and poll interval through authenticated `GET /v1/client/policy`; clients poll every 60 seconds by default. This applies only after the client has authenticated the Zoe connection. Events identify a voice stage, request path, status, response size, client request ID, app/OS versions, and a hash of the installation ID; they exclude credentials, headers, response bodies, SDP/ICE, audio, and transcripts. The client does not present detailed server, infrastructure, runtime, transport, account, or local exception text to the user: it shows only a concise actionable state and automatically queues and uploads the redacted diagnostic when authentication and transport permit. This upload is best effort and must not block interaction, change command authority, or change receipt/retry semantics. The server stores these events in an independent short-retention table with batch, event-size, and per-device rate limits. Instance restarts and container rebuilds do not create or rotate the instance key; only the explicit CLI `key init` and `key rotate` operations change that authority, and diagnostic records never contain key material.

Client behavior follows [product chapter 3](product.md#client-behavior-boundaries). Infrastructure capacity, OpenCode/runner faults, runtime IDs, and reconciliation procedures belong in operator diagnostics/audit, not client surfaces. Actual task state, actionable submission failure, and pending confirmation remain visible. Runtime diagnostics do not change receipt or retry semantics.

### Ambient macOS screen and client-tools boundary

On macOS the server/AI owns a versioned declarative screen document and opaque interaction intents; the client owns the generic renderer, platform visuals, accessibility, and graceful degradation. The client never fabricates content, hardcodes business concepts, or decides an intent's outcome, and it exposes the surface, component-catalog, settings, voice, and text-bar tools listed in [the screen-document contract](design/screen-document.md). The AI controls its own surfaces and window form; the menu-bar item and microphone-use indication always remain. Rationale is in [ADR 0008](architecture/decisions/0008-ambient-screen-client.md); the Go contract in [`contracts/screen.go`](../contracts/screen.go) is authoritative while the Swift renderer is pending.

## Voice and business work

When reading the historical voice descriptions below, [ADR 0013](architecture/decisions/0013-context-register.md) supersedes triage, impact confirmation, opening instructions and task/context routing. These old gates, fallback and interaction states were deployed on `bb07671`, removed on main; they are not current implementation requirements.

Zoe-compatible `ronigooja/chatgpt-web-voice` fork is the selected gateway/account pool. The current voice topology and lease boundary are defined by [ADR 0005](architecture/decisions/0005-voice-task-and-lease-boundaries.md); deployment details live in [voice deployment](../deploy/voice/README.md).

When gateway transcript ingestion is enabled, full call-text authority and its independent service credential follow the [transcript lifecycle contract](operations/data-lifecycle.md#voice-transcript-authority-and-ingestion); transcript storage is separate from command-authorizing committed delegation turns.

When the register routes `recall`, the server searches the memory worker using the register restatement and appends retrieved text to the upstream voice model. The whole-call summary pipeline deployed on `bb07671` is removed on main; migration 220 removes its jobs, summaries and triggers. Transcript export and destruction coverage remains. Migration 221 removes unused legacy decision/confirmation columns and the instruction-delivery table, preserving active turn state. See [ADR 0013](architecture/decisions/0013-context-register.md#3-memory-cadence) for historical context; implementation does not establish deployment acceptance.

The selected gateway-relayed media topology and `AccountMedia` mode are specified in [ADR 0005](architecture/decisions/0005-voice-task-and-lease-boundaries.md); this architecture retains only that boundary rather than duplicating its rationale.

On macOS, continuous voice is on by default and ends itself after a configurable silence timeout (default 60 seconds, counted only when no voice interaction is in progress); server context permits later continuation, and voice on/off and the timeout are AI-mutable settings. After connection and identity initialization, the client automatically attempts continuous voice: it attempts to establish and maintain an authorized voice session while OS microphone permission, audio-session policy, app lifecycle/background limits, network, account-pool capacity, and server/account policy permit it. These constraints are independent: permission does not override audio policy, lifecycle limits, account availability, or server rejection. The client reports the actual lifecycle state and retains text fallback; temporary failures may reconnect, while permission/policy failures require truthful recovery guidance in Settings or system settings. Stable turn IDs, revisions, and deduplication bind voice turns to durable commands. Partial captions, spoken output, and transport continuity never authorize business work. Voice is the front desk and a spoken request is the instruction. Committed delegated turns are routed outside command transactions by the [register](architecture/decisions/0013-context-register.md#1-register-first-triage-deleted), not stateless triage. Register, Steward and memory-worker failures create no task and speak `Processing error.`; the former impact-confirmation gates and slow fallback are removed.

The authenticated `voice.turn.handoff` command is the server-owned handoff boundary. It accepts only a committed turn's workspace/subject/device-bound session generation, stable turn identity/revision, and matching content hash. Idempotent command receipts acknowledge replay; conflicting content and stale generations fail closed. Current routing, alias-free task selection and memory delivery follow [ADR 0013](architecture/decisions/0013-context-register.md) and [handlers.go](../internal/voice/handlers.go). ADR 0012's roster/held-task behavior is historical; source implementation is not deployment or live acceptance.

Device, account, and media leases carry holder, generation, expiry, and termination evidence. A new account gets a new media session while preserving task associations. The old media session must end before account reuse; expiry alone is not evidence of termination. Unknown sessions/accounts remain quarantined. Muting capture, stopping playback, ending voice, and stopping a task are distinct operations. Natural conversation may continue while business work runs; only authoritative task events announce formal completion. Raw audio is not retained by default. See [ADR 0005](architecture/decisions/0005-voice-task-and-lease-boundaries.md).

The selected gateway's missing termination evidence and fail-closed account-reuse boundary are design constraints, not proof of a working end-to-end voice path; see [ADR 0005](architecture/decisions/0005-voice-task-and-lease-boundaries.md). Zoe tracks the gateway media generation separately from Zoe's client-facing session generation. Termination evidence is a gateway-attested local observation of a closed relay peer, not proof that the upstream account has been reclaimed, and exists only for gateway-relayed sessions; client-direct media remains fail-closed.

The control plane owns interaction state separately from task execution status. It may expose `awaiting_message`, `explicit_confirmation`, or exceptional `approval_required`; ordinary completed results have no interaction requirement. `awaiting_user` remains an API compatibility name and is not a client authority. A `task.append` against a waiting task persists the user message, preserves result/evidence history, applies server policy and expected-version checks, and emits an authoritative event. When a run completes, the runner reports the final completed OpenCode assistant message through the authenticated observation path; Zoe stores its bounded text once as an authoritative `assistant_message` task entry and emits `task.assistant_message` with `task_id`, `run_id`, `entry_id`, `entry_kind`, and role in the same transaction. Clients and downstream voice/notification consumers render or deliver that entry and never fabricate conversation history. The server resolves whether a follow-up is clarification, requested change, continuation, or confirmation. Confirmation records scope, task/run/action identity, expiry, expected version, confirmed text, device, and audit metadata.

## Memory, retrieval, and context

When the memory worker API is configured, `internal/memory` schedules owner-scoped ingestion through `internal/memoryapi.Client.Ingest` using the same worker base URL, Bearer authentication, and JSON `[workspace, owner]` user identifier as recall/write. The minute ticker runs no immediate startup batch. `ZOE_MEMORY_BATCH_INTERVAL_HOURS` accepts 1..168 (default 24), measured from the last successful ingestion; failures retry on the next tick without advancing progress. Owners without new transcript or completed-task content make no worker call.

For scheduled ingestion, Go reads turns and completed tasks in `(boundary, tick time]`, preserving chronological turn order and original task titles/results. Transcript blocks are `[RFC3339Nano UTC] user: text\n` or `assistant: text`; task blocks are `[RFC3339Nano UTC] completed task:\nuser: original request\nresult: result\n`, appended after the transcript in completion order. Sequential requests contain at most 200 KiB of UTF-8 text and at most 256 KiB of serialized JSON, preserving whole turns/tasks unless an individual block exceeds either limit. All chunks must succeed before a compare-and-swap advances `memory_ingest_state.boundary_ns` and the next-attempt deadline. Missing owner state starts at the beginning (Unix epoch). Retried chunks may duplicate earlier successes; this is not an exactly-once ingestion guarantee. Source: [batch.go](../internal/memory/batch.go).

Memory lives in Mem0/Qdrant behind the Python worker. The Go scheduler performs no extraction-model call, existing-key lookup, staged write, or claim write. `ZOE_MEM0_LLM_*` configures only the worker's model and is not required by Go startup. Migration 222 in the independent `memory_ingest` namespace creates the small progress table; the namespace and `internal/memoryapi` migration owner survive removal of legacy claim storage. The claim subsystem, its migrations, commands, routes, runner context, and native management UI are removed. Only fresh databases are supported; no old-database migration or old-backup restore path exists. Historical ADRs describe the superseded storage/extraction design, not this ingestion contract. Formal task state remains authoritative in the task ledger.

The Python worker owns embeddings, Mem0 history, and Qdrant vectors; [ADR 0006](architecture/decisions/0006-memory-authority-and-derived-stores.md) defines the storage boundary. Go has no SQLite memory authority or fallback index. Exact versions, model hashes and worker configuration belong in [config.py](../services/memory/config.py). Durable Qdrant storage and shared HTTP credentials are specified in [deployment](operations/deployment.md#server-and-worker-settings).

Explicit `memory_write` uses a durable at-most-once job, then worker search plus one LLM call deciding add/replace/none; success is silent. Recall searches using the register restatement and appends retrieved texts unmodified to the upstream voice model. No answer model exists. [ADR 0013](architecture/decisions/0013-context-register.md#3-memory-cadence) owns routing, crash handling and delivery semantics.

The Go client exposes synchronous worker `delete-all`, returning failures without durable retry, but no user-facing entry point invokes it today ([client.go](../internal/memoryapi/client.go)). Logical exports omit memory explicitly because the worker has no export endpoint ([export.go](../internal/control/export.go)). Complete instance destruction must still inventory and remove Mem0 and Qdrant resources. Historical ADR deletion/rebuild semantics are superseded for memory.

## Capabilities and deployment isolation

A service definition has its own identity, purpose, environment, triggers, permissions, configuration, health, and execution history. Each trigger produces a finite run. Ordinary services continue without a client or Super session and have resource limits separate from the agent-run ceiling.

When deploying generated capabilities, source, tests, and a release manifest first pass capability-manager validation and the applicable user authorization. A constrained manifest names immutable artifact/version, entrypoint, deployment-managed environment references, allowed mounts, and capabilities. The runner enforces user, mount, network, port, CPU, memory, process, and log limits. Workload manifests cannot grant privileged mode, arbitrary host mounts/devices, or Docker-socket access.

Temporary workspaces/caches, immutable releases, deployment configuration, and long-lived service volumes use separate roots. Each persistent volume has an owner, purpose, mount, backup/restore policy, retention rule, and deletion action. Runtime workspace cleanup cannot remove deployed data or its only release artifact.

The trusted runner is independently supervised on a dedicated Linux execution host and necessarily has Docker-daemon or equivalent host authority. That host authority is trusted infrastructure, not a workload isolation boundary. Keep its socket out of server, memory, OpenCode workload containers, and capability manifests. systemd supervises the control/memory stack and independent runner. [Linux deployment configuration](../deploy/linux/) and [the deployment runbook](operations/deployment.md) own exact commands and layout.

## Backup, deletion, and recovery

At most one active control server may operate on a database. Migration/restore pauses dispatch, isolates the old instance, restores a consistent snapshot plus artifacts/configuration/required volumes and deletion manifests, and reconciles services and uncertain effects before execution reopens. Secrets are recovered separately or external integrations are reauthorized.

Restore must preserve the operator-maintained current deletion generation/head/hash as its independent trust root. A backup's embedded journal head cannot prove freshness or later deletion coverage. Restore-only gating blocks activation and business reads/writes until reconciliation succeeds. Missing real adapters/checkpoints keep the gate closed; operators cannot replace evidence with a manually asserted success.

Backup inventory and retention include applicable service data and deletion manifests. Physical deletion, retention, host isolation, migration, and measured production restore remain acceptance boundaries. Daily backup scheduling, RPO 24 hours, and RTO two hours are unverified targets, not measured guarantees. Vector-index rebuild duration is reported separately from core recovery time. Operational procedures belong in [data lifecycle operations](operations/data-lifecycle.md); evidence and unresolved acceptance remain in [acceptance evidence](acceptance/evidence.md) and [the master plan](master-plan.md).

## Decision records

These records capture selected decisions, not new scope or acceptance claims:

- [0001 — Linux/native Apple platform and durable persistence](architecture/decisions/0001-linux-and-native-apple-clients.md)
- [0002 — superseded runtime ownership and external-service boundaries](architecture/decisions/0002-ao-runtime-ownership.md)
- [0004 — Shared instance-key authentication](architecture/decisions/0004-instance-key-authentication.md)
- [0005 — Voice turns, tasks, and lease termination](architecture/decisions/0005-voice-task-and-lease-boundaries.md)
- [0006 — Memory authority and derived stores](architecture/decisions/0006-memory-authority-and-derived-stores.md)
- [0007 — Zoe-owned OpenCode runtime (historical)](architecture/decisions/0007-zoe-owned-opencode-runtime.md)
- [0010 — Host-installed OpenCode runtime](architecture/decisions/0010-host-opencode-runtime.md)
- [0008 — Ambient macOS screen client](architecture/decisions/0008-ambient-screen-client.md)
- [0009 — Voice front desk, steward routing, and impact-level authorization](architecture/decisions/0009-voice-steward-impact-policy.md)
- [0011 — Profiles, per-run permissions, waiting runs, and bounded context](architecture/decisions/0011-profiles-permissions-and-context-hygiene.md)
- [0012 — Zoe-owned conversation layer for real-time voice (proposed)](architecture/decisions/0012-voice-conversation-layer.md)
- [0013 — Persistent conversation context register (Accepted, not implemented)](architecture/decisions/0013-context-register.md)
Scrapling is an optional web-access MCP dependency configured as remote HTTP or host-local stdio; its token remains a deployment secret reference.
