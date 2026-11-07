# Voice lab results

Offline transport-fingerprint findings are separate from these historical live
results: [Codex feasibility](FINGERPRINT_FEASIBILITY.md). The new approximate Codex
profile has not been tested against AVAS; these results used the older profile.

Observations from live runs against the upstream AVAS realtime service, 2026-11-07. One pool account, through its stored egress proxy, one call at a time, synthetic speech only. Samples are small (usually three calls per cell), so results show tendencies, not rates. The `instructions` text used in most runs is `cases/instructions_v2.txt`.

Not covered: human voices, interruption of speech already being spoken, sending context while the user is silent, call duration limits, a second delegation in one call.

P5-P8 implementation adds offline fake-based coverage only. Append channels, the spawn prompt, and `acknowledgement_delay_ms` have not been tested against AVAS; no live findings are claimed for these probes.

## Protocol facts

- Create call: HTTP 201 with an SDP answer and a `Location` header ending in the call id (`rtc_...`). The sideband connects with 101. A SOCKS account proxy needs `python-socks` for the WebSocket client.
- The committed aiortc media path has no account-proxy integration and no TURN servers. The earlier claim that audio flows through the account proxy is unsupported by this implementation; system-level routing was not measured. Media needs the ordered data channel `oai-events`.
- Sideband event types seen: `session.started`, `session.input_audio.append`, `session.output_audio.delta`, `input_transcript.added`, `turn.created`, `turn.delta`, `delegation.created`, `output_transcript.added`, `turn.done`, `session.context.appended`, `delegation.context.appended`, `session.usage.updated` (about every 15 seconds), `error`.
- The final text of a user or assistant turn is `turn.done` with `turn.transcript` and `turn.role`. A delegation carries the full user sentence in `item.content[0].text`, `handoff_id`, `target: client`, and `user_bidi_turn_id`; it arrives about 1.2 seconds after the last transcript fragment.
- Synthetic Chinese speech is transcribed and does trigger delegations.
- No event marks a delegation as finished. A delegation id stays valid after it was answered and after later user turns; binding to it again is accepted.
- `delegation_item_id` is validated: an unknown id returns an `error` event (`invalid_request_error`, "Unknown delegation item id") and nothing is spoken.
- `delegation.ack_filler`: with `true` the model says a short waiting sentence after delegating; with `false` it stays silent after delegating; unset behaves like `true`.

## Delegation behavior with `instructions`

Seven utterances, one call each, two instruction versions.

- Delegated reliably in both versions: a weather request, a delete-files request. A personal-fact question was delegated in some runs and not in others.
- Not delegated in either version, repeated three times each with the second version: a progress question and a cancel request. The model answered itself every time, with and without an active task described in `initial_items`.
- A recall question about earlier calls was answered by the model ("cannot see earlier calls") with the first version and delegated with the second.
- Other synthetic voices gave the same picture: progress and cancel were not delegated reliably.
- Transcription errors occur with synthetic speech (the short progress question was heard as unrelated or near-homophone phrases in some runs); human speech may differ.

## Context sent after the call started

Same result sentence, three calls per cell.

- Unbound `session.context.append`: spoken in every run; wording matched in 2 of 3.
- `delegation.context.append` bound to the open delegation: spoken in every run; the model added its own words in 3 of 3.
- Prefixes: an English instruction-style prefix gave matching wording in 2 of 3; no prefix or a Chinese label gave 0 of 3. No wording was ever dropped. The project does not require verbatim speech; these figures describe the model's behavior only.
- State sentences change what the model says. After a cancel, asked "is it done", the model said it had stopped in 3 of 3 runs with a state sentence and in 1 of 3 without one (the other two implied the task was still running). Sending the state sentence 1, 6 or 12 seconds after the cancel gave correct answers in 9 of 9 runs. A sentence arriving after the user's next question was not tested. For a progress question during a running task, a state sentence made little difference.

## `initial_items`

- Items are read and used. With a one-item fact the model answered a personal question directly in 3 of 3 runs and did not delegate; without the item it delegated or said it could not know.
- Roles `user` and `developer` both worked. A relevant item was found among five, and at the end of an item of about 1,200 characters.
- Contradicting items: the later one was used, with no mention of the conflict (3 of 3).
- An item telling the model to ignore its rules and answer only one word was not followed (3 of 3).
- A fact absent from the items was not invented (3 of 3).
- Limits: the Codex client allows 128 items and about 8,192 tokens in total and rejects more. Larger sizes were not tested; Zoe follows the Codex values.

## Consequences for design

- Cancel and progress cannot depend on the model delegating them. The transcript already reaches Zoe, so Zoe has to classify each user turn and tell the model the true state through context.
- Personal facts placed in `initial_items` are answered by the model without asking Zoe, so a lookup route is reached only for facts that are not in the items.
- The items are a snapshot taken when the call starts.
