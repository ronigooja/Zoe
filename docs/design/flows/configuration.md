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

**Retry.** Every model request that is retried uses one shared exponential backoff: the delay doubles per consecutive failure from a consumer-specific base delay up to a consumer-specific cap, with jitter. Interactive consumers (Register, Steward, UI_Controller) use a short base and a bounded attempt count; background consumers (Compact, memory write, memory ingestion) use a longer base and retry without an attempt bound. Failure count and next-attempt time are persisted and survive restart. Authentication failure (401/403) is not retried: the consumer stops, reports the condition in `zoe status`, and resumes only after the operator reconfigures it. The server clears an authentication stop when it observes that the consumer's configuration changed (endpoint, model, effort or credential content, compared by digest, never by stored secret); an unchanged restart keeps the stop. `zoe status` reads retry state that the server publishes, not the server database. Transient failure (timeout, 429, 5xx, network error) retries under the backoff. UI_Controller malformed-output retries are set by `ZOE_UI_CONTROLLER_MAX_RETRIES` (0..3, default 0), do not count against the transient-error budget, and carry only the fixed validation category. A retry never replays an uncertain write blindly; see [Memory](memory.md#failure-and-recovery).

**Prompts.** Every instruction text sent to a model or to the voice upstream (Register routing, Compact, task summary, Steward, UI_Controller, the task display instruction, the voice upstream instruction and memory instructions) is a plain UTF-8 text file maintained in the repository under `deploy/prompts/`, published with the server release and installed under `/etc/zoe/prompts/<name>.txt`. The server reads each file once at startup and keeps no built-in copy; a missing, unreadable, empty, non-UTF-8 or oversized (more than 16000 bytes) file stops startup with a fixed error category. The memory worker independently loads `memory_ingest.txt`, `memory_write.txt` and `memory_custom.txt` once at its own startup, with the same directory override (`ZOE_PROMPT_DIR`), failure rules and immutable snapshot semantics; both loaders remove exactly one final newline. A changed file takes effect after restart. A voice upstream instruction file whose entire content is `off` disables that instruction. Deployment installs a prompt file that is absent, keeps an existing one, and replaces existing files only when the operator selects the overwrite option, which first backs each replaced file up as `<name>.txt.pre-<sha12>`. Deployment fails before switching if a required prompt file is missing or unreadable, and a dry run lists each file as install, keep, overwrite or identical without printing content. Prompt content, including memory instructions, is not part of a consumer's configuration identity, so changing it never clears an authentication stop. `zoe status` shows each prompt's path and byte size, never its content. The UI_Controller prompt must stay consistent with the output validation in [uictl](../../../internal/uictl/controller.go).

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

## Persistence and idempotency

Keep previous known configuration and applied version. Record effective per-consumer configuration identity. Retain immutable release identity, retained configuration/data inventory and explicit backup/rollback references.

## Failure and recovery

Probe/save/reload failure rolls back the whole change, not a partially selected route. A working consumer does not prove another is configured; report truthful failure. Health/startup success is not end-to-end success.

## Implementation locations

[Operator configuration](../../../cmd/zoe/configure.go), [menu/operations](../../../cmd/zoe/operations.go), [runner](../../../internal/runner/engine.go), [deployment](../../../deploy/linux/), [voice deployment](../../../deploy/voice/README.md).

## Verification obligations

Verify probe/save/reload failures with atomic rollback, effective model+effort equality across Worker route/provider/agent/session, consumer separation, retained network/key/data and explicit reset/backup/restore evidence; real end-to-end task/result/voice/UI success separately from health. Verify probes against the runtime endpoint, 401/403 stop without retry, transient-failure backoff growth and cap, persistence across restart, and `zoe status` reporting. Follow the [acceptance boundary](../../development.md#evidence-and-acceptance).
