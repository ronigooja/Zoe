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

When no call is active, pending full results retain source/task/result identity and delivery state and remain available as silent next-call context and authoritative candidate facts, not compulsory playback. A contextual `report` delivers its fact-grounded reply once through the durable reply path; it must not also queue the original result for that report or mark the original delivered merely because a summary was supplied. The original remains pending and available unless its own delivery is acknowledged. `redisplay` remains UI-only and does not authorize voice playback.

For production receipt delivery, Zoe probes authenticated `GET /v1/health` using the configured gateway URL, owner bearer and HTTP client before checkpoint transactions. Only the exact `result_deliveries` contract `{version:"v1",max_text_bytes:8000,applied_evidence:"local_transport_writes_and_durable_receipt"}` enables the receipt adapter for task results and generic durable texts. Capability evidence is cached for 30 seconds, with one probe in flight and a two-second probe budget. No new deployment setting is required.

Production discovery starts the result dispatcher only with a configured HTTPS gateway and owner key; it is independent of account-media enablement and register configuration. Without gateway configuration, discovery starts neither the dispatcher nor a capability probe. Explicit `ResultDeliveryEnabled` and register-owned worker startup retain their separate configuration semantics.

## Persistence and idempotency

Durable task result/assistant entry and event have independently tracked consumer progress. Durable result delivery and acknowledged chunk checkpoint are keyed by result, destination/session generation and chunk identity. Stable chunk IDs/acknowledged progress prevent prefix repetition after partial delivery.

## Failure and recovery

Transport acknowledgement or model wording is not task success; stale observations cannot overwrite current authority. No active call leaves delivery pending for contextual delivery on the next call: not dropped and not indiscriminate playback of all history. Reconcile chunk acknowledgements and resume only unacknowledged chunks. Stop on failure and resume from checkpoint; an HTTP append alone does not prove spoken delivery.

Unsupported, malformed or unavailable health retains pending work with `receipt_capability_disabled`, without transport attempts. An applied receipt proves local transport writes plus durable receipt persistence, never upstream consumption or speech; capability discovery is not live end-to-end evidence.

## Implementation locations

[Result delivery](../../../internal/voice/result_delivery.go), [capability gate](../../../internal/voice/result_capability.go).

## Verification obligations

Verify the same authoritative result for voice and screen, no-call pending next-call contextual delivery, and partial-chunk restart/dedupe. For contextual-availability verification, check production opening context, candidate facts and the report reply path. Scoped utility tests for `SelectPendingResult` verify explicit original-delivery selection only; a production call to that utility is not an acceptance criterion for contextual availability. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
