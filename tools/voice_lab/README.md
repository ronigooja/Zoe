# Voice lab

Test-only Python client that talks to the upstream realtime voice service directly, the way the Codex CLI does. It replaces guessing with recorded observations. It is not part of the product, is never deployed, and does not import Zoe or gateway code.

Status: implemented. E0, E1 and E2 were run live through `run.py`. The E3 to E7 drivers in `lab/experiments.py` were not run live; the live observations for those topics came from throwaway scripts that are not kept, and are recorded in [RESULTS.md](RESULTS.md) together with what was not covered.

## Why

The gateway and Zoe were built on assumptions about the upstream protocol. Observed failures (the upstream rewrites text it should speak, does not delegate cancel or recall requests, and `opening_context` is recorded as unsupported) cannot be diagnosed through the full chain. The lab isolates the upstream: one process, one call, every event recorded.

## Scope

In scope:
- Create an AVAS call with an arbitrary `session` object.
- Join the sideband WebSocket and record every event.
- Send speech as microphone audio, and send context events on the sideband.
- Run the experiments below and write a result file per run.

Out of scope: Zoe, the gateway, memory, tasks, deployment, any product code. Findings feed back into Zoe and gateway designs only after a result file supports them.

## Where it runs

Upstream accounts exist only on the gateway host (`root@154.19.187.117`), and the account egress proxy is stored per account. The lab runs there, from a directory under `/tmp`, copied over `scp` from this folder. Nothing is installed globally.

Credentials are copied from the gateway, never from this repository:
- Account row: read-only copy of the gateway SQLite file (`accounts.access_token`, `accounts.proxy`, and `bridge_accounts.device_id`, joined by `bridge_accounts.account_id = accounts.id`, as verified in `internal/accounts/pool.go`).
- Decryption key: the gateway's token key (`VOICE_TOKEN_ENCRYPTION_KEY`, file `/opt/llm-auth-bridge/.secrets/voice_token_key`), 32 bytes as hex or base64.
- Sealed format, from gateway `internal/secretbox/box.go`: prefix `enc1.`, then raw URL-safe base64 of `nonce(12) || ciphertext`, AES-256-GCM, no additional data. A value without the prefix is plaintext.

Device identity uses a non-empty account device id first, then persona `oai-device-id`, and fails before network use if neither is available.

Rules:
- When recording results or displaying output, never print, log, or write a token, proxy URL, device id, key, or email. Result files carry an account label (`acct-<id>`) only. Pass all sensitive values to the recorder's redaction list; redaction applies to every file write, including JSON keys and nested values.
- Work on a copy of the database (`/tmp/voice-lab/accounts.db`), opened read-only, deleted at the end of the session.
- Use one account, one call at a time. Do not run while a real call is active on that account.
- The operator supplies the account id. The lab never selects or mutates account state (no status, cooldown, or last-used updates).

## Upstream protocol (facts and their sources)

Sources: Codex CLI source in `codex-rs/` (`codex-api/src/endpoint/realtime_call.rs`, `realtime_websocket/methods_frameless_bidi.rs`, `core/src/realtime_conversation.rs`, `prompts/templates/realtime/`) and the gateway (`internal/voice/service.go`). Items marked UNVERIFIED are expectations, not facts, until a result file shows them.

Create call, as the gateway does it today:
- `POST https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas`, JSON body `{"sdp": <offer>, "session": <session>}`.
- Headers: `authorization: Bearer <token>`, `openai-alpha: quicksilver=v2`, `originator: codex_cli_rs`, `content-type: application/json`, plus the browser-persona header set from gateway `authHeaders` (`oai-device-id`, `oai-session-id`, `oai-language`, user agent and client hints). The operator supplies persona values using `--persona-file PATH`, a local JSON object mapping header names to string values; no identity values are hardcoded. Required fields are `user-agent`, `oai-session-id`, and `oai-language`; the device identity comes from the account or persona. Other supported fields include `sec-ch-ua*`, `sec-fetch-*`, `oai-client-version`, `oai-client-build-number`, and the ordinary browser headers used by `authHeaders`. Credentials do not belong in this JSON file.
- The request goes through the account proxy when one is stored.
- Response: SDP answer in the body, call id in the `Location` header (last path segment, `rtc_...`).

Codex CLI sends the same create call as multipart (`sdp` and `session` parts) on the API route. The lab uses the JSON shape because that is what the gateway sends and the account route accepts. Whether the multipart shape behaves differently is UNVERIFIED and not tested.

`session` object, from Codex `session_json`:
- `instructions` (string): the system prompt. Codex fills it from `backend_prompt.md` plus an optional startup context block.
- `delegation`: `{"type": "client"}`, optionally `ack_filler` (bool). Meaning of `ack_filler` UNVERIFIED.
- `audio.output.voice`: voice name.
- `model`: the gateway sends `gpt-live-1-codex`.
- `initial_items`: list of `{"type":"message","role":<user|developer|assistant>,"content":[{"type":<input_text|output_text>,"text":...}]}`. Codex limits count and tokens (constants `REALTIME_INITIAL_ITEMS_MAX_COUNT`, `REALTIME_INITIAL_ITEMS_MAX_TOKENS` in `realtime_conversation.rs`).

Sideband:
- `wss://api.openai.com/v1/live/<call_id>` with `Authorization`, `openai-alpha: quicksilver=v2`, `originator`, and a user agent. Without the alpha header the handshake returns 400 (recorded in `docs/acceptance/evidence.md`, 2026-10-31).
- Events received (seen in gateway tests and logs): `delegation.created` (item with `id`, `target: client`, `content[].input_text`), `turn.done` (role and text), context-appended, transcript, audio-delta, usage.
- Events sent by the gateway: `session.context.append` and `delegation.context.append` (with `delegation_item_id`), both with `channel: "speakable"` and `content: [{"type":"input_text","text":...}]`, text split into pieces of at most 500 bytes.

Media: WebRTC. The lab sends microphone audio from a prepared file and records the audio it receives (duration only; no audio is stored).

Gateway behavior the lab must reproduce or contrast with:
- The gateway sets `openDelegation` to the id of the latest `delegation.created` (`service.go`, where it wires the final-turn mapper) and clears it only when the sideband closes. `/context` therefore uses `delegation.context.append` for every later message in the call, even after that delegation has been answered. Whether the upstream accepts that, and how it treats it, is UNVERIFIED. This is the first thing the lab measures.

## Experiments

Each experiment is one call, one result file. An experiment states the setup, the input, and the observation that decides it. No experiment changes Zoe or the gateway.

For experiments that use instructions, `--instructions-file PATH` overrides `cases/instructions.txt`; dry-run output includes only the instruction file name and text length, never its text.

E0. Connectivity. Create a call with an empty `instructions`, join the sideband, send a short silence, hang up. Pass: 2xx create, call id parsed, sideband 101, clean close. This validates the lab itself.

E1. Speech input. Same session, send a prepared utterance. Observe final user and assistant text from `turn.done.turn.transcript` (role from `turn.done.turn.role`), the delegated sentence from `delegation.created.item.content[0].text`, and event order; pass requires non-empty final user text and a delegation. Utterances are generated by a local text-to-speech tool and recorded in the result file by name. Prior probes got no `delegation.created` from synthetic speech (evidence register, 2026-10-31), so failure here is possible and is itself a result.

E2. Delegation by instructions. Run E1 with the proposed `instructions` text (stored in `cases/instructions.txt`) for these utterances: a greeting, a weather request, a progress question, a recall question, a personal-fact question, a delete request, a cancel request. Observe per utterance: delegation created or not, and what the model says aloud (from `turn.done` assistant text). Decides whether the rules are obeyed and which wording must change.

E3. Context append variants. After a delegation, send the same sentence three ways in separate runs: `session.context.append`, `delegation.context.append` bound to the open delegation, and `delegation.context.append` bound to an already answered one. Observe: assistant text compared with the sent text (exact, rewritten, absent), error events, and whether the reply is spoken at all. Decides the correct way to deliver a result and a fast reply.

E4. Delegation lifecycle. Record the full event sequence of one delegation from creation to the next user turn. Look for a completion or close event and for anything that marks a delegation as finished. Compare with Codex `CompletedHandoff` handling in `core/src/realtime_conversation.rs`. Decides whether Zoe or the gateway must close delegations explicitly.

E5. Wording sensitivity. Send the same content with and without an instruction prefix (the gateway currently adds none for fast replies except a `Read this to the user exactly as written:` prefix from Zoe). Observe rewriting. Decides what text format makes the upstream speak the content as given.

E6. `initial_items`. Create a call with one `user` item stating a fact, then ask for it by voice. Observe whether the model uses it, whether it delegates anyway. Repeat with role `developer`.

E7. `ack_filler`. Same as E2 with `delegation.ack_filler` set true and false. Observe any spoken filler and event differences.

P1. Cancel state: `probes.py --experiment P1 --audio DIR` joins `01.wav` (weather) and `02.wav` (cancel) with `--gap-s` silence, then sends `--state-file` bound to the open delegation `--delay-s` seconds after the second final user turn. It plays `03.wav` (progress) after sending state and records `progress_answer`; prepare these three WAVs in that order, independently of the E2 utterance set.

P2. Missing fact: `--experiment P2 --audio question.wav --items-file cases/items_plate_home.txt` seeds user facts and records delegation and final assistant text. Compare separate invocations with `--instructions-file cases/instructions_v2.txt` and `cases/instructions_v3.txt`; the human verifies that the question is absent from the facts.

P3. Mid-call instructions: `--experiment P3 --audio first.wav --second-audio later.wav --update-instructions-file cases/instructions_override.txt` sends one `session.update`, then plays the later utterance. `--update-trigger event` waits for the first delegation or final assistant answer; `--update-trigger timer --update-after-s 3` instead uses elapsed time after connection; all later event types, error messages/codes and final assistant text are retained for human judgment (mid-call AVAS support is UNVERIFIED).

P4. Mid-call facts: the same controls as P3, with `--experiment P4 --update-items-file cases/items_plate_home.txt`, send `session.update.session.initial_items` built from `role|text` lines. Observe acknowledgments, errors and later assistant text; acceptance is not inferred from a successful send.

P5. Append channels: weather `--audio`, then one frame after the first delegation using `--channel instructions|thinking|commentary`, `--bind none|open`, and `--text-file` (use `cases/append_state.txt` for state). Observe `spoken` during `--spoken-window-s` (default 6), then play `--second-audio`; retain sent frame, later event types, errors, and assistant text.
The frame shape follows [GPT-Live docs](https://developers.openai.com/api/docs/guides/live-conversations), not verified AVAS behavior. A successful send is not acceptance.

P6. Spawn prompt: `--instructions-file cases/instructions_spawn.txt --audio FILE_OR_DIR`, without seeded items. A directory joins consecutive `01.wav`, `02.wav`, etc. with `--gap-s`, using the P1 WAV helper.
Record each utterance's user transcript, delegation flag, and assistant text; match by turn id when available, otherwise final-user order/time. Overlapping or delayed turns require human review.

P7. Acknowledgment delay: weather `--audio`, optionally `--ack-delay-ms N` sets `delegation.acknowledgement_delay_ms`; absent means omit the field. `--empty-thinking` sends one empty thinking append bound to the first delegation.
Record `filler`, its transcript, and all errors over the five seconds after delegation. Delay and append support are UNVERIFIED on AVAS.

P8. Mid-call instruction append: alias of P5 with channel fixed to instructions and unbound, using `--text-file cases/append_override.txt`, followed by `--second-audio` after the spoken window.
Record later assistant text for human judgment; neither protocol acceptance nor compliance is inferred automatically.

For P1-P8, `--repeat N` defaults to three independent calls and result directories; stdout contains counts only. Each call is bounded by `--timeout-s` plus independent cleanup budgets, uses the normal account/persona redaction rules, and records evidence rather than semantic pass/fail; dry runs print file names and text lengths only. `probes.py` and `run.py` share the same CLI; no probes were run live during implementation. For P5/P7/P8, choose a timeout long enough for the delegation, observation window, and later utterance; a false spoken/filler flag with no window-complete flag is only a partial observation.

Order: E0, E1, then E2 to E7 only after E1 produces a delegation from synthetic speech. If E1 never does, the experiments switch to typed input if the sideband accepts a user text item (UNVERIFIED), and the plan is revised in this document before more code is written.

## Result files

One directory per run: `results/<UTC timestamp>-<experiment>/`:
- `events.jsonl`: every sideband event, one JSON object per line, with `t_ms` (monotonic milliseconds since recorder creation), `dir`, and `event` fields. Text fields are kept except redacted credentials. The recorder does not record request headers.
- `summary.md`: setup, input list, observations, pass or fail against the stated criterion, and anything unexpected. Non-ASCII result text is escaped to keep Markdown English-only; original non-secret text remains readable after JSON decoding in the JSON files.
- `session.json`: the `session` object sent, with `instructions` and item text intact.

`results/` is not committed. Findings that matter are summarized in the evidence register by a person, with the run timestamp.

## Layout

```
tools/voice_lab/
  README.md          this document
  requirements.txt   pinned third-party packages
  lab/               package: account, upstream, webrtc, sideband, record
  cases/             input text and prepared utterance scripts per experiment
  run.py             command line: one experiment per invocation
  tests/             offline unit tests (sealed-token decryption, event recorder, frame splitting)
```

## Dependencies

An offline Codex CLI transport-fingerprint investigation is recorded in
[FINGERPRINT_FEASIBILITY.md](FINGERPRINT_FEASIBILITY.md). Stock curl_cffi 0.17.0
did not match either reference TLS hash; the live transports and default persona
remain unchanged pending a decision about approximate versus exact parity.

Dependencies are pinned in `requirements.txt`. The gateway host has Python 3.11 and none of these installed; installation goes into a virtual environment under `/tmp/voice-lab`, removed afterwards. Whether the chosen WebRTC library negotiates audio and ICE with the upstream through the account proxy is UNVERIFIED and is the main implementation risk; E0 exists to find out.

When using SOCKS proxies, `websockets` requires `python-socks`.

Pinned choices in `requirements.txt`: `cryptography` for AES-GCM, `websockets` for asynchronous sideband connections, `aiortc` for WebRTC media, and `httpx[socks]` for HTTP with HTTP/SOCKS proxy support. Live dependencies must be imported lazily; offline tests require only the standard library and `cryptography`.

## Tests

Offline tests only, run locally, no network and no credentials:
- Decrypt a sealed token produced with the documented format and a fixed key.
- Reject a wrong key and a truncated value.
- The recorder writes one line per event, in order, with offsets.
- Text splitting matches the gateway rule (pieces of at most 500 bytes on UTF-8 boundaries).
- Result files never contain the strings passed as secrets (redaction test).

Live runs are never part of CI.

Run the account and recorder tests from this folder with `nice -n 10 python3 -m unittest discover -s tests -p 'test_*.py'`. If a temporary environment is needed for offline tests, use `/tmp/voice-lab-venv`; never install globally. These tests use generated fixtures only, not account databases or keys.

## Open points

- Whether the `session` JSON route needs fields beyond the four the gateway sends: compare with a Codex multipart request body before E0 if E0 fails.
- Audio format the upstream expects (sample rate, channels): UNVERIFIED; E0 and E1 determine it.
- Hanging up cleanly so the account is released: the gateway owns that logic today; the lab must close the peer connection and the sideband and record the result.
