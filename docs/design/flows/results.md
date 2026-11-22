# Authoritative results and voice

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. Voice and [UI](screen.md) consume the same authoritative result text and stable identity. Neither a report nor a UI summary replaces the authoritative result. Voice/UI completion acknowledgements are separate evidence.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Worker terminal observation: task/run identity, assistant result, evidence | Control plane validates fences and commits authoritative result/event. |
| Result event, stable result identity, full text | Voice delivery appends unchanged original text; report replies and recall texts use the same transport without a separate answer model. |
| Voice text chunks | Gateway/upstream context consumer; validate UTF-8/nonblank text, preserve whitespace/control characters and full content; use sequential UTF-8-boundary-safe chunks within existing 8000-byte append limit. |

## Required behavior

Pending results retain their authoritative identity and are candidates for silent opening context, not compulsory playback. Opening takes Register compact/raw first, then up to 25 owner-scoped pending results ordered by creation time and entry ID, excluding reserved, consumed or previously attempted results. It shares one rune budget and does not truncate or reserve items that do not fit. Each result is a `user` item with four lines labeled `Task result:`, `Goal:`, `Status:` and `Result:`; delivery metadata stays private. A contextual `report` sends its fact-grounded reply through the durable reply path without queueing or acknowledging the original result. `redisplay` remains UI-only and does not authorize voice playback.

For production receipt delivery, Zoe probes authenticated `GET /v1/health` using the configured gateway URL, owner bearer and HTTP client before checkpoint transactions. Only the exact `result_deliveries` contract `{version:"v1",max_text_bytes:8000,applied_evidence:"local_transport_writes_and_durable_receipt"}` enables the receipt adapter for task results and generic durable texts. Capability evidence is cached for 30 seconds, with one probe in flight and a two-second probe budget. No new deployment setting is required.

Production discovery starts the result dispatcher only with a configured HTTPS gateway and owner key; it is independent of account-media enablement and register configuration. Without gateway configuration, discovery starts neither the dispatcher nor a capability probe. Explicit `ResultDeliveryEnabled` and register-owned worker startup retain their separate configuration semantics.

## Persistence and idempotency

Durable task result/assistant entry and event have independently tracked consumer progress. In-call result checkpoints are keyed by result, destination/session generation and chunk identity. Opening reserves the exact sent items before the create call; reservations and consumed results exclude in-call voice delivery. Only a receipt matching applied, silent, version and content hash consumes the reservation.

## Failure and recovery

Transport acknowledgement or model wording is not task success; stale observations cannot overwrite current authority. Keep opening reservations while the create outcome is unknown. Release unconsumed reservations on a non-matching receipt or when the session reaches failed or ended. Within the same conversation reconcile chunk acknowledgements and resume only unacknowledged chunks; an HTTP append alone does not prove spoken delivery.

Unsupported, malformed or unavailable health retains pending work with `receipt_capability_disabled`, without transport attempts. An applied receipt proves local transport writes plus durable receipt persistence, never upstream consumption or speech; capability discovery is not live end-to-end evidence.

## Delivery paths

After opening, every write to the upstream conversation uses one of two checkpointed paths; there is no unkeyed append.

| Content | Path | Identity |
|---|---|---|
| Task results | `deliverCheckpointedResult` | result, destination/session generation, chunk |
| Other persisted texts (fast replies, UI feedback) | `deliverDurableText` | `(workspace, owner, source_kind, source_id)`, chunk ordinal |

Both require an active call with a bound gateway session and the receipt capability above. Pending fast replies, including recall answers, follow the owner to the next active call if the previous call closes. A new conversation starts the full reply from the first chunk and preserves its source identity. Conversation-text source kinds are defined in [User text and input feedback](#user-text-and-input-feedback).

## User text and input feedback

Instructions for the voice upstream list background tools only. Message formats and delivery rules are defined here and are not repeated in instructions.

| Source kind | Content sent to upstream | Source identity |
|---|---|---|
| `user_text` | `[User text input] <content>` | conversation-text command identity |
| `ui_input_request` | `The background tool has asked the user for input. Request: <task pending-question text>` | question identity |
| `ui_input_received` | `The background tool received the user's input. Content: <submitted text>` | answer command identity |

`user_text` is the user's own input, produced by the [screen](screen.md) conversation-text input, and is delivered through `deliverDurableText`. `ui_input_request` and `ui_input_received` apply only to `task_question` surfaces. The request text is the task's authoritative pending question, not UI_Controller `input_note`. When no call is active, each text remains pending and is delivered on the next call. A delivery receipt proves transport writes only, never upstream understanding.

## Conversation context events

Zoe records `voice_context_events` atomically with the fenced transition to delivered (all chunks confirmed) or consumed (strict opening receipt); a recording failure rolls back the transition and retries reuse the source identity.
The owner/workspace-scoped identities and roles are `task_result`/entry ID (`background`), `ui_feedback`/request ID (`background`), `fast_reply`/reply ID for task reports and recall (`background`), `user_text`/command ID (`user`), `ui_input_request`/question ID (`background`), and `ui_input_received`/answer command ID (`user`).
Each event retains the delivery session and the text the upstream received without transport wrappers: the source text for in-call deliveries, and the exact `Task result` item (goal, status and summary or result) for results sent in the opening. `at` and `received_at` share the UTC delivery-completion timestamp.
Register, compact and memory ingestion merge events with transcript turns by parsed UTC time, then events before turns, then event kind/ID strings or turn session string/numeric sequence; ingestion membership uses canonical identities (`event:%q:%q` for kind/ID, `turn:%q:%d` for session/sequence).
Compact publication fences the owner-wide count of both sources in its snapshot and retains that snapshot's event rowid watermark, so later inserted events remain raw even when their timestamps equal or precede the build time.

## Task result summaries

`tasks.result` remains authoritative for UI_Controller and prior-task context; in-call voice delivery uses the authoritative assistant entry. `tasks.summary_result` is nullable: NULL means not generated, while an empty string means generated and empty. Steward receives only the summary; lexical candidate ranking may still use full results inside SQL. Opening uses the summary when non-NULL and the authoritative assistant entry text otherwise.

An asynchronous durable sweep selects complete tasks whose summary is NULL, including after restart. One model call receives the goal, status, ordered user additions and full result and returns only a minimal factual summary in the content's language, at most 200 characters. Completion never waits for generation; no database transaction spans the call. Writes only fill a still-NULL summary and fence changes to the source task. See [configuration](configuration.md#required-behavior) for model reuse and retries.

## Implementation locations

[Result delivery](../../../internal/voice/result_delivery.go), [result checkpoint](../../../internal/voice/result_checkpoint.go), [text checkpoint](../../../internal/voice/text_checkpoint.go), [capability gate](../../../internal/voice/result_capability.go).

## Verification obligations

Verify authoritative in-call voice and screen results, opening format and budgeting, reservation exclusion and receipt recovery, and partial-chunk restart/dedupe. Verify owner-scoped whole-reply restart on a new call, the exact user-text and input-feedback formats, pending retention without a call, and chunking under the 8000-byte limit. Check production opening context, candidate facts and the report reply path. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
