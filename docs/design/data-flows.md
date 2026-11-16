# Approved target data flows

Status: **APPROVED TARGET, 2026-11-16; not a claim of current source implementation, deployment, or acceptance.** When implementing or reviewing the flows below, this document is the canonical field and behavior contract. Existing source links identify implementation locations, not conformance. [ADR 0013](../architecture/decisions/0013-context-register.md) records the decision; the [master plan](../master-plan.md) owns delivery status and the [evidence register](../acceptance/evidence.md#evidence-categories-and-acceptance-boundary) owns verification categories.

For these flows, this target supersedes older restatement/`none`, sentence-based task selection, completed-goal concatenation, non-checkpointed delivery, and local-only screen-composition decisions. The [screen contract](screen-document.md#protocol-and-renderer) still owns A2UI version, basic catalog, renderer, platform shell, settings and presence floors; its server-document prohibition and undecided server-push option do not apply to the approved UI_Controller flow here. No additional impact-confirmation or delegation prohibition is introduced.

## 1. Canonical model boundaries

### Register

Input is the owner's compact context, contiguous recent raw dialogue, and current original utterance. The register makes one routing call, without task management or memory search. Its exact output schema is:

```typescript
type RegisterOutput = {
  routes: Array<"chat" | "memory_write" | "recall" | "task" | "ui">;
  request: string | null;
};
```

`routes` is nonempty and distinct. `chat`, `memory_write`, and `recall` are each exclusive; `task` and `ui` may occur alone or together. For `chat`, `request` is null. For every other route, `request` is a nonempty, complete, self-contained statement of the user's entire request, resolving conversational references without adding intent. It is required even when the original utterance was already complete. Literal `none`, omitted context, and fallback to the original utterance are forbidden. There is no one-sentence or verbatim-quotation restriction that could truncate a complete request.

| Route | Semantic responsibility and consumer |
|---|---|
| `chat` | Conversation requiring no execution, screen action or memory operation; end delegated handling without Steward, task admission, memory search, or duplicate Zoe speech. Upstream voice answers conversation. |
| `memory_write` | Save or update personal information; send the full request to the memory worker. |
| `recall` | Retrieve personal memory; send the full request to scoped search. |
| `task` | Perform, modify, answer, continue, inspect, cancel or redisplay work; send the request and candidate facts to Steward. |
| `ui` | Present content or collect input on the screen; send the full request to UI_Controller. With `task`, also set display_requested for Steward/task delivery. |

The server retains the register request as the downstream execution input. The original utterance is retained only as register input, provenance, dialogue and asynchronous memory-ingestion source; it is not a second downstream execution input. Request derivation does not expand the user's authority. Strict parsing checks shape and nullability; semantic completeness and fidelity also need model-quality acceptance, not merely parser tests.

### Steward

The server retrieves owner/workspace-scoped task facts, preserving full goals, pending questions, results and retained additions. Titles, quoted original utterances and per-call aliases are not selectors. The exact input and output schemas are:

```typescript
type StewardInput = {
  request: string;
  display_requested: boolean;
  tasks: Array<{
    task_id: string;
    goal: string;
    status: string;
    pending_questions: Array<{ question_id: string; text: string }>;
    result: string | null;
    additions: string[];
  }>;
};
type StewardOutput = {
  action: "new" | "append" | "answer" | "continue" | "report" |
          "cancel" | "redisplay" | "clarify";
  task_id: string | null;
  question_id: string | null;
  reply: string | null;
};
```

Every key is present; irrelevant fields are null. IDs are real stable server-owned identities from the supplied facts. Steward returns a decision, never echoes or rewrites the request. The server retains and dispatches that full request. `status` reflects authoritative task state; `result` is null when absent; additions remain in conversational order. Pending-question IDs survive retries and are scoped to the selected task.

| Action | Semantic choice | Required non-null output beyond action | Server effect using retained request |
|---|---|---|---|
| `new` | Independent work not dependent on an existing task. | None | Admit a new task whose goal is the full request. |
| `append` | Additional direction for existing live work. | task_id | Persist the full request as an ordered addition and deliver to the original live session, without replacement work. |
| `answer` | Response to a currently pending question of existing work. | task_id, question_id | Bind the full request as the answer to that exact pending question. |
| `continue` | New work dependent on prior work rather than an addition to its live execution. | task_id | Admit a new task with the full request as goal and structured prior-task context. |
| `report` | Request for factual state, progress, result or handling of identified work. | task_id, reply | Deliver the fact-grounded reply; do not execute or mutate the task. |
| `cancel` | Withdrawal or stopping of identified work. | task_id | Apply cancellation to that task, preserving actual termination semantics. |
| `redisplay` | Presentation of an existing authoritative result with no new work. | task_id | Send that result and request to UI_Controller; invoke no Worker. |
| `clarify` | Intent or task/question reference is insufficiently determined. | reply | Ask the resolving question without admitting work. |

Steward chooses semantically using these definitions and supplied context, not examples, keyword rules or newest-task heuristics. An empty candidate set does not imply `new`: unresolved references or intent require `clarify`. Ambiguity requires clarification instead of arbitrary selection. A report must identify a real supplied task; absence of a resolvable subject uses clarification, not an invented report task. Replies for report/clarify are nonempty and follow the request's language.

Before applying a decision, the server validates ownership, selected IDs, eligibility and current pending state transactionally. If facts changed, refresh and re-evaluate the selection; never silently convert it into new work. Invalid model output or an unavailable model fails without admitting work. Existing processing-error behavior may report failure; it is not fallback authorization.

## 2. Voice handoff and execution

| Producer / fields | Consumer and transformation | Persistence / idempotency | Failure / recovery |
|---|---|---|---|
| Gateway final: turn_id, revision, transcript; client adds session_id, generation, content, user_confirmed | Commit validates current identity/revision and content; server retains original provenance, not a later execution fallback. | Durable voice turn, content hash, command journal and receipt. | Stale/conflicting revisions are rejected; unknown acceptance reconciles the original receipt. |
| Client handoff: session_id, generation, turn_id, revision, content_hash | Server checks committed source and classifies outside the authority transaction; handoff contains no client-authored goal. | Stable turn-to-task linkage and receipt prevent duplicate tasks across retries/revisions. | Classification failure admits nothing; never bypass register/Steward on timeout. |
| Register output (§1) and source identity | Server persists request and routes, sends only request to route consumers; original remains separately linked provenance. | Classification and selected action/IDs/reply are durable; replay reuses accepted classification. | Invalid/partial output creates no fallback task or memory operation. |
| Task route: retained request, display_requested, retrieved facts | Steward decision (§1), then transactional admission/append/answer/cancel/report/redisplay. | Keep source identity, decision, selected stable IDs and resulting task/entry/question linkage. | Stale decisions are refreshed; rejected or ambiguous selections do not become new tasks. |
| New admission: full request | Task goal becomes Worker goal; source reference is metadata, not another initial user instruction. | Task/run linkage and claim fence; initial goal is delivered once. | Rollback leaves no partial admission/link. |
| Append/answer: full request, task_id and question_id for answers | Original live OpenCode session receives ordered additional request or bound answer; active append uses steer, not stop-and-recreate. | Stable entry identity and per-run delivery record. Initial/resume/rotation prompts mark included entries delivered. | Retry reconciles admission/delivery identity; stale questions cannot receive answers. |
| Continue: full request plus selected prior task | New task goal is exactly the request; carry prior task_id, goal, status, result and relevant ordered additions as structured context, not concatenated semantic goal text. | New task identity plus durable prior-task relation/context and provenance. | Missing/stale source facts re-evaluate selection; no text-based substitute admission. |
| Runner claim: goal, ordered additions, question-bound answers, structured prior results | Worker receives the goal once, then these distinct context types in order. Do not repeat the goal as InitialMessage/instructions. | Claim/version/session fences and entry delivery checkpoints survive recovery/rotation. | Preserve accepted inputs and uncertain external-effect handling on restart; no duplicate goal/addition replay. |

For initial, resume and rotation prompts, remove `Do not delegate or perform external irreversible changes.` Workers use available tools and subagents as needed: simple work is performed directly; independent substantial parts are delegated. This removes a blanket prohibition, not existing task authority or effect-recovery boundaries. Do not add new policy gates.

Implementation locations: [voice handlers](../../internal/voice/handlers.go), [register](../../internal/voice/register.go), [handoff preflight](../../internal/voice/handoff_preflight.go), [candidate retrieval](../../internal/voice/taskcandidates.go), [Steward](../../internal/steward/choose.go), [task commands](../../internal/control/core_handlers.go), [runner claims](../../internal/control/runner.go), [entry delivery](../../internal/control/entry_deliveries.go), [Worker engine](../../internal/runner/engine.go).

## 3. Authoritative results, voice and UI

| Producer / fields | Consumer and transformation | Persistence / idempotency | Failure / recovery |
|---|---|---|---|
| Worker terminal observation: task/run identity, assistant result, evidence | Control plane validates fences and commits authoritative result/event. Voice and UI consume the same result text and stable identity. | Durable task result/assistant entry and event, independently tracked consumer progress. | Transport acknowledgement or model wording is not task success; stale observations cannot overwrite current authority. |
| Result event, stable result identity, full text | Voice delivery appends unchanged original text; report replies and recall texts use the same transport without a separate answer model. | Durable result delivery and acknowledged chunk checkpoint keyed by result, destination/session generation and chunk identity. | No active call leaves delivery pending for contextual delivery on the next call: not dropped and not indiscriminate playback of all history. Reconcile chunk acknowledgements and resume only unacknowledged chunks. |
| Voice text chunks | Gateway/upstream context consumer; validate UTF-8/nonblank text, preserve whitespace/control characters and full content; use sequential UTF-8-boundary-safe chunks within existing 8000-byte append limit. | Stable chunk IDs/acknowledged progress prevent prefix repetition after partial delivery. | Stop on failure and resume from checkpoint; an HTTP append alone does not prove spoken delivery. |
| UI route / display request / redisplay / authoritative result or question | UI_Controller receives full request(s), task state/result/question, current surfaces and current screen facts. No raw-utterance duplicate input. | Durable request/job with source and result/question identity, current surface revision and delivery state. | UI-only content requiring research must obtain task execution through the task path; UI_Controller does no research or invented result generation. A stale surface is not the current task result. |
| UI_Controller presentation | Output carries A2UI content, outer container choice (popover/window), optional size, optional awaiting-input linkage to task_id/question_id, and feedback. Use the screen contract's renderer/catalog and platform shell. | Persist surface/document revision, originating request/result IDs and awaited-input linkage. | Generation/dispatch is not display success; retain a pending presentation until render acknowledgement. |
| Mac render acknowledgement: surface/document identity and revision, actual render outcome | Server validates matching current presentation; success feedback is allowed **only after render ACK**. | Durable ACK and feedback dedupe by presentation identity/revision. | Rejected/failed/stale ACK does not announce success; reconnect repairs missing state without duplicate presentation/feedback. |
| A2UI user input: bound task/question, surface revision, submitted value and command idempotency identity | Server routes directly to the bound task or pending question; no voice relay, register pass or semantic task reselection is required. | Durable command receipt and task/question entry; preserve submitted full value. | Validate ownership, current pending state, binding/revision and idempotency; stale/duplicate input cannot answer another question or create another execution. |

Pending next-call delivery must retain source/task/result identity and delivery state so relevance and already-delivered content can be determined from current conversation context. It must not replace the authoritative result with a UI summary. Screen input can complete while voice is unavailable. Voice/UI completion acknowledgements are separate evidence.

Implementation locations: [result delivery](../../internal/voice/result_delivery.go), [UI commands](../../internal/voice/ui_commands.go), [UI jobs](../../internal/voice/ui_jobs.go), [UI feedback](../../internal/voice/ui_feedback.go), [UI controller](../../internal/uictl/), [Apple client](../../clients/apple/Packages/ZoeKit/Sources/).

## 4. Memory and continuous conversation coverage

| Producer / fields | Consumer and transformation | Persistence / idempotency | Failure / recovery |
|---|---|---|---|
| Explicit memory_write: full register request and owner/workspace scope | Worker searches existing memories, selects add/replace/none, validates operations and persists in Mem0/Qdrant. No original-utterance duplicate execution input or Steward task. | Durable write job/operation identity and outcome; successful write is silent. | Never replay an uncertain write blindly; preserve known applied progress and distinguish uncertainty from success. |
| Recall: full register request and owner/workspace scope | Worker scoped search returns stored texts; deliver those texts unchanged to voice. No task, answer model, second register round or fixed not-found speech. | Durable search/delivery linkage and chunk progress (§3). | Empty retrieval appends nothing; search failure is not “no memory” and must not fabricate content. |
| Scheduled batch: both-role original transcripts and completed-task source/request/result records in a fixed owner interval | Memory worker ingests chronological chunks; Go does not extract facts. | Stable batch/chunk identity and idempotent worker application; persist successful chunk progress. Only complete success advances sole ingestion boundary M via CAS. | Resume unfinished chunks without duplicate writes; boundary does not advance over failure or unseen content. No new content makes no worker call. |
| Register compact/raw/live from original dialogue and M | Compact covers M to retained compact right edge; raw covers that edge to current input, then live current sentence. Rebuild compact from source, not incrementally. | One authoritative M in memory_ingest_state; retain prior compact and its coverage on failure. | Compact failure keeps raw coverage from the retained edge; no gap or premature source exclusion. Call opening reads persisted compact plus raw without waiting for a new model/ingestion call. |

Keep existing independently configurable register, compact and memory model consumers and existing cadence/payload limits unless explicitly changed. Original dialogue remains the asynchronous source even though downstream execution uses the register request. Existing crash-finalized at-most-once writes and repeated successful ingestion chunks do not establish target idempotency.

Implementation locations: [recall/write jobs](../../internal/voice/recall.go), [batch scheduler](../../internal/memory/batch.go), [worker HTTP client/schema](../../internal/memoryapi/), [memory worker](../../services/memory/person_memory.py), [register compaction](../../internal/voice/register_compact.go).

## 5. Voice media, leases and recovery

| Producer / fields | Consumer and transformation | Persistence / idempotency | Failure / recovery |
|---|---|---|---|
| Client create attempt: session, generation, request identity and pool source | Server/gateway attempt and outcome lookup remain correlated to the exact attempt; distinguish media generation from client generation. | Durable attempted creation, binding and attestation/evidence identity. | Timeout is unknown, not proof no media exists; retain held/quarantined resource. |
| Gateway never_established or released outcome with evidence | Recovery may clear a matching **unbound failed create** only when the evidence matches session, generation, request and pool source. | Persist matched outcome/evidence and idempotent release transition. | Never release a bound/replacement session using an old failed request; missing/conflicting evidence remains unknown/held. |
| Gateway media release/termination evidence | Server reconciles the exact bound media/lease; expiry or silence alone never proves release. | Durable evidence and fenced lease transition. | Unknown outcomes trigger automatic checks; retain held state while unresolved, without unsafe account reuse. |

The target retains existing valid new-evidence semantics rather than weakening quarantine. User-visible recovery reflects actual state; ending media does not stop a task. Implementation locations: [account media](../../internal/voice/account_media.go), [gateway HTTP](../../internal/voice/account_media_http.go), [service recovery](../../internal/voice/service.go), [ADR 0005](../architecture/decisions/0005-voice-task-and-lease-boundaries.md).

## 6. Mac screen, activity and event recovery

| Producer / fields | Consumer and transformation | Persistence / idempotency | Failure / recovery |
|---|---|---|---|
| ui.screen and surface state: current displays/visible bounds, surfaces and revisions | UI_Controller plans against current facts; Mac uses consistent native style and clamps size/position to chosen screen's visible bounds. Direct user window actions retain precedence. | Surface identity/revision and authoritative desired state; render ACK as in §3. | Missing/stale screen facts cannot establish current presentation; failed rendering is not success feedback. |
| SSE: stable event identity/cursor, surface and result revision | Client deduplicates events, applies only current revisions and repairs state from authoritative snapshots on gaps/reconnect. | Durable/recoverable cursor and presentation/result identities. | Replayed SSE cannot reopen obsolete results, duplicate actions, or reset delivered state. |
| Valid user/voice interaction activity | Silence timer ends voice after default 60 seconds without valid interaction; retain configurable timeout. | Track actual interaction activity separately from transport health/events. | Heartbeats, SSE, polling, retries, stale/duplicate events and render chatter do not indefinitely reset silence. |

Use [ADR 0008](../architecture/decisions/0008-ambient-screen-client.md) for native shell/platform conventions subject to §3's approved server-driven presentation override. Do not introduce a new client UI authority or business-result source.

## 7. Model configuration and deployment

| Producer / fields | Consumer and transformation | Persistence / idempotency | Failure / recovery |
|---|---|---|---|
| Operator model menu: selected provider/model and effort plus secret references | Probe the proposed configuration, then atomically save/reload the corresponding environment/routes, OpenCode provider/agent configuration and session selection. All Worker layers resolve the same model+effort. | Keep previous known configuration and applied version; never log credentials. | Probe/save/reload failure rolls back the whole change, not a partially selected route. Verify effective runtime/session settings rather than menu success alone. |
| Register, Compact, Steward, UI_Controller, memory and Worker consumers | Preserve separate model configurations; changes target the chosen consumer rather than silently changing another or falling back across consumers. | Record effective per-consumer configuration identity. | A working consumer does not prove another is configured; report truthful failure. |
| Release/deployment manifest, network configuration, key references and persistent data | Deployment retains existing network reachability, keys and data; gateway image-only update does not implicitly reset configuration or state. | Immutable release identity, retained configuration/data inventory and explicit backup/rollback references. | Reset and backup are explicit operations; never silently regenerate keys or destroy data to make startup pass. Health/startup success is not end-to-end success. |

Implementation locations: [operator configuration](../../cmd/zoe/configure.go), [menu/operations](../../cmd/zoe/operations.go), [runner](../../internal/runner/engine.go), [deployment](../../deploy/linux/), [voice deployment](../../deploy/voice/README.md), [deployment runbook](../operations/deployment.md). No external CLIProxyAPI configuration change is authorized by this design.

## 8. Acceptance requirements

When accepting an implementation, record source revision, environment, action, outcome and limits under the evidence categories linked above. Schema/fixture, source review, build, installation, health, render ACK and real user-visible end-to-end success are distinct. This document records design approval only; no tests, deployment or new acceptance are established.

- Register: exact schema/routes/nullability, complete contextual requests without `none` or original-input fallback; semantic fidelity on real contextual turns.
- Steward: every action's required/null fields, stable duplicate-title task IDs and pending-question IDs, semantic ambiguity/empty candidates, stale-selection re-evaluation, preserved full request at all execution branches.
- Worker: goal once, ordered exactly-once additions/answers across initial/live/resume/rotation, structured continuation result context, direct simple work and needed tool/subagent use.
- Results/UI: same authoritative result for voice and screen, no-call pending next-call contextual delivery, partial-chunk restart/dedupe, no Worker for redisplay, render-ACK-gated feedback, direct bound input while voice is unavailable, stale/duplicate input and stale-surface rejection.
- Memory/context: scoped full-request write/search, add/replace/none, unchanged recall text/no answer model, idempotent batch/chunk crash recovery and contiguous compact/raw coverage.
- Media: matching unbound failed-create never_established/released evidence, old-generation/pool-source rejection, bound-session protection, automatic unknown-state checks without premature release, real gateway reconnection and task continuity.
- Mac: consistent style, screen clamp, render failures/ACK, SSE replay/gap repair, valid-activity-only silence60, native accessibility and real device interaction under the screen contract.
- Configuration/deployment: probe/save/reload failures with atomic rollback, effective model+effort equality across Worker route/provider/agent/session, consumer separation, retained network/key/data and explicit reset/backup/restore evidence; real end-to-end task/result/voice/UI success separately from health.
