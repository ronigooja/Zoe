# UI presentation and Mac screen recovery

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. Use A2UI presentations with the renderer/basic catalog and platform shell; retain settings and presence floors. UI_Controller produces server-driven presentations, not local-only composition. Do not introduce a new client UI authority or business-result source.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| UI route / display request / redisplay / authoritative result or question | UI_Controller receives full request(s), task state/result/question and current screen facts, never stored surfaces. No raw-utterance duplicate input. |
| UI_Controller presentation | Output carries A2UI content and ui-meta with container, required size, and mode-specific feedback. The server owns input semantics and task/question linkage. |
| Mac render acknowledgement: surface/document identity and revision, actual render outcome | Server validates matching current presentation; success feedback is allowed **only after render ACK**. |
| A2UI user input: bound task/question, surface revision, submitted value and command idempotency identity | Server routes directly to the bound task or pending question; no voice relay, register pass or semantic task reselection is required. |
| Conversation text input: command idempotency identity, full text. Producers are the menu-bar text window and an A2UI surface presented with `chat_input` | Server persists the command and delivers the text to the voice conversation as defined in [Results](results.md#user-text-and-input-feedback). No register pass, Steward, or task is involved. |
| ui.screen: current displays/visible bounds | UI_Controller plans against current screen facts; Mac uses consistent native style and clamps size/position to chosen screen's visible bounds. Direct user window actions retain precedence. |
| SSE: stable event identity/cursor, surface and result revision | Client deduplicates events, applies only current revisions and repairs state from authoritative snapshots on gaps/reconnect. Surface events and snapshot entries contain full retained documents, with no `deleted` field or deletion records. |

## Required behavior

The server selects `show`, `task_question` (a pending question_id), or
`chat_input` before calling UI_Controller, assembling the common prompt section
plus only the active mode section. Startup rejects missing or empty sections.
Mode is not duplicated in model input JSON.

Show requires nonempty feedback of at most 8000 bytes; input modes require a
bound TextField and submit event and store empty feedback. Model ui-meta never
selects input mode. Persisted requests, `ui.surface` SSE payloads and recovery
entries use server-owned booleans `task_question` and `chat_input`.

UI-only content requiring research must obtain task execution through the task path; UI_Controller does no research or invented result generation. Redisplay invokes no Worker and remains UI-only; it does not authorize voice playback. A stale surface is not the current task result. Neither a report nor a UI summary replaces the authoritative result. Screen input can complete while voice is unavailable. Voice/UI completion acknowledgements are separate evidence.

`chat_input` is true only for text typed for the conversation and never together with `task_question`. A `chat_input` surface binds no task or question. Stale surface or revision input is rejected as for task input. The menu-bar text window is a platform-shell entry, not server-driven content, and is available whether or not a call is active.

## Persistence and idempotency

- Durable request/job with source and result/question identity, current surface revision and delivery state.
- Create new surfaces only, with server-assigned identities rewritten into every message; retain document revision, originating request/result IDs and input linkage only while needed.
- Durable ACK and feedback dedupe by presentation identity/revision.
- Durable command receipt and task/question entry; preserve submitted full value.
- Delete non-input documents after their revision-bound render ACK, success or failure. Retain task-question and conversation-text input documents while their binding is current; delete them after input or when the question becomes stale. Client-local History is independent.
- Durable/recoverable cursor and presentation/result identities.
- Durable conversation-text command receipt and pending text keyed by the command identity. Duplicate submission neither duplicates delivery nor overwrites the stored text.

## Failure and recovery

Generation/dispatch is not display success; retain an unacknowledged presentation, including while the client is offline. Rejected/failed/stale ACK does not announce success; reconnect repairs still-retained state without duplicate presentation/feedback.

Validate ownership, current pending state, binding/revision and idempotency; stale/duplicate input cannot answer another question or create another execution. Missing/stale screen facts cannot establish current presentation; failed rendering is not success feedback. Replayed SSE cannot reopen obsolete results, duplicate actions, or reset delivered state.

## Implementation locations

[UI commands](../../../internal/voice/ui_commands.go), [UI jobs](../../../internal/voice/ui_jobs.go), [UI feedback](../../../internal/voice/ui_feedback.go), [UI controller](../../../internal/uictl/), [Apple client](../../../clients/apple/Packages/ZoeKit/Sources/).

## Verification obligations

Verify the same authoritative result for voice and screen, no Worker for redisplay, render-ACK-gated feedback, direct bound input while voice is unavailable, stale/duplicate input and stale-surface rejection. Verify menu-bar submission with and without an active call, `chat_input` surface submission, duplicate and stale input, and that `chat_input` never reaches a task. Verify consistent style, screen clamp, render failures/ACK, SSE replay/gap repair, native accessibility and real device interaction. Valid-activity-only silence verification is defined in [Media](media.md#verification-obligations). Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
