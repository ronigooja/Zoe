# Voice handoff and execution

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. Downstream execution uses the retained [register request](register.md); task selection follows [Steward](steward.md).

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| AVAS `delegation.created` with `item.target = "client"` | Gateway sends the user turn via `POST /runner/v1/voice/turns`. The delegation mark carries upstream `item.id`; ordinary transcript turns do not trigger execution. |
| Durable delegated turn | The Zoe worker routes it through [Register](register.md) and applies the outcome. |
| Task route: retained request, display_requested, retrieved facts | Steward decision, then transactional admission/append/answer/cancel/report/redisplay. |
| New admission: full request | Task goal becomes Worker goal; source reference is metadata, not another initial user instruction. |
| Append/answer: full request, task_id and question_id for answers | Original live OpenCode session receives ordered additional request or bound answer; active append uses steer, not stop-and-recreate. |
| Continue: full request plus selected prior task | New task goal is exactly the request; carry prior task_id, goal, status, result and relevant ordered additions as structured context, not concatenated semantic goal text. |
| Runner claim: goal, ordered additions, question-bound answers, structured prior results | Worker receives the goal once, then these distinct context types in order. Do not repeat the goal as InitialMessage/instructions. |

## Required behavior

The transcript channel requires the authenticated runner principal and
`voice:turn:ingest` in the bound workspace; no new scope is required. Zoe
derives the workspace, owner, device, Zoe session and Zoe generation from the
`voice_sessions` binding identified by `gateway_session_id` and
`media_generation`, not from caller-supplied identities. The upstream
delegation authorizes execution without a client `user_confirmed` assertion.
The gateway no longer injects `user_audio_turn.final` and the Apple client no
longer relays voice turns. Client commands `voice.text_input`, `ui.screen`,
`ui.rendered` and `ui.action` are unchanged.

One Zoe-internal worker is the sole classification trigger. It runs Register
then Steward when required, outside the applying transaction, and applies the
outcome transactionally as the session's recorded owner and device, not as the
gateway runner. Task ownership and creator attribution remain with the caller
and their device. No client-held handoff request, `prepareHandoff` wait or
client 409 handling is needed for direct delegation. A turn accepted while
active is still executed if the call ends before the worker applies it. The
transcript endpoint retains its late-arrival window through 10 minutes after
`ended_at`. A delegation never attaches to a replacement session or generation.

For initial, resume and rotation prompts, remove `Do not delegate or perform external irreversible changes.` Workers use available tools and subagents as needed: simple work is performed directly; independent substantial parts are delegated. This removes a blanket prohibition, not existing task authority or effect-recovery boundaries. Do not add new policy gates.

## Persistence and idempotency

- In the same transaction as transcript storage, Zoe creates the delegated
  `voice_turns` row with the Zoe `session_id` and `generation`, upstream item ID
  as `turn_id`, `revision = 1`, `content_hash` of the turn text, and `confirmed_at`
  set to Zoe's receive time. Retries use both transcript `(session_id, seq)` and
  turn `(session_id, generation, turn_id, revision)` identities. Same identity
  with different text conflicts; content-hash and revision checks stay server-side.
- New admission: task/run linkage and claim fence; initial goal is delivered once.
- Append/answer: stable entry identity and per-run delivery record. Initial/resume/rotation prompts mark included entries delivered.
- Continue: new task identity plus durable prior-task relation/context and provenance.
- Runner: claim/version/session fences and entry delivery checkpoints survive recovery/rotation.

## Failure and recovery

- Flush marked turns immediately without the 500 ms batching wait; do not evict
  them on queue overflow. Retry for at most 10 seconds with exponential
  backoff and jitter, only on network errors and HTTP 5xx (including 503).
  HTTP 400, 401, 403, 404, 409 and 413 are terminal. The
  [atomic media binding](media.md#required-behavior) prevents a partially
  written binding from being observed by ingest.
- On terminal failure or expiry, the gateway appends a hand-off-failed note
  upstream through the existing AVAS context-append mechanism. This is a
  gateway notice, not a Zoe result delivery. The turn then follows the normal
  long retry as an ordinary transcript turn. No persistent outbox, automatic
  compensation or deduplication beyond the stated identities. Duplicate
  execution after a user-visible failure is a fault to investigate.
- Classification failures follow [Register failure rules](register.md#failure-and-recovery).
  The worker handles stale decisions under [Steward revalidation](steward.md#failure-and-recovery).
- New admission: rollback leaves no partial admission/link.
- Append/answer: retry reconciles admission/delivery identity; stale questions cannot receive answers.
- Continue: missing/stale source facts re-evaluate selection; no text-based substitute admission.
- Runner: preserve accepted inputs and uncertain external-effect handling on restart; no duplicate goal/addition replay.

## Rollout order

1. Ship Zoe's atomic media binding, transactional delegation ingest and worker
   first. Keep `voice.turn.commit` and `voice.turn.handoff` for the installed client.
2. After Zoe accepts delegation turns, ship gateway marking and immediate
   flush, and stop client injection.
3. Ship the client without the relay path, then remove the old server commands.

## Implementation locations

[Transcript ingest](../../../internal/voice/transcript.go), [voice handlers](../../../internal/voice/handlers.go), [task commands](../../../internal/control/core_handlers.go), [runner claims](../../../internal/control/runner.go), [entry delivery](../../../internal/control/entry_deliveries.go), [Worker engine](../../../internal/runner/engine.go).

## Verification obligations

Verify atomic ingest, identity conflicts, caller attribution, post-hangup execution without rebinding, and visible failure with transcript-only degradation. Verify one classification trigger and no fallback admission. Verify goal once, ordered exactly-once additions/answers across initial/live/resume/rotation, structured continuation result context, direct simple work and needed tool/subagent use. Verify preserved full request at all execution branches. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
