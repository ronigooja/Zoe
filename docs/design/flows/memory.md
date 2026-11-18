# Memory and contiguous conversation contract

## Purpose and boundary

Approved target, 2026-11-16; not implementation, deployment, or acceptance evidence. The [approved overview](../data-flows.md) establishes precedence. Personal memory lives only in Mem0/Qdrant behind the Python HTTP worker. Go extracts no facts and owns no SQLite memory content, claims, revisions, proposals, tombstones, memory outbox, or deletion ledger. Formal task state, permissions, configuration, and confirmed-decision authority remain outside unrestricted memory writes. Qdrant is part of the memory store, not reconstructible from Go SQLite.

## Inputs and outputs

| Input | Consumer and result |
|---|---|
| Explicit `memory_write`: full register request and owner/workspace scope | Worker searches existing memory, selects add/replace/none, validates operations, persists in Mem0/Qdrant. Successful writes are silent; no Steward task or duplicate original-utterance execution input. |
| `recall`: full register request and owner/workspace scope | Scoped worker search returns stored texts unchanged to [voice transport](results.md); no task, answer model, second register round, or fixed not-found speech. |
| Scheduled fixed-owner interval: both-role original transcripts and completed-task source/request/result records | Worker ingests chronological chunks; no Go extraction. |
| Original dialogue plus sole ingestion boundary M | Register compact/raw/live coverage and call-opening context. |

The server derives one person identity from JSON `[workspace, owner]`, never caller-selected cross-owner scope. Use Bearer HTTP `POST /v1/memory/search`, `/v1/memory/write`, `/v1/memory/ingest`, `/v1/memory/delete-all`, and `GET /healthz`; worker does not poll Go jobs. Recall text is data, not business-record authority. Mem0 add appends; explicit replacement uses update. Prompts/model/embedding versions remain in worker configuration, not copied here.

## Required behavior

### Scheduled ingestion

Keep independent register, compact, and memory model consumers and existing cadence/payload limits. `ZOE_MEMORY_BATCH_INTERVAL_HOURS` accepts 1..168, default 24 hours from last successful ingestion, not hangup. A minute ticker performs no immediate startup batch; failures retry on the next tick. No new content means no worker call, no rebuild, and no boundary advancement. Explicit writes are independent of the schedule.

Read `(M, tick time]`; missing owner progress starts at Unix epoch. Preserve chronological both-role transcript turns and completed-task source/original request/results. Transcript blocks are `[RFC3339Nano UTC] user: text\n` or `assistant: text`; completed-task blocks are `[RFC3339Nano UTC] completed task:\nuser: original request\nresult: result\n`, appended after transcript blocks in completion order. Sequential payloads contain at most 200 KiB UTF-8 text and 256 KiB serialized JSON. Keep turns/tasks whole unless an individual block exceeds either limit.

### Compact/raw/live coverage

Persist context per workspace/owner across calls. Sole ingestion boundary M belongs to `memory_ingest_state`; no independently advanced register boundary. Compact covers M to its retained right edge; raw covers that edge to current input; live is current utterance. [Configuration](configuration.md#register-configuration-and-routing-deadlines) owns raw-window, rebuild-interval, and compact-character defaults/ranges. Rebuild compact from original source, never incrementally; without new dialogue, do not rebuild compact even if task records changed. Original both-role `voice_transcript_turns` are ordered by timestamp, session ID, and session-scoped sequence. Originals remain asynchronous source even though downstream execution uses only the register request.

Call opening reads persisted compact plus raw into `initial_items` within existing budgets without waiting for a model/ingestion call. Mid-call context uses `session.context.append`. Historical Codex-source 128-item/8192-token limits are not measured gateway/upstream ceilings. Gateway-reported resumed upstream conversation receives changes since its last context version; a new conversation receives full opening context. Record sent version in the call context manifest.

### Product scope and current exclusions

Memory/context obligations remain goals: persistent global/project/task/raw-history context, project access boundaries, relevant authorized source-linked retrieval, dated decision changes/validity, distinction between confirmed facts/execution evidence/inference, and explicit uncertainty when evidence is missing/conflicting. Optional event-driven memory organization may detect duplicates/staleness/conflicts but cannot overwrite authoritative decisions. Assemble minimal relevant task/runtime/project/permission context without losing traceability; a goal is a relevance hint, not scope authority. Retrieval must preserve access/source/budget checks; this does not restore removed Go keyword-search or claim APIs.

Current exclusions are explicit: no claim/proposal review or native memory-management UI, memory export endpoint, user-facing deletion entry, Go fallback memory/index, or summary pipeline. Logical exports explicitly exclude memory. Only fresh databases/fresh-schema backups are supported; no old-database migration or old-backup restore path is supplied. These limits do not erase broader product obligations or prove them implemented.

## Persistence and idempotency

Persist explicit write job/operation identity and outcome, search/delivery linkage, and [chunk progress](results.md). Scheduled ingestion requires stable batch/chunk identity, idempotent worker application, and persisted successful-chunk progress. Only complete success advances M and next-attempt deadline via CAS in `memory_ingest_state` (migration 222, `memory_ingest` namespace). Preserve worker Mem0 history and durable Qdrant storage; Go backup/export alone cannot recover memory. Operations are not cross-store transactions.

## Failure and recovery

Never blindly replay uncertain writes; retain applied progress and distinguish uncertainty from success. Empty recall appends nothing; search failure is not “no memory” and cannot fabricate content; retain concise `Processing error.` behavior. Resume unfinished ingestion chunks without duplicate writes; M cannot advance over failure or unseen content. Crash-finalized at-most-once writes and replaying successful ingestion chunks do not establish target idempotency.

Compact failure retains previous compact/coverage and raw from its retained edge, with no gap or premature exclusion. Worker `DeleteAll` is synchronous and returns failures without durable retry; no user-facing entry invokes it. Deletion clears person-scoped vectors/tracked history/messages but does not prove physical backup erasure. Instance destruction still inventories external stores. Transcript retention enforcement remains unresolved.

## Implementation locations

[Recall/write](../../../internal/voice/recall.go), [batch scheduler](../../../internal/memory/batch.go), [worker client/schema](../../../internal/memoryapi/), [worker](../../../services/memory/person_memory.py), [HTTP API](../../../services/memory/memory_api.py), [configuration](../../../services/memory/config.py), [compaction](../../../internal/voice/register_compact.go), [logical export](../../../internal/control/export.go).

## Verification obligations

Verify scoped full-request search/write, search-before-add/replace/none quality, unchanged recall/no answer model, silence/empty/failure distinction, idempotent batch/chunk crash recovery, exact boundaries/limits/CAS, and gap-free compact/raw/live coverage including failure and call opening. Independently verify scope isolation, factual/inference traceability, retention, worker/Qdrant backup recovery, and physical deletion; source/storage health does not establish them. Follow [evidence and acceptance](../../development.md#evidence-and-acceptance).
