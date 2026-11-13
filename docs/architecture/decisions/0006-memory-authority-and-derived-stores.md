# ADR 0006: Worker-owned Mem0 and Qdrant memory

Status: accepted architecture baseline, updated 2026-11-13; source implemented on `4eb00fb`, not accepted end to end. [Evidence](../../acceptance/evidence.md) owns verification and [the master plan](../../master-plan.md) owns delivery status.

## Context

Zoe needs persistent personal facts and preferences across calls without making AI memory authoritative for task state, permissions or configuration. The former Go memory authority and derived-store protocol are removed, without backward compatibility.

## Decision

When storing or retrieving personal memory, use only Mem0 and Qdrant behind the Python HTTP worker. Go owns no SQLite memory content, revisions, proposals, tombstones, outbox or deletion ledger. Its only `memory_*` table is ingestion progress, `memory_ingest_state` ([schema.go](../../../internal/memoryapi/schema.go), migration 222 in `memory_ingest`). Formal task state, permission and configuration authority remain outside unrestricted memory writes.

The worker scopes one Mem0 user to a person derived from the JSON workspace/owner tuple ([client.go](../../../internal/memoryapi/client.go), [person_memory.py](../../../services/memory/person_memory.py)). The server selects that identity; recalled text is data, not permission to change business records. [ADR 0013 decision 3](0013-context-register.md#3-memory-cadence) defines immediate writes, scheduled ingestion and recall; [worker configuration](../../../services/memory/config.py) owns model and embedding settings.

When accessing the worker, use Bearer-authenticated HTTP: `POST /v1/memory/search`, `/v1/memory/write`, `/v1/memory/ingest`, `/v1/memory/delete-all`, and `GET /healthz` ([memory_api.py](../../../services/memory/memory_api.py)). The worker does not poll Go jobs. The [deployment runbook](../../operations/deployment.md#server-and-worker-settings) owns listener, token and durable storage configuration. Qdrant is part of the memory store, not an index reconstructible from Go SQLite.

Logical data exports explicitly exclude memory: the worker has no export endpoint ([export.go](../../../internal/control/export.go)). Claim commands, proposal review, old memory routes and native memory management are absent. Only fresh databases and fresh-schema backups are supported; this design provides no old-schema compatibility.

When invoking the Go client's `DeleteAll`, call the worker's `/v1/memory/delete-all` synchronously and return failures without durable retry. No user-facing entry point invokes it today. Worker deletion clears person-scoped vectors and tracked Mem0 history/messages; it does not prove physical backup erasure. Complete instance destruction still requires inventory and external-store cleanup under [data lifecycle operations](../../operations/data-lifecycle.md).

## Consequences

Memory durability requires preserving worker history and the Qdrant bind mount; Go database backup/export alone cannot recover it. Memory operations are not cross-store transactions. Scope isolation, write quality, crash behavior, retention, backup recovery and physical deletion require separate evidence; no product acceptance is implied by source implementation.

## Sources

[Product chapter 10](../../product.md#10-long-term-memory-requirements); [memory architecture](../../architecture.md#memory-retrieval-and-context); historical design: [memory requirements](https://github.com/ronigooja/Zoe/blob/95ee55a5bbd8dccfb7f14af9f9703bb83727da6c/docs/archive/research/reports/17-memory-requirements-and-design.md).
