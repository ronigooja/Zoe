# Authoritative results and delivery contract

## Purpose and boundary

Approved target, 2026-11-16; not source conformance, deployment, or acceptance evidence. The [approved overview](../data-flows.md) establishes precedence. Voice and [screen](screen.md) consume the same authoritative result text and stable identity. Transport acknowledgement, model wording, task success, display success, and speech are distinct facts. This contract also owns durable report/recall text transport; it introduces no separate answer model.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Worker terminal observation: task/run identity, assistant result, evidence | Control plane validates fences and commits authoritative task result/assistant entry and event. |
| Result event: stable result identity, full text | Voice appends unchanged original text; report replies and recall texts use the same durable transport. |
| Voice chunks | Gateway/upstream context consumer receives sequential UTF-8-boundary-safe chunks within the existing 8000-byte append limit. |
| Authoritative result/question and display request | [UI_Controller](screen.md) presents authoritative data, never replaces the result with a UI summary. |

The terminal assistant entry and `task.assistant_message` event commit together; event metadata includes `task_id`, `run_id`, `entry_id`, `entry_kind`, and role. Downstream consumers never fabricate history.

## Required behavior

Validate UTF-8 and nonblank text while preserving whitespace, control characters, and full content. Split sequentially without dropping/truncating content. No separate answer model rewrites results, report replies, or recalled stored texts.

With no active call, retain full results pending with source/task/result identity and delivery state. They remain available as silent next-call context and authoritative candidate facts: not dropped, notification-only, or compulsory indiscriminate playback of all history. A contextual `report` delivers its fact-grounded reply once through the durable reply path. That report must not also queue the original result or mark it delivered because a summary was supplied. Original text remains pending/available until its own acknowledged delivery. `redisplay` is UI-only and does not authorize voice playback. Neither report nor UI summary replaces the authoritative result. Voice/UI completion ACKs are separate evidence; screen input can complete without voice.

### Production receipt capability

Before checkpoint transactions, probe authenticated `GET /v1/health` using configured gateway URL, owner bearer, and HTTP client. Only the exact `result_deliveries` contract enables the receipt adapter for task results and generic durable texts:

```javascript
{version:"v1",max_text_bytes:8000,applied_evidence:"local_transport_writes_and_durable_receipt"}
```

Cache capability evidence for 30 seconds, with one probe in flight and a two-second probe budget. Unsupported, malformed, or unavailable health retains pending work with `receipt_capability_disabled`, with no transport attempts. No new deployment setting is required. Applied receipts prove local transport writes plus durable receipt persistence, never upstream consumption or speech; discovery is not live end-to-end evidence.

Production discovery starts the dispatcher only with a configured HTTPS gateway and owner key, independent of account-media enablement and register configuration. Without gateway configuration it starts neither dispatcher nor capability probe. Explicit `ResultDeliveryEnabled` and register-owned worker startup retain separate configuration semantics.

### Notices

Completion/failure, mail, approvals, scheduled outcomes, and unrecoverable conditions need source-linked notices. Classify routine information, completion, failure, and decisions; deduplicate and avoid noisy reminders. A stable random installation UUID scopes push registration independently of the owner key so one installation cannot overwrite/revoke another. APNs is only a wake-up hint to refresh the durable inbox; payloads contain no task content/notice body. Server retry/invalid-token handling and actual APNs delivery require separate evidence. Notices do not substitute for pending contextual results.

## Persistence and idempotency

Commit durable result/assistant entry and outbox event after task/run fences pass. Track consumers independently. Durable voice delivery and acknowledged chunk checkpoint are keyed by result, destination/session generation, and chunk identity. Stable chunk IDs prevent prefix repetition after partial delivery. Report and recall preserve their search/reply/delivery linkage. [Screen](screen.md) owns durable presentation identities and render ACKs, not voice-delivery state.

## Failure and recovery

Stale observations cannot overwrite current authority. Stop chunk delivery on failure; reconcile receipts and resume only unacknowledged chunks from checkpoint, not the prefix. HTTP append alone does not prove spoken delivery. No active call retains pending work for contextual next-call handling. Capability failure retains pending state without transport attempts. Event retry/reconnect must not reset delivered state or duplicate actions.

## Implementation locations

[Result delivery](../../../internal/voice/result_delivery.go), [capability gate](../../../internal/voice/result_capability.go), [recall/write jobs](../../../internal/voice/recall.go), [task entries](../../../internal/control/task_entries.go), [UI jobs](../../../internal/voice/ui_jobs.go), [UI feedback](../../../internal/voice/ui_feedback.go).

## Verification obligations

Verify same authoritative voice/screen result, unchanged complete UTF-8 text, 8000-byte boundary-safe chunks, partial-delivery restart/dedupe, independent consumer progress, report/original separation, and no-call contextual availability. For the latter, inspect production opening context, candidate facts, and report reply path. Scoped `SelectPendingResult` utility tests cover explicit original-delivery selection only; production invocation of that utility is not an acceptance criterion for contextual availability.

Exercise exact capability matching, authenticated probe/client, cache/in-flight/budget constants, disabled reasons/no transport, and startup independence. Distinguish receipt evidence, context append, actual speech, render ACK, APNs, and real user-visible end-to-end success using [evidence and acceptance](../../development.md#evidence-and-acceptance).
