# Memory and continuous conversation coverage

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance. Original dialogue and [confirmed context events](results.md#conversation-context-events) are the asynchronous sources even though downstream execution uses the register request. Keep existing independently configurable register, compact and memory model consumers and existing cadence/payload limits unless explicitly changed.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Explicit memory_write: full register request and owner scope | Worker searches existing memories, selects add/replace/none, validates operations and persists in Mem0/Qdrant. No original-utterance duplicate execution input or Steward task. |
| Recall: full register request and owner scope | Worker scoped search returns stored texts; deliver those texts unchanged to voice. No task, answer model, second register round or fixed not-found speech. |
| Scheduled batch: original transcripts and confirmed context events in a fixed owner interval | Memory worker ingests chronological chunks; Go does not extract facts. |
| Register compact/raw/live from original dialogue, confirmed context events and M | Compact covers M to retained compact right edge; raw covers that edge to current input, then live current sentence. Rebuild compact from source, not incrementally. |

## Required behavior

Successful write is silent. Empty retrieval appends nothing. No new content makes no worker call. [Call opening](results.md#required-behavior) reads persisted Register context without waiting for a new model or ingestion call.

Zoe supplies wall-clock timestamps to models only in scheduled memory ingestion. Each source record starts with `[YYYY-MM-DDTHH:MMZ] role: text`; the UTC minute is the observation time for relative dates in that record. Selection and ordering retain the original timestamp precision.

## Persistence and idempotency

- Write: durable write job/operation identity and outcome.
- Recall: durable search/delivery linkage and [chunk progress](results.md#persistence-and-idempotency).
- Batch: stable batch/chunk identity and idempotent worker application; persist successful chunk progress. Only complete success advances sole ingestion boundary M via CAS.
- Compact: one authoritative M in memory_ingest_state; retain prior compact and its coverage on failure.

Existing crash-finalized at-most-once writes and repeated successful ingestion chunks do not establish target idempotency.

## Failure and recovery

Never replay an uncertain write blindly; preserve known applied progress and distinguish uncertainty from success. Search failure is not “no memory” and must not fabricate content. Resume unfinished chunks without duplicate writes; boundary does not advance over failure or unseen content. Compact failure keeps raw coverage from the retained edge; no gap or premature source exclusion. Failed ingestion, write and compact requests follow the [retry contract](configuration.md#required-behavior); an authentication failure stops them until reconfiguration, and unfinished batches and write jobs are retained.

## Implementation locations

[Recall/write jobs](../../../internal/voice/recall.go), [batch scheduler](../../../internal/memory/batch.go), [worker HTTP client/schema](../../../internal/memoryapi/), [memory worker](../../../services/memory/person_memory.py), [register compaction](../../../internal/voice/register_compact.go).

## Verification obligations

Verify scoped full-request write/search, add/replace/none, unchanged recall text/no answer model, idempotent batch/chunk crash recovery and contiguous compact/raw coverage. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
