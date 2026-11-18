# UI presentation and Mac screen recovery

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. Use A2UI presentations with the renderer/basic catalog and platform shell; retain settings and presence floors. UI_Controller produces server-driven presentations, not local-only composition. Do not introduce a new client UI authority or business-result source.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| UI route / display request / redisplay / authoritative result or question | UI_Controller receives full request(s), task state/result/question, current surfaces and current screen facts. No raw-utterance duplicate input. |
| UI_Controller presentation | Output carries A2UI content, outer container choice (popover/window), optional size, optional awaiting-input linkage to task_id/question_id, and feedback. Use the renderer/catalog and platform shell. |
| Mac render acknowledgement: surface/document identity and revision, actual render outcome | Server validates matching current presentation; success feedback is allowed **only after render ACK**. |
| A2UI user input: bound task/question, surface revision, submitted value and command idempotency identity | Server routes directly to the bound task or pending question; no voice relay, register pass or semantic task reselection is required. |
| ui.screen and surface state: current displays/visible bounds, surfaces and revisions | UI_Controller plans against current facts; Mac uses consistent native style and clamps size/position to chosen screen's visible bounds. Direct user window actions retain precedence. |
| SSE: stable event identity/cursor, surface and result revision | Client deduplicates events, applies only current revisions and repairs state from authoritative snapshots on gaps/reconnect. |

## Required behavior

UI-only content requiring research must obtain task execution through the task path; UI_Controller does no research or invented result generation. Redisplay invokes no Worker and remains UI-only; it does not authorize voice playback. A stale surface is not the current task result. Neither a report nor a UI summary replaces the authoritative result. Screen input can complete while voice is unavailable. Voice/UI completion acknowledgements are separate evidence.

## Persistence and idempotency

- Durable request/job with source and result/question identity, current surface revision and delivery state.
- Persist surface/document revision, originating request/result IDs and awaited-input linkage.
- Durable ACK and feedback dedupe by presentation identity/revision.
- Durable command receipt and task/question entry; preserve submitted full value.
- Surface identity/revision and authoritative desired state; render ACK as above.
- Durable/recoverable cursor and presentation/result identities.

## Failure and recovery

Generation/dispatch is not display success; retain a pending presentation until render acknowledgement. Rejected/failed/stale ACK does not announce success; reconnect repairs missing state without duplicate presentation/feedback.

Validate ownership, current pending state, binding/revision and idempotency; stale/duplicate input cannot answer another question or create another execution. Missing/stale screen facts cannot establish current presentation; failed rendering is not success feedback. Replayed SSE cannot reopen obsolete results, duplicate actions, or reset delivered state.

## Implementation locations

[UI commands](../../../internal/voice/ui_commands.go), [UI jobs](../../../internal/voice/ui_jobs.go), [UI feedback](../../../internal/voice/ui_feedback.go), [UI controller](../../../internal/uictl/), [Apple client](../../../clients/apple/Packages/ZoeKit/Sources/).

## Verification obligations

Verify the same authoritative result for voice and screen, no Worker for redisplay, render-ACK-gated feedback, direct bound input while voice is unavailable, stale/duplicate input and stale-surface rejection. Verify consistent style, screen clamp, render failures/ACK, SSE replay/gap repair, native accessibility and real device interaction. Valid-activity-only silence verification is defined in [Media](media.md#verification-obligations). Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
