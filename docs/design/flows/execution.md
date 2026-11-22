# Voice handoff and execution

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. Downstream execution uses the retained [register request](register.md); task selection follows [Steward](steward.md).

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| AVAS client-targeted delegation | The gateway marks the user turn through [transcript ingest](#transcript-ingest). |
| Durable delegated turn | The Zoe worker routes it through [Register](register.md) and applies the outcome. |
| Task route: retained request, display_requested, retrieved facts | Steward decision, then transactional admission/append/answer/cancel/report/redisplay. |
| New admission: full request | Task goal becomes Worker goal; source reference is metadata, not another initial user instruction. |
| Append/answer: full request, task_id and question_id for answers | Original live OpenCode session receives ordered additional request or bound answer; active append uses steer, not stop-and-recreate. |
| Continue: full request plus selected prior task | New task goal is exactly the request; carry prior task_id, goal, status, result and relevant ordered additions as structured context, not concatenated semantic goal text. |
| Runner claim: goal, ordered additions, question-bound answers, structured prior results | Worker receives the goal once, then these distinct context types in order. Do not repeat the goal as InitialMessage/instructions. |

## Required behavior

The existing voice ingest runner credential is sent as `Authorization: Bearer
<runner token>` over HTTPS. Its `voice:turn:ingest` scope and workspace
allowlist authorize only `POST /runner/v1/voice/turns` and the three
[lifecycle POST routes](media.md#gateway-lifecycle-reports-to-zoe), not a
path prefix or any execution route. No new scope. For `call.start`, check
the workspace from the validated instance key; for other calls and turns,
check the workspace from the matched Zoe session. Zoe derives owner, device,
session and generation from its records, never from caller-supplied actor
fields. Client commands `voice.text_input`, `ui.screen`, `ui.rendered` and
`ui.action` are unchanged.

One Zoe-internal worker is the sole classification trigger. It runs Register
then Steward when required, outside the applying transaction, and applies the
outcome transactionally as the session's recorded owner and device, not as the
gateway runner. Task ownership and creator attribution remain with the caller
and their device. A turn accepted while active is still executed if the call
ends before the worker applies it. A delegation never attaches to a replacement
session or generation.

For initial, resume and rotation prompts, remove `Do not delegate or perform external irreversible changes.` Workers use available tools and subagents as needed: simple work is performed directly; independent substantial parts are delegated. This removes a blanket prohibition, not existing task authority or effect-recovery boundaries. Do not add new policy gates.

### Transcript ingest

| Endpoint | Request | Committed response |
|---|---|---|
| Zoe `POST /runner/v1/voice/turns` | `gateway_session_id`, positive integer `media_generation`, `turns` (1–200). Each turn has nonnegative integer `seq` (gateway-assigned for the session), `role` (`user` or `assistant`), `text` (string, at most 64 KiB UTF-8), `at` (RFC3339 timestamp, optional fractional seconds), and optional `delegation_id`. The complete JSON body is at most 512 KiB; `received_at` is server-owned, not sent. | `200 {accepted,duplicates}`: integer counts of newly stored and already stored transcript rows. Every marked turn has both a durable transcript row and durable `voice_turns` row, even if counted as a transcript duplicate. No execution-completion guarantee. |

Only an AVAS `delegation.created` with `item.target = "client"` marks a
turn: send the upstream `item.id` unchanged as `delegation_id`, a string of
1–256 Unicode code points, on a `user` turn with nonblank text. Omit the
field for ordinary transcript. The gateway does not inject
`user_audio_turn.final`; the upstream delegation itself authorizes execution
without `user_confirmed`. Reuse the transcript sequence for the same turn if
already forwarded; do not suppress its delegation mark as a transcript
duplicate. Transcript ingest retains the late-arrival window through 10
minutes after `ended_at`, for the original established binding only.

Use the [voice error envelope](media.md#inputs-and-outputs). For delegation,
terminal errors are `400 invalid_request`, `401 unauthenticated`, `403 forbidden`,
`404 not_found`, `409 voice_generation_stale` (stale media generation),
`409 transcript_window_closed`, `409 transcript_conflict`
(sequence reused with different text), `409 turn_revision_conflict` (delegation
identity reused with different text), and `413 transcript_too_large`. Other
4xx or redirects are terminal too. Only network errors and HTTP 5xx (including
503) are retryable. No partial batch commit on rejection.

## Persistence and idempotency

- In the same transaction as transcript storage, Zoe creates the delegated
  `voice_turns` row with the Zoe `session_id` and `generation`, upstream item ID
  as `turn_id`, `revision = 1`, `content_hash` of the turn text, and `confirmed_at`
  set to Zoe's receive time. Retries use both transcript `(session_id, seq)` and
  turn `(session_id, generation, turn_id, revision)` identities. If a matching
  transcript row exists but the delegation row does not, insert the delegation
  row in the same transaction. Content-hash and revision checks stay server-side.
- New admission: task/run linkage and claim fence; initial goal is delivered once.
- Append/answer: stable entry identity and per-run delivery record. Initial/resume/rotation prompts mark included entries delivered.
- Continue: new task identity plus durable prior-task relation/context and provenance.
- Runner: claim/version/session fences and entry delivery checkpoints survive recovery/rotation.

## Failure and recovery

- Flush marked turns immediately without the 500 ms batching wait; do not evict
  them on queue overflow or truncate delegation text to fit. Retry marked
  turns and `call.established`/`call.ended` in memory for at most 10 seconds
  with exponential backoff and jitter, only on network errors and HTTP 5xx.
  Forwarding is gated by [call establishment](media.md#call-lifecycle).
- On terminal failure or expiry, the gateway appends a hand-off-failed note
  upstream through the existing AVAS context-append mechanism; see the
  [delivery exception](results.md#delivery-paths). The turn then follows the
  ordinary transcript path, with its normal long retry for retryable failures;
  404 and 409 are terminal on that path too. No persistent outbox, automatic
  compensation or deduplication beyond the stated identities. Duplicate
  execution after a user-visible failure is a fault to investigate.
- Classification failures follow [Register failure rules](register.md#failure-and-recovery).
  The worker handles stale decisions under [Steward revalidation](steward.md#failure-and-recovery).
- New admission: rollback leaves no partial admission/link.
- Append/answer: retry reconciles admission/delivery identity; stale questions cannot receive answers.
- Continue: missing/stale source facts re-evaluate selection; no text-based substitute admission.
- Runner: preserve accepted inputs and uncertain external-effect handling on restart; no duplicate goal/addition replay.

## Release

Follow the [unified voice release](media.md#release).

## Implementation locations

[Transcript ingest](../../../internal/voice/transcript.go), [voice handlers](../../../internal/voice/handlers.go), [task commands](../../../internal/control/core_handlers.go), [runner claims](../../../internal/control/runner.go), [entry delivery](../../../internal/control/entry_deliveries.go), [Worker engine](../../../internal/runner/engine.go).

## Verification obligations

Verify atomic ingest, identity conflicts, caller attribution, post-hangup execution without rebinding, and visible failure with transcript-only degradation. Verify one classification trigger and no fallback admission. Verify goal once, ordered exactly-once additions/answers across initial/live/resume/rotation, structured continuation result context, direct simple work and needed tool/subagent use. Verify preserved full request at all execution branches. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
