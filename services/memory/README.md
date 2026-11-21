# Zoe memory worker

This service exposes person-scoped memory through HTTP only. Memory lives in Mem0 with Qdrant vectors and a local history database. There is no claim index, polling loop, control-plane client, deletion-only mode, or control-plane job accounting. Receiver receipts live in the existing Mem0 history database.

Dependency versions are locked in [pyproject.toml](pyproject.toml) and [uv.lock](uv.lock). [config.py](config.py) pins the multilingual embedding revision and dimension; [embedding.py](embedding.py) checks model artifacts. [mem0_backend.py](mem0_backend.py) constructs Mem0 with the pinned embedder and Responses LLM adapter. [Compose](docker-compose.yml) persists Qdrant, Mem0 history, person ownership metadata, and model cache under `ZOE_MEMORY_DATA_ROOT`; it mounts the API token as a file secret. Existing claim-scoped data is not migrated: deployment wipes the test machine's Mem0/Qdrant data.

## Process and configuration

Run `python -m services.memory.worker`. When `ZOE_MEMORY_API_TOKEN_FILE` is unset, `main()` logs one error and exits nonzero before constructing the backend. Otherwise it builds Mem0 and serves HTTP in the main thread; SIGTERM requests shutdown from a separate thread, closes the listener, restores the previous signal handler, and closes the history database. The Responses adapter dispatches directly with SDK retries disabled; request timeout context variables bound HTTP-triggered LLM calls.

The worker reads `ZOE_MEMORY_API_TOKEN_FILE`, `ZOE_MEMORY_API_ADDR`, `ZOE_MEMORY_API_TIMEOUT_SECONDS`, `ZOE_QDRANT_URL`, `ZOE_QDRANT_API_KEY`, `ZOE_MEM0_COLLECTION`, `ZOE_MEM0_HISTORY_DB`, `ZOE_MEM0_LLM_PROVIDER` (only `openai`), `ZOE_MEM0_LLM_MODEL` (required), `ZOE_MEM0_LLM_BASE_URL`, `ZOE_MEM0_LLM_API_KEY`, `ZOE_MEM0_LLM_REASONING_EFFORT`, `ZOE_MEMORY_MODEL_CACHE`, `FASTEMBED_CACHE_PATH`, and `ZOE_LOG_LEVEL`. Obsolete polling/control/runner/claim-collection environment settings are ignored. Mem0 telemetry is forcibly disabled before import.

## Internal person memory API

The standard-library HTTP listener uses `ZOE_MEMORY_API_ADDR` (default `0.0.0.0:8788`). All routes, including `GET /healthz`, require bearer authentication. POST JSON bodies are limited to 64 KiB except `/v1/memory/ingest`, which allows 256 KiB (including JSON framing, measured in bytes); `ZOE_MEMORY_API_TIMEOUT_SECONDS` bounds responses (default 60, maximum 120). In-flight backend operations cannot be forcibly interrupted; mutation outcomes after timeouts can be uncertain. Callers retry only with the same scoped operation/chunk identity; receiver receipts fence uncertain external effects. Concurrency is bounded and new mutations check the deadline before dispatch.

For person-scoped requests, `user` is the caller's opaque workspace/owner tuple, hashed into the `zoe-person-` namespace. These are fresh-baseline contracts: legacy sentence/restatement/query fields and identity-free mutation bodies are rejected. Every required string must be nonempty and non-whitespace.

| POST route | Exact body fields | Result / behavior |
| --- | --- | --- |
| `/v1/memory/search` | `user`, `request`, optional `top_k` | Scoped search without an LLM; `items` contains stored `id`, unchanged `text`, and `score`. `top_k` defaults to 8, maximum 20. |
| `/v1/memory/write` | `user`, `op_id`, `request` | `op_id` is the durable Go write-job ID. `request` is the full register request, never original speech plus a restatement. Returns `operations` with `op`, `text`, and optional `id`. |
| `/v1/memory/ingest` | `user`, `batch_id`, `chunk_id`, `text` | Fixed chronological source chunks merge transcript turns and [confirmed context events](../../docs/design/flows/results.md#conversation-context-events). Returns `added` and `updated` counts; empty extraction returns zeros. |
| `/v1/memory/delete-all` | `user` | Removes scoped vectors, attributable history/messages, ownership metadata and receiver receipts. Unknown pre-ledger history is not globally purged. |

When handling explicit writes, search recalls five existing memories and the configured model selects add/replace/none. The receiver validates the entire output and persists the resolved operation plan before external mutations. Replacement references are exact memory text; unmatched or ambiguous references become adds. Scoped `op_id` receipts retain request hashes, operation progress, status and final outcome. Ready plans resume from their checkpoint; completed writes return the persisted outcome without another model or mutation call. A changed request under the same ID returns `idempotency_conflict`.

When applying a write operation, persist `applying` before calling Mem0, then checkpoint the returned operation and next position. A persisted `applying` state or exception during application returns `write_uncertain`, not ordinary planning/backend failure. It requires reconciliation and must not replay automatically. A crash before planning completes may safely repeat search/model planning because no external mutation began.

When ingesting chunks, Mem0 inference uses [INGEST_INSTRUCTIONS](person_memory.py): durable self-stated user facts/preferences are eligible, not assistant statements or ordinary task content. Scoped chunk receipts retain batch ID, content hash and completed outcome. Completed chunks dedupe across HTTP retries; changed content/identity returns `idempotency_conflict`, and started-without-completion returns `ingest_uncertain` without another Mem0 call.

For scheduled ingestion, Go persists the source snapshot, source-identity membership and chunk payloads in one transaction, with no network call inside it. Source identities follow the [conversation context contract](../../docs/design/flows/results.md#conversation-context-events). Selection uses unassigned identities rather than an event-time lower bound, so backdated arrivals remain eligible after M advances. Eligible source records must be immutable; corrections require a new append-only revision identity. Successful chunks checkpoint durably; only full success advances M by CAS. Existing cadence and HTTP payload limits remain unchanged.

For both mutation routes, SQLite receipts and Mem0/Qdrant effects are not atomic. Receipts prevent blind replay but do not establish exactly-once external writes or automatic recovery of uncertain effects. No job headers or compatibility paths are used.

When `ZOE_LOG_LEVEL=debug` (default `info`), Zoe logs search query/results, complete write model input/output, ingest text/instructions and Mem0 results, and permits Mem0 content logs. At INFO, each synchronous LLM call logs only `memory_api llm_call component=<memory_write|memory_ingest> status=<ok|error> error_kind=<none|backend_unavailable|deadline_exceeded> elapsed_ms=<duration>`; requests log fixed request-kind labels (`ingest` for ingestion), status, and duration. Neither INFO record includes content, user IDs, credentials, or raw exception messages. HTTP client debug logging remains disabled to protect credentials.
