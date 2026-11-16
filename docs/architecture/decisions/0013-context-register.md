# ADR 0013: Persistent conversation context register

Status: **Approved target updated 2026-11-16; not a claim of current source implementation, deployment or end-to-end acceptance.** The [data-flow contract](../../design/data-flows.md) is canonical for schemas, semantic action definitions, producers/consumers, persistence, recovery and acceptance requirements. Earlier source review on `4eb00fb` remains [historical evidence](../../acceptance/evidence.md#worker-memory-redesign-source-review---2026-11-13), not conformance to this target.

This decision supersedes routing, impact policy, opening instructions, context, Steward task references/actions, reply language and memory cadence in [ADR 0009](0009-voice-steward-impact-policy.md) and [ADR 0012](0012-voice-conversation-layer.md). Their deployment observations remain historical evidence. The approved UI flow also supersedes the local-only composition/server-document prohibition in [ADR 0008](0008-ambient-screen-client.md) and the [screen contract](../../design/screen-document.md#local-composition-and-delivery), not their renderer, platform shell, settings or presence rules.

## Context

Recorded calls exposed incomplete context, misleading delivery statements and failed reference resolution. Earlier designs used restatement sentinels, quoted-sentence selection, and duplicated or concatenated execution inputs. The approved target replaces those boundaries without treating design approval as product acceptance.

## Decision

### 1. Register first; triage deleted

When handling a delegated sentence, the register reads compact context, contiguous raw dialogue and the current sentence in one routing call. It reads no memory keys/subjects and performs no task management. Apply the [canonical register contract](../../design/data-flows.md#register): a complete request replaces restatement, with no original-utterance fallback. Original utterances remain provenance/dialogue/asynchronous-ingestion source, not duplicate downstream execution input. Source constants or old storage columns do not override this target.

There are no legacy progress/cancel/confirm/decline/unclear/memory_read register routes. Failure admits no fallback task; retain the concise `Processing error.` failure path rather than inventing work. No keyword-based impact classification, impact-confirmation waiting task or mandatory confirmation gate is introduced. Existing Worker authority and effect-recovery boundaries remain applicable.

### 2. Register scope and contiguous layers

When maintaining conversation context, persist it per workspace/owner across calls. The sole ingestion boundary M belongs to memory_ingest_state, not independently advanced register state. Coverage and failure-safe compact/raw/live assembly are canonical in the [memory/context flow](../../design/data-flows.md#4-memory-and-continuous-conversation-coverage).

Keep existing defaults: raw window 60 minutes, compact rebuild hourly, compact character limit 8000. Rebuild from original source rather than incrementally; no new dialogue means no rebuild. Originals include both roles from voice_transcript_turns, ordered by timestamp, session ID and session-scoped sequence. Preserve the prior compact and raw text from its retained edge when rebuilding fails.

When configuring routing and compaction, use independent ZOE_REGISTER_* and ZOE_COMPACT_* groups with no Steward/cross-model fallback. Both require BASE_URL, MODEL and a readable nonempty API_KEY_FILE to enable the register. Unset/partial groups disable it without startup failure and report missing variable names without secret values; disabled routing admits no task. Invalid explicitly set tunables or unreadable/empty keys in a complete group still fail startup by variable name. Existing routing/compaction timeout defaults are 30000/600000 ms. Exact deployment variables remain in [deployment configuration](../../operations/deployment.md#register-routing-configuration).

When creating a call, read stored compact plus raw into initial_items within existing budgets; never wait for a model/ingestion call. Mid-call context uses session.context.append. Historical Codex-source 128-item/8192-token limits are not measured gateway/upstream ceilings.

### 3. Memory cadence

When writing, recalling or ingesting memory, use the [canonical memory flow](../../design/data-flows.md#4-memory-and-continuous-conversation-coverage). Full register requests replace original-plus-restatement execution input; recall has no task or answer model. Scheduled ingestion requires durable idempotent batch/chunk progress, not repeated successful writes after failure.

Memory remains scoped to one person derived from workspace/owner, behind the HTTP worker in Mem0/Qdrant; [ADR 0006](0006-memory-authority-and-derived-stores.md) owns storage authority. Go does no extraction-model call. The worker searches before explicit add/replace/none decisions; Mem0 add appends and explicit replacement uses update. Exact extraction/write prompts remain in [person_memory.py](../../../services/memory/person_memory.py), not duplicated here.

When scheduling batches, retain the existing 1..168 hour ZOE_MEMORY_BATCH_INTERVAL_HOURS setting, default 24 hours measured from last success, not hangup. No new content means no call or boundary advancement. Both-role transcripts and completed-task source/request/results form the batch. Existing sequential payload limits remain; only complete success advances M and next-attempt deadline via CAS in memory_ingest_state (migration 222, memory_ingest namespace). Explicit writes are independent. Transcript retention enforcement remains unresolved.

Historical summary/legacy-decision migrations 220/221 removed unused paths; they do not establish target request/question storage, deployment or acceptance.

### 4. Opening instructions: capabilities, not requirements

When building upstream voice instructions, list available capabilities: execute tasks, search the web, use files/devices, report progress/results, cancel/change tasks and retrieve personal records/earlier conversations. Do not require voice delegation or add a mandatory first user-rule item. Worker execution delegation is a separate policy in the linked execution flow, not a voice-model instruction.

Historical synthetic-speech instruction probes did not measure capability-only instructions. Real contextual delegation quality remains an acceptance requirement.

### 5. Steward and task references

When selecting task handling, use the [canonical Steward schemas and semantic action definitions](../../design/data-flows.md#steward). Stable real task/question IDs replace quoted sentences and aliases. Empty candidates do not automatically imply new work; ambiguity requires clarification; stale facts require refreshed selection, never silent new admission. Preserve full candidate goals/questions/results and retained ordered additions rather than filtering long requests out.

The server retains the request and applies the [execution flow](../../design/data-flows.md#2-voice-handoff-and-execution): all execution branches use it in full. Live additions retain their session; answers bind a current question; continuation uses structured prior context, not semantic goal concatenation; redisplay invokes no Worker. Worker goal-once delivery and tool/subagent policy apply to initial, resume and rotation paths.

Historical OpenCode V2 source review at anomalyco/opencode `6d02304` found busy-session prompt steer at the next step boundary and optional first-admission-wins message identities. This is source evidence only; deployed runtime/session recovery behavior requires verification.

### 6. Reports and latency

When reporting or clarifying, use Steward's canonical decision/reply fields without another answer model. Report actual selected-task facts, not an invented delivery flag, newest-task default or empty-task exception. Reporting does not execute work.

When delivering results, replies or recall, apply the [authoritative result/voice/UI flow](../../design/data-flows.md#3-authoritative-results-voice-and-ui). It owns unchanged text, stable identity, pending contextual next-call delivery, chunk checkpoint dedupe, direct bound UI input and render-ACK-gated success feedback. Older non-checkpointed retries and notification-only no-call handling are not target conformance.

Every delegated sentence incurs the register routing call; task routing adds Steward's call. Retain existing 30 s Steward/35 s classification budgets and ZOE_VOICE_CLASSIFICATION_TIMEOUT_MS configuration name. Added latency is approved, not measured; routing cannot guarantee upstream speech does not begin first.

## Consequences and unverified boundaries

When implementing this decision, use the [target acceptance requirements](../../design/data-flows.md#8-acceptance-requirements); do not infer source, deployment or acceptance status from this ADR. Model semantic fidelity, task/reference selection, memory quality, runtime continuity, actual speech/rendering, recovery and live end-to-end flows remain separate evidence obligations. Task retention and full Worker step-level narrative storage remain unresolved. [The master plan](../../master-plan.md) owns delivery status.
