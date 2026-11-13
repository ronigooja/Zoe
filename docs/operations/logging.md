# Logging standard

Applies to Zoe server and runner process diagnostics, not client diagnostic storage.

## Levels and fields

Set `ZOE_LOG_LEVEL=debug|info|warn|error`; the default is **debug**. Values are case-insensitive. An invalid value emits `logging status=invalid_level default=debug error_kind=config` at WARN and uses debug without echoing the value. Output remains on stderr through the standard Go logger. Lines prepend `level=<name>` before the existing event text; existing metadata event names, field order and substring matches remain intact.

| Level | Meaning | Allowed data |
| --- | --- | --- |
| ERROR | Failed operations | Metadata and allowlisted `error_kind` or error type, never raw error messages |
| WARN | Degraded, rejected or recoverable operations | Metadata and safe error classification |
| INFO | Operational metadata | Time, status, route, elapsed time, counts, safe IDs and user hashes |
| DEBUG | Content-bearing diagnostics | Full model instructions, input, raw output and parsed results, plus metadata |

**Content text is allowed only at DEBUG.** This includes user speech, compact records, restatements, model input/output and text returned upstream. Tokens, API keys, credentials, Authorization headers, key-bearing endpoints and HTTP request/client dumps are forbidden at every level. Call sites select permitted fields; there is no redaction engine or keyword policy. Never pass arbitrary errors to logging functions: error messages can contain private input.

## Events

Existing metadata events include `register status=`, `memory_batch status=`, `register_call`, `steward_call`, `runner startup`, `voice_routing`, `voice_classification_wait`, `voice_routing_wait` and `voice recall`. Call timing summaries remain INFO; separate failures use ERROR and recoverable conditions use WARN.

At DEBUG, Register, Steward and register compaction calls emit the following events;
memory HTTP calls emit input and raw output when those stages are reached:

- `model_call_input`: full `instructions` and `input`, exactly as sent to the model.
- `model_call_output`: full raw `output`, before parsing, including invalid output.
- `model_call_result`: parsed `result` (route/action/selected request or memory IDs/counts, as applicable) and safe `error_kind`.

Components include `register`, `steward`, `register_compact`, `memory_search`, `memory_write` and `memory_ingest`. Events carry a logging-only `call_id`, `attempt`, `parent_call_id` and `user_hash` when available. Register routing makes one call, with no memory follow-up round. Hashes reuse the owner's SHA-256 prefix; raw user IDs are not logged as metadata. Steward receives its owner's hash through context, without adding identity to model input. Text is quoted/escaped on one line, never truncated by the logger. Sources: [logging.go](../../internal/logging/logging.go), [memory client](../../internal/memoryapi/client.go).

When inspecting memory traffic, Go DEBUG input/output contains the full HTTP JSON payload, including its opaque person identifier, and raw worker response; the client does not emit a parsed `model_call_result`. The Python worker logs full write/ingest model input and raw output at DEBUG and metadata timing/status at INFO ([person_memory.py](../../services/memory/person_memory.py)). Its HTTP transport libraries stay at WARNING and Mem0 content logs are suppressed unless DEBUG is enabled ([worker.py](../../services/memory/worker.py)). Go defaults to DEBUG; Python defaults to INFO, independently. Do not infer worker log level from the server's setting.

## Gateway and storage boundary

The separate gateway uses `VOICE_LOG_LEVEL` and its always-on `VOICE_ZOE_FINAL_TURN_DEBUG_TEXT`, documented in the gateway repository. Zoe's level does not configure that service.

Zoe host logging rotation, retention and size limits are **NOT DETERMINED**. The gateway container uses Docker `local` logging with `max-size=10m` and `max-file=3`, per [`deploy/voice/compose.yaml`](../../deploy/voice/compose.yaml). Those limits do not establish Zoe host limits. DEBUG can emit large, private content; protect collected logs accordingly.

`deploy/linux/docker-compose.yml` forwards `ZOE_LOG_LEVEL` with a debug default. `expected-env.txt` intentionally does not require this optional setting: its verifier fails if a listed name is absent from a running container, including older hosts/releases.

## Verification boundary

Unit tests establish filtering, safe error classification and complete call-site text capture. They do not establish deployed settings, access controls, rotation or end-to-end acceptance; see [evidence categories](../acceptance/evidence.md) and [delivery status](../master-plan.md).
