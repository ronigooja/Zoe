# Voice handoff and execution

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. Downstream execution uses the retained [register request](register.md); task selection follows [Steward](steward.md).

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Gateway final: turn_id, revision, transcript; client adds session_id, generation, content, user_confirmed | Commit validates current identity/revision and content; server retains original provenance, not a later execution fallback. |
| Client handoff: session_id, generation, turn_id, revision, content_hash | Server checks committed source and classifies outside the authority transaction; handoff contains no client-authored goal. |
| Register output and source identity | Server persists request and routes, sends only request to route consumers; original remains separately linked provenance. |
| Task route: retained request, display_requested, retrieved facts | Steward decision, then transactional admission/append/answer/cancel/report/redisplay. |
| New admission: full request | Task goal becomes Worker goal; source reference is metadata, not another initial user instruction. |
| Append/answer: full request, task_id and question_id for answers | Original live OpenCode session receives ordered additional request or bound answer; active append uses steer, not stop-and-recreate. |
| Continue: full request plus selected prior task | New task goal is exactly the request; carry prior task_id, goal, status, result and relevant ordered additions as structured context, not concatenated semantic goal text. |
| Runner claim: goal, ordered additions, question-bound answers, structured prior results | Worker receives the goal once, then these distinct context types in order. Do not repeat the goal as InitialMessage/instructions. |

## Required behavior

For initial, resume and rotation prompts, remove `Do not delegate or perform external irreversible changes.` Workers use available tools and subagents as needed: simple work is performed directly; independent substantial parts are delegated. This removes a blanket prohibition, not existing task authority or effect-recovery boundaries. Do not add new policy gates.

## Persistence and idempotency

- Commit: durable voice turn, content hash, command journal and receipt.
- Handoff: stable turn-to-task linkage and receipt prevent duplicate tasks across retries/revisions.
- Classification: durable classification and selected action/IDs/reply; replay reuses accepted classification.
- Task route: keep source identity, decision, selected stable IDs and resulting task/entry/question linkage.
- New admission: task/run linkage and claim fence; initial goal is delivered once.
- Append/answer: stable entry identity and per-run delivery record. Initial/resume/rotation prompts mark included entries delivered.
- Continue: new task identity plus durable prior-task relation/context and provenance.
- Runner: claim/version/session fences and entry delivery checkpoints survive recovery/rotation.

## Failure and recovery

- Commit: stale/conflicting revisions are rejected; unknown acceptance reconciles the original receipt.
- Handoff: classification failure admits nothing; never bypass register/Steward on timeout. Invalid/partial register output creates no fallback task or memory operation.
- Task route: stale decisions are refreshed; rejected or ambiguous selections do not become new tasks.
- New admission: rollback leaves no partial admission/link.
- Append/answer: retry reconciles admission/delivery identity; stale questions cannot receive answers.
- Continue: missing/stale source facts re-evaluate selection; no text-based substitute admission.
- Runner: preserve accepted inputs and uncertain external-effect handling on restart; no duplicate goal/addition replay.

## Implementation locations

[Voice handlers](../../../internal/voice/handlers.go), [register](../../../internal/voice/register.go), [handoff preflight](../../../internal/voice/handoff_preflight.go), [candidate retrieval](../../../internal/voice/taskcandidates.go), [Steward](../../../internal/steward/choose.go), [task commands](../../../internal/control/core_handlers.go), [runner claims](../../../internal/control/runner.go), [entry delivery](../../../internal/control/entry_deliveries.go), [Worker engine](../../../internal/runner/engine.go).

## Verification obligations

Verify goal once, ordered exactly-once additions/answers across initial/live/resume/rotation, structured continuation result context, direct simple work and needed tool/subagent use. Verify preserved full request at all execution branches. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
