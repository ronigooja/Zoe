# Model configuration and deployment

## Purpose and boundary

Approved target, not a claim of current implementation, deployment or acceptance.

## Inputs and outputs

| Producer / fields | Consumer and transformation |
|---|---|
| Operator model menu: selected provider/model and effort plus secret references | Probe the proposed configuration, then atomically save/reload the corresponding environment/routes, OpenCode provider/agent configuration and session selection. All Worker layers resolve the same model+effort. |
| Register, Compact, Steward, UI_Controller, memory and Worker consumers | Preserve separate model configurations; changes target the chosen consumer rather than silently changing another or falling back across consumers. |
| Release/deployment manifest, network configuration, key references and persistent data | Deployment retains existing network reachability, keys and data; gateway image-only update does not implicitly reset configuration or state. |

## Required behavior

Never log credentials. Reset and backup are explicit operations; never silently regenerate keys or destroy data to make startup pass. Verify effective runtime/session settings rather than menu success alone.

**Probe.** An operator probe of a model consumer makes one real generation request using the endpoint and protocol style the consumer uses at runtime. Listing models does not establish that a consumer works. The Worker provider probe also lists models, for model metadata and limits, and makes one real `responses` request. A probe makes one request and never retries. Authentication failure (401/403) is reported as such; other HTTP errors, timeouts and network errors are reported as distinct categories, never as authentication failure.

**Retry.** Every model request that is retried uses one shared exponential backoff: the delay
doubles per consecutive failure from a consumer-specific base delay up to a consumer-specific cap,
with jitter. Interactive consumers (Register, Steward, UI_Controller) use a short base and a bounded
attempt count; background consumers (Compact, memory write, memory ingestion) use a longer base and
retry without an attempt bound. Failure count and next-attempt time are persisted and survive
restart. Authentication failure (401/403) is not retried: the consumer stops, reports the condition
in `zoe status`, and resumes only after the operator reconfigures it. The server clears an
authentication stop when it observes that the consumer's configuration changed (endpoint, model,
effort or credential content, compared by digest, never by stored secret); an unchanged restart
keeps the stop. `zoe status` reads retry state that the server publishes, not the server database.
Transient failure (timeout, 429, 5xx, network error) retries under the backoff. UI_Controller
malformed-output retries use the `ui_output_attempts` limit (including the first attempt), do not
count against the transient-error budget, and carry only the fixed validation category. A retry
never replays an uncertain write blindly; see [Memory](memory.md#failure-and-recovery).

**Prompts.** Every instruction text sent to a model or to the voice upstream (Register routing,
Compact, task summary, Steward, UI_Controller, the task display instruction, the voice upstream
instruction and memory instructions) is a plain UTF-8 text file maintained in the repository under
`deploy/prompts/`, published with the server release and installed under
`/etc/zoe/prompts/<name>.txt`. The server reads each file once at startup and keeps no built-in
copy. A missing, unreadable, empty, non-UTF-8, or oversized file stops startup. The size ceiling is
`prompt_file_max_bytes`. The memory worker independently loads `memory_ingest.txt`,
`memory_write.txt` and `memory_custom.txt` once at its own startup, with the same directory override
(`ZOE_PROMPT_DIR`), failure rules and immutable snapshot semantics; both loaders remove exactly one
final newline. A changed file takes effect after restart. A voice upstream instruction file whose
entire content is `off` disables that instruction. Deployment installs a prompt file that is absent,
keeps an existing one, and replaces existing files only when the operator selects the overwrite
option, which first backs each replaced file up as `<name>.txt.pre-<sha12>`. Deployment fails before
switching if a required prompt file is missing or unreadable, and a dry run lists each file as
install, keep, overwrite or identical without printing content. Prompt content, including memory
instructions, is not part of a consumer's configuration identity, so changing it never clears an
authentication stop. `zoe status` shows each prompt's path and byte size, never its content. The
UI_Controller prompt must stay consistent with the output validation in
[uictl](../../../internal/uictl/controller.go).

**UI_Controller timeout.** `ZOE_UI_CONTROLLER_TIMEOUT_MS` bounds each model HTTP request (500..120000 ms, default 30000 ms); no separate UI job call timeout overrides it.

**Task summaries.** The [task-summary worker](results.md#task-result-summaries)
reuses Compact's loaded `ZOE_COMPACT_*` endpoint, model, effort, thinking,
credential and timeout, and is enabled with the configured Register/Compact pair.
It uses the separate persisted consumer name `task_summary` with one global retry
scope, the shared background exponential backoff without an attempt bound, and
a consumer-wide 401/403 stop. `zoe status` reports it independently; a Compact
configuration change clears its authentication stop. Its instruction file is
`deploy/prompts/task_summary.txt`, installed as `/etc/zoe/prompts/task_summary.txt`
under the prompt lifecycle above.

### Zoe limits

The server, runner and memory worker load the same UTF-8 JSON object once at
startup from `/etc/zoe/limits.json` or `ZOE_LIMITS_FILE`. The memory worker's
configuration digest excludes limits: they do not change model identity or
clear an authentication stop. Restart all consumers after editing the file.
An absent, unreadable, empty, or larger than 65,536-byte file stops startup,
as do malformed JSON, duplicate, unknown or missing keys, and values other
than positive JSON integers in the declared ranges. Decimal and exponent
notation are not accepted. Keep every key when editing the shipped file.
Removed limit environment variables must be deleted, not left empty; startup
and deployment report the replacement key without logging the value.

Release tooling ships `deploy/limits.json` beside the prompt assets. Install
and update keep existing valid limits; `deploy.sh --prompts overwrite` also
replaces differing limits and backs up the previous file as
`<filename>.pre-<sha12>`. Deployment validates the shipped and effective files
before switching; dry runs print only metadata and the planned action. Set
the same absolute override in `zoe-stack.env` and `zoe-runner.env` for systemd
deployments. Mount it read-only in both Compose services. `zoe status` reports
the path and byte size, not the contents.

| Key | Default | Range | Bounds |
|---|---:|---:|---|
| `opening_context_runes` | 8000 | 1–100000 | Opening context, runes |
| `opening_pending_results` | 25 | 1–128 | Pending opening result rows |
| `compact_max_characters` | 8000 | 500–100000 | Compact output, runes |
| `steward_candidate_default` | 8 | 1–100 | Default Steward candidate tasks |
| `steward_candidate_max` | 20 | 1–100 | Maximum Steward candidate tasks |
| `steward_recent_unresolved` | 5 | 1–100 | Recent unresolved candidate rows |
| `steward_input_max_bytes` | 4194304 | 1024–33554432 | Serialized Steward input, bytes |
| `task_text_max_bytes` | 20000 | 1–1048576 | Each goal, initial message, addition, answer; bytes |
| `task_summary_max_runes` | 200 | 1–8192 | Task summary output, runes |
| `task_summary_sweep_rows` | 50 | 1–1000 | Task summary sweep rows |
| `task_entries_page_default` | 50 | 1–1000 | Default task entry page rows |
| `task_entries_page_max` | 100 | 1–1000 | Maximum task entry page rows |
| `ui_output_max_bytes` | 65536 | 1024–1048576 | UI model output, bytes |
| `ui_state_max_bytes` | 262144 | 1024–4194304 | Retained UI state, bytes |
| `ui_input_state_multiplier` | 16 | 1–32 | UI input budget in multiples of state bytes |
| `ui_feedback_max_bytes` | 8000 | 1–65536 | UI feedback, bytes |
| `ui_max_messages` | 64 | 1–256 | A2UI messages per response |
| `ui_max_components` | 256 | 1–1024 | A2UI components per update |
| `ui_output_attempts` | 1 | 1–4 | Total attempts on invalid UI output |
| `ui_event_sweep_rows` | 100 | 1–1000 | UI event sweep rows |
| `ui_job_sweep_rows` | 25 | 1–1000 | UI job sweep rows |
| `voice_text_max_bytes` | 20000 | 1–1048576 | Conversation text input, bytes |
| `voice_text_sweep_rows` | 25 | 1–1000 | Conversation text sweep rows |
| `voice_feedback_sweep_rows` | 25 | 1–1000 | UI feedback sweep rows |
| `voice_delegation_sweep_rows` | 25 | 1–1000 | Delegation sweep rows |
| `voice_result_sweep_rows` | 25 | 1–1000 | Result delivery scan and retry rows |
| `voice_fast_reply_sweep_rows` | 25 | 1–1000 | Fast reply sweep rows |
| `voice_recall_sweep_rows` | 25 | 1–1000 | Recall job sweep rows |
| `voice_recall_workers` | 4 | 1–64 | Concurrent recall jobs |
| `voice_delivery_attempts` | 3 | 1–10 | Result and fast-reply delivery attempts |
| `steward_selection_refreshes` | 3 | 1–10 | Refreshes after stale selection |
| `voice_sdp_max_bytes` | 1048576 | 1024–1048576 | Decoded SDP offer, bytes |
| `voice_lifecycle_max_bytes` | 65536 | 4096–1048576 | Call lifecycle request, bytes |
| `http_request_max_bytes` | 1048576 | 4096–33554432 | General HTTP request body, bytes |
| `http_call_start_max_bytes` | 6307840 | 16384–33554432 | Call start JSON body, bytes |
| `http_header_max_bytes` | 1048576 | 4096–4194304 | HTTP headers, bytes |
| `entry_receipt_max_bytes` | 4096 | 1024–1048576 | Entry delivery receipt body, bytes |
| `instance_fence_max_bytes` | 4096 | 1024–1048576 | Instance fence body, bytes |
| `event_page_rows` | 100 | 1–1000 | Event stream page rows |
| `diagnostic_body_max_bytes` | 1048576 | 4096–33554432 | Diagnostic request body, bytes |
| `diagnostic_event_max_bytes` | 16384 | 256–1048576 | Serialized diagnostic event, bytes |
| `diagnostic_max_batch` | 50 | 1–1000 | Diagnostic events per request |
| `diagnostic_max_devices` | 1024 | 1–16384 | Tracked diagnostics devices |
| `diagnostic_events_per_device` | 200 | 1–4096 | Diagnostic events per device per minute |
| `memory_ingest_chunk_bytes` | 204800 | 32–4194304 | Raw memory ingestion text, bytes |
| `memory_ingest_body_bytes` | 262144 | 256–8388608 | Memory ingestion JSON body, bytes |
| `memory_api_body_bytes` | 65536 | 256–8388608 | Other memory API request bodies, bytes |
| `memory_search_default` | 8 | 1–100 | Default memory search results |
| `memory_search_max` | 20 | 1–100 | Maximum memory search results |
| `memory_write_recall_rows` | 5 | 1–100 | Existing memory rows for write planning |
| `memory_write_max_operations` | 8 | 1–64 | Operations per memory write |
| `memory_recent_rows` | 10 | 1–1000 | Recent memory message rows retained |
| `prompt_file_max_bytes` | 16000 | 1–1048576 | Each prompt file, bytes |
| `profile_max_external_directories` | 8 | 1–32 | External directory rules per profile |
| `max_concurrent_runs` | 12 | 1–64 | Concurrent runs including uncertain runs |
| `runner_context_max_tokens` | 9223372036854775807 | 1–9223372036854775807 | Runner context ceiling, tokens |
| `runner_max_rotations` | 3 | 1–64 | Context rotations per run |
| `runner_rotation_prompt_bytes` | 24000 | 1024–1048576 | Rotation prompt budget for derived history, bytes |
| `runner_rotation_summary_bytes` | 12000 | 1024–1048576 | Session history for rotation summarization, bytes |
| `runner_rotation_history_item_bytes` | 300 | 1–2000 | Bytes per message or tool field |
| `runner_rotation_summary_max_tokens` | 4000 | 1–32000 | Requested summarizer output tokens |
| `runner_rotation_summary_input_bytes` | 16000 | 1024–1048576 | Summarizer input history bytes |
| `runner_rotation_summary_output_bytes` | 16000 | 1024–1048576 | Reused summary bytes |
| `runner_assistant_output_max_bytes` | 20000 | 1–1048576 | Assistant result bytes |
| `runner_result_evidence_max_bytes` | 20000 | 1–1048576 | Result evidence summary bytes |
| `capability_manifest_max_bytes` | 262144 | 1024–4194304 | Canonical capability manifest, bytes |
| `capability_manifest_max_args` | 128 | 1–1024 | Capability entrypoint argument count |
| `capability_manifest_max_capabilities` | 64 | 1–512 | Capability IDs per manifest |
| `capability_manifest_max_mounts` | 64 | 1–512 | Mounts per capability manifest |
| `capability_manifest_max_triggers` | 32 | 1–256 | Triggers per capability manifest |
| `capability_manifest_arg_max_bytes` | 4096 | 1–65536 | Each capability entrypoint argument, bytes |
| `capability_manifest_entrypoint_max_bytes` | 500 | 1–4096 | Capability entrypoint, bytes |
| `capability_trigger_max_scopes` | 32 | 1–512 | Scope IDs per trigger |
| `capability_trigger_max_filters` | 32 | 1–256 | Filters per trigger |
| `capability_filter_value_max_bytes` | 256 | 1–4096 | Filter comparison value bytes |
| `capability_trigger_payload_max_bytes` | 65536 | 1024–1048576 | Capability event payload, bytes |
| `capability_trigger_page_rows` | 100 | 1–1000 | Capability trigger processing page rows |
| `capability_deployment_page_rows` | 100 | 1–1000 | Capability deployment claim page rows |
| `model_api_response_max_bytes` | 8388608 | 1024–33554432 | Model API response, bytes |
| `model_catalog_max_bytes` | 16777216 | 1024–67108864 | Provider and official model catalog, bytes |
| `model_catalog_max_entries` | 500 | 1–10000 | Entries returned in model catalog |

The runner context ceiling defaults to the largest signed 64-bit integer,
so the model window ratio and run budget govern it unless explicitly lowered.
The Steward cap uses 4 MiB: the default eight candidates, allowing four
20,000-byte facts per candidate at six encoded bytes per input byte, and a
64 KiB request plus JSON framing. Additions and stored full results are not
truncated; unusually large accumulated inputs fail classification without
model transport or task admission and use the existing `Processing error.`
reply. Compact output is assembled and trimmed independently for opening;
there is no minimum dependency between their budgets.

Cross-limit checks:

- Opening runes × 4 ≤ the gateway opening item byte maximum.
- SDP bytes × 6 + 16,384 ≤ the call-start body, separate from the general body.
- Two task text fields × 6, conversation text × 6, and runner assistant
  output × 6 (each with 4,096 bytes of framing allowance) fit the general body.
- General request body accommodates transcript batches, receipts, fencing,
  call lifecycle, diagnostics bodies, result evidence and capability manifests.
- Memory ingestion chunk ≤ ingestion body; emitted JSON is size-checked too.
- Default page, roster and search counts ≤ their corresponding maxima.
- One diagnostic batch fits the device quota; one event fits the body.
- UI feedback ≤ UI output ≤ model API response bytes.

### Protocol constants (not operator limits)

| Boundary | Fixed bounds | Peer |
|---|---|---|
| Transcript | 64 KiB per turn, 512 KiB per batch, 1–200 turns | Gateway transcript sender |
| Delegation ID | 1–256 Unicode code points | Gateway transcript sender |
| Text and result chunks | 8,000 UTF-8 bytes | Gateway advertised result capability |
| Opening context | 32 KiB per item, 128 items | Gateway silent opening receiver |
| Digests and keys | SHA-256 and 32-byte key widths | Storage schema and credential contracts |
| Per-run budget | 32 model calls, 500,000 input units, 100,000 output units, 128 steps, 16 MiB artifacts | Runner claim snapshot |

Transport probe and diagnostic client field widths, A2UI shape and identifiers,
archive compatibility ceilings, and exact-cardinality queries are protocol
or structural constraints, not model budgets. Timeouts, intervals, ports and
ratios remain in their existing configuration.

### Voice configuration

Set `ZOE_VOICE_GATEWAY_DIAL_ORIGIN` to the client-reachable HTTPS origin advertised in the [authenticated AVAS configuration response](media.md#apple-signaling-and-configuration). It is independent of Zoe's private gateway API URL.

## Persistence and idempotency

Keep previous known configuration and applied version. Record effective per-consumer configuration identity. Retain immutable release identity, retained configuration/data inventory and explicit backup/rollback references.

## Failure and recovery

Probe/save/reload failure rolls back the whole change, not a partially selected route. A working consumer does not prove another is configured; report truthful failure. Health/startup success is not end-to-end success.

## Implementation locations

[Operator configuration](../../../cmd/zoe/configure.go), [menu/operations](../../../cmd/zoe/operations.go), [runner](../../../internal/runner/engine.go), [deployment](../../../deploy/linux/), [voice deployment](../../../deploy/voice/README.md).

## Verification obligations

Verify probe/save/reload failures with atomic rollback, effective model+effort equality across Worker route/provider/agent/session, consumer separation, retained network/key/data and explicit reset/backup/restore evidence; real end-to-end task/result/voice/UI success separately from health. Verify probes against the runtime endpoint, 401/403 stop without retry, transient-failure backoff growth and cap, persistence across restart, and `zoe status` reporting. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
