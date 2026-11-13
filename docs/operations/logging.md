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

At DEBUG, each Register, Steward, register compaction and memory batch call emits:

- `model_call_input`: full `instructions` and `input`, exactly as sent to the model.
- `model_call_output`: full raw `output`, before parsing, including invalid output.
- `model_call_result`: parsed `result` (route/action/selected request or memory IDs/counts, as applicable) and safe `error_kind`.

All three carry `component=register|steward|register_compact|memory_batch`, a unique logging-only `call_id`, `attempt`, `parent_call_id` and `user_hash`. Register memory follow-up is attempt 2 with the first call as parent. Hashes reuse the owner's SHA-256 prefix; raw user IDs are not logged. Steward receives its owner's hash through context, without adding identity to model input. Text is quoted/escaped on one line, never truncated by the logger. No model-client internals are instrumented.

## Gateway and storage boundary

The separate gateway uses `VOICE_LOG_LEVEL` and its always-on `VOICE_ZOE_FINAL_TURN_DEBUG_TEXT`, documented in the gateway repository. Zoe's level does not configure that service.

Zoe host logging rotation, retention and size limits are **NOT DETERMINED**. The gateway container uses Docker `local` logging with `max-size=10m` and `max-file=3`, per [`deploy/voice/compose.yaml`](../../deploy/voice/compose.yaml). Those limits do not establish Zoe host limits. DEBUG can emit large, private content; protect collected logs accordingly.

`deploy/linux/docker-compose.yml` forwards `ZOE_LOG_LEVEL` with a debug default. `expected-env.txt` intentionally does not require this optional setting: its verifier fails if a listed name is absent from a running container, including older hosts/releases.

## Verification boundary

Unit tests establish filtering, safe error classification and complete call-site text capture. They do not establish deployed settings, access controls, rotation or end-to-end acceptance; see [evidence categories](../acceptance/evidence.md) and [delivery status](../master-plan.md).
