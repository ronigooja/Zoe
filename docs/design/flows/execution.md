# Task execution and runtime contract

## Purpose and boundary

Approved target, 2026-11-16, with complementary host-runtime and product obligations; not a claim of source conformance, deployment, or acceptance. The [approved overview](../data-flows.md) takes precedence. [Register](register.md) routes; [Steward](steward.md) decides; the control plane alone commits business state; the independent trusted runner executes admitted work and reports fenced evidence. Media, tasks, runtime sessions, and services have distinct lifetimes.

The durable association is `Project → Task → Run → Runtime Session`. First-release server/execution hosts are Linux, independent of native Apple clients. OpenCode is the only execution runtime; CLIProxyAPI remains an unchanged external provider. Role, runtime, and model route are distinct choices. Model/configuration ownership belongs to [configuration](configuration.md).

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Gateway final: `turn_id`, `revision`, `transcript`; client adds `session_id`, `generation`, `content`, `user_confirmed` | Commit validates current identity/revision/content and retains original provenance, never later execution fallback. |
| Client handoff: `session_id`, `generation`, `turn_id`, `revision`, `content_hash` | Check committed source; classify outside the authority transaction. Handoff contains no client-authored goal. |
| Register output/source identity | Persist routes/request; send only retained request to route consumers. |
| Task route: request, `display_requested`, candidate facts | Steward decision followed by transactional admission/append/answer/cancel/report/redisplay. |
| New admission: request | Task goal becomes Worker goal; source reference is metadata, not another initial instruction. |
| Append/answer: request, `task_id`, plus `question_id` for answer | Original live OpenCode session receives ordered addition or bound answer; active append uses steer, not stop-and-recreate. |
| Continue: request and selected prior task | New goal is exactly request. Structured context carries prior `task_id`, goal, status, result, and relevant ordered additions; never concatenate them into semantic goal text. |
| Runner claim | Worker receives goal once, then distinct ordered additions, question-bound answers, and structured prior results. Do not repeat goal as `InitialMessage`/instructions. |
| Runtime observations | Control plane checks claim/version/session fences and evidence; [results](results.md) owns authoritative terminal delivery. |

Tasks retain identity, project, objective, source, executing entity, status, start/update times, result, errors, recovery information, and execution records. Pending, executing, dependency/event waiting, awaiting-user, paused, completed, failed, and canceled states need actual semantics. Interaction state is separate; `awaiting_user` compatibility does not create client authority or restore historical impact confirmation. Completion requires verification, not an agent saying “done.”

## Required behavior

### Admission and Worker context

- Every request, including child Worker work from Super or service triggers, uses controlled admission. Check authority, budget, and capacity before runtime creation; the runner cannot grant authority.
- Remove `Do not delegate or perform external irreversible changes.` from initial, resume, and rotation prompts. Workers use available tools/subagents as needed: simple work directly; independent substantial parts delegated. This removes a blanket prohibition, not granted-scope or effect-recovery boundaries; add no new policy gate.
- Preserve project dependencies, parallel/child relationships, shared-resource ownership, and conflict protection. Workers execute independently with scoped relevant context/tools; Super handles complex planning, dependencies, conflict, adjustment, and escalation, not every low-level event.
- Deterministic timers, explicit rules, state transitions, health, conversion, and routine event routing make no model calls. Semantic cases may invoke a Worker directly; complex coordination may invoke Super.

### Host OpenCode lifecycle

Each host has one dedicated host-installed OpenCode V2 service, `zoe-opencode.service`, owning `/var/lib/zoe/opencode`, port, password file, configuration, and service-only provider secrets. Runner restart must not kill sessions. The runner is an authenticated HTTP client. Create with `POST /api/session`, setting `location.directory` to private `<ZOE_OPENCODE_WORKSPACE_ROOT>/<run-id>`; record session ID in new journal rows (legacy container/password fields remain readable, not current runtime authority).

The resident Worker identity uses profile/agent ID `zoe-steward` and the `full` tier, allowing all actions/resources without approval prompts. Other profiles retain their configured server-derived tiers; runner translation must not widen them. Child sessions inherit parent permissions. OpenCode uses ordered last-match rules; inspect effective agent and session configuration rather than assuming one wildcard request fits every profile. OpenCode permissions are tool policy, not an OS sandbox: shell access cannot establish host-path isolation. The accepted operator baseline runs the service as root without filesystem unit hardening. Do not restore older runtime-container or blanket impact-confirmation requirements; this contract adds no mandatory gate.

Poll messages for idle outcome: `succeeded` is a result; `failed` includes error detail; `interrupted` is stopped; missing outcome is nonterminal. A prompt/transport acknowledgement is not completion. Completion/usage require provider-backed terminal evidence, including non-busy session and terminal latest assistant facts. Stop interrupts, then retries `/api/session/active` until exact session absence proves termination. Host-session reconciliation replaces runtime-container reconciliation; Docker-backed capability deployment remains separate.

### Capacity, budget, waiting, and rotation

- Shared `ZOE_MAX_RUNS` defaults to 12 active runs, including Super, Workers, and curator work. Waiting-user/uncertain sessions retain slots until fenced termination evidence permits release.
- `ZOE_WAIT_LIMIT_MINUTES` defaults to 30 minutes. Worker questions end the turn and keep the run while awaiting the user; a bound answer returns to that session. After the limit, terminate and release only on evidence; a later answer starts a new run continuing task messages/workspace files.
- Attribute every model call to authorized task/run, route, and budget. Reserve/settle usage idempotently; unknown usage remains reserved. Enforce provider-call, time, tool-loop/step, artifact-byte, and termination limits at actual effect boundaries. Because OpenCode has no per-run tool-step limit, runner observes events and aborts when limits are reached; contract fields alone are not enforcement evidence.
- Rotate long-lived execution context when latest assistant usage exceeds 50% of selected model `/api/model` `limit.context`. `ZOE_AGENT_CONTEXT_WINDOW_RATIO` defaults `0.5`, valid only in `(0,1)`. Unknown model window disables rotation for that run. Optional `ZOE_AGENT_CONTEXT_TOKEN_BUDGET` only lowers the threshold; claim `MaxContextTokens` may lower it further. In the new session, the earliest assistant by `(time.created, id)`, not API list order, at or above threshold ends with `context_threshold_below_baseline`, not repeated rotation. Recovery rebinds journal rotation chains.

### Services and installed capabilities

A finite task is not a durable service. Each service has identity, purpose, scope/project, environment, triggers, permissions, configuration, runtime status, latest execution, health/errors, and stop/recovery methods; each trigger creates a finite execution record. Services continue without clients/Super and have resource limits separate from agent-run capacity. Users must be able to inspect, configure, authorize, run, pause, resume, update, stop, remove, and audit installed capabilities. A vague grant cannot confer unlimited service authority.

Generated source/tests/release manifest pass capability-manager validation and applicable authorization before deployment; generated code is not deployment permission. Constrained manifests name immutable artifact/version, entrypoint, deployment-managed environment references, allowed mounts, and capabilities. Enforce user, mount, network, port, CPU, memory, process, and log limits; never allow privileged mode, arbitrary host mounts/devices, or Docker socket access. Trusted runner host/daemon authority is infrastructure trust, not workload isolation.

Separate temporary workspaces/caches, immutable releases, deployment configuration, and persistent service roots. Volumes record owner, purpose, mount, backup/restore, retention, and deletion. Workspace cleanup cannot remove deployed data or its only release artifact. Extensible mail/calendar/files/development/web/apps/system/schedule/monitoring capabilities remain product obligations, not claims of implemented connectors.

## Persistence and idempotency

Durably retain voice turn/content hash, command journal/receipt, stable turn-to-task linkage across retries/revisions, accepted classification/action/IDs/reply, and task/entry/question linkage. One authority transaction commits receipt, work, and outbox event before acceptance; admission rollback leaves no partial task/run/source linkage. Commands use `Idempotency-Key` and applicable `expected_version`; opaque workspace IDs are not host paths.

Claims use version/session fences. Stable entry identity and per-run delivery records cover additions/answers; initial/resume/rotation prompts mark included entries delivered. Checkpoints survive recovery/rotation, preventing duplicate initial goal or addition replay. Continue retains a durable prior-task relation/context and provenance. Preserve artifacts, model route, execution host, workspace, authorization, usage and result evidence beyond runtime/provider maintenance. Shared-workspace writes need explicit ownership/conflict protection.

## Failure and recovery

Reject stale/conflicting turn revisions. Unknown acceptance reconciles the original receipt, not a new command. Classification failure admits nothing; stale questions cannot receive answers. Reconcile append admission/delivery identities; missing/stale continuation facts re-evaluate Steward, not text-substitute admission.

Unknown create becomes `effect_unknown`, never automatic replay; session titles are not idempotency keys. Query/reconcile uncertain external effects when possible and require resolution otherwise, preserving accepted inputs, completed steps/results, external state, pending work, and recovery evidence. No blind irreversible-effect retries or slot release based only on quiet output. Preserve task/evidence history when accepting waiting-task messages. Client close, voice end, account change, and runtime pause/stop do not themselves prove task completion/cancellation.

Detect service exit, agent/API/network failures, timeout, permission/resource shortages, and conflicts. Recover only safely under policy; retain failure/context and source-linked notices for unrecoverable cases. [Results](results.md) owns notice semantics. [Configuration](configuration.md) owns deploy/restore safety.

## Implementation locations

[Voice handlers](../../../internal/voice/handlers.go), [handoff](../../../internal/voice/handoff_preflight.go), [commands](../../../internal/control/core_handlers.go), [task entries](../../../internal/control/task_entries.go), [claims](../../../internal/control/runner.go), [entry delivery](../../../internal/control/entry_deliveries.go), [Worker engine](../../../internal/runner/engine.go), [Linux deployment](../../../deploy/linux/).

## Verification obligations

Follow [evidence and acceptance](../../development.md#evidence-and-acceptance). Verify goal once, ordered exactly-once additions/answers across initial/live/resume/rotation, structured prior results, direct simple work, and needed tools/subagents. Verify creation uncertainty, crashes, session continuity, actual exact stop, capacity accounting, usage/streaming finish, real budget enforcement, rotation thresholds/baseline handling, permission limitations, workspace conflicts, capability lifecycle/data retention, and background task continuity on the deployed runtime binary. Adapter registration, source review, DTO limits, build, health, and runtime success are distinct evidence.

Cross-component journeys and quality obligations live in [development](../../development.md#product-verification-obligations). Optional native human handoff is not required runtime acceptance. Task retention and full Worker step-level narrative storage remain unresolved.
