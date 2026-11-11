# Voice lab results

Offline transport-fingerprint findings are separate from these historical live
results: [Codex feasibility](FINGERPRINT_FEASIBILITY.md). The new approximate Codex
profile was later used for the P9, P10, items, and event-identity runs below; the earlier runs used the older profile.

Observations from live runs against the upstream AVAS realtime service, 2026-11-07. One pool account, through its stored egress proxy, one call at a time, synthetic speech only. Samples are small (usually three calls per cell), so results show tendencies, not rates. The `instructions` text used in most runs is `cases/instructions_v2.txt`.

Not covered: human voices, interruption of speech already being spoken, sending context while the user is silent, call duration limits, a second delegation in one call.

P5-P10 historical findings are below. Later GPT-Live probes supersede the inference that a rejected malformed function-output frame means the event type is unsupported; see [the follow-up](#gpt-live-follow-up--2026-11-12).

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
- Limits: reading Codex source established client limits of 128 items and 8192 tokens. These are not measured gateway/upstream ceilings; those ceilings remain undetermined. Existing Zoe opening-context budgets still apply.

## Consequences for design

- Historical proposal, not deployed policy: classify every user turn to avoid missed cancel/progress delegation. The later [delegation decision](../../docs/architecture/decisions/0012-voice-conversation-layer.md#delegation-is-upstreams-responsibility-decided-2026-11-11) classifies only delegated sentences; upstream speech cannot be prevented. The [register target](../../docs/architecture/decisions/0013-context-register.md) is **Accepted, not implemented**.
- Personal facts placed in `initial_items` are answered by the model without asking Zoe, so a lookup route is reached only for facts that are not in the items.
- The items are a snapshot taken when the call starts.

## Probes P5-P10 and later runs (2026-11-08 to 2026-11-11)

Account 4, Codex-style fingerprint profile (approximate, see FINGERPRINT_FEASIBILITY.md), synthetic speech, small samples.

- P5/P8 (official append channels): `session.instructions`, `thinking`, and `commentary` appends were rejected by upstream in 18 of 18 calls; the error lists the frame types the server accepts. `session.update` (instructions or initial items) was also unusable. At this checkpoint only `session.context.append` and `delegation.context.append` had working mid-call shapes; the follow-up below establishes additional accepted types.
- P6 (`<|SpawnThinking|>` template in instructions): it did not make cancel, progress, or personal questions delegated.
- P7: any `acknowledgement_delay_ms` value made the sideband connection fail.
- P9: `response.create` was rejected 6 of 6 ("requires a session with Responses delegation").
- P10: `delegation.function_call_output.create` failed 9 of 9 with "Missing required parameter: 'item'"; the item shape is untested.
- Empty `initial_items`, personal information: delegated 3 of 36 across the v2, v4, and v5 instruction texts.
- Rule only in `initial_items` (role `developer` or `user`): personal information delegated 23 of 24. The same rule in `instructions` had no effect.
- Combined rule (cancel, progress, personal) with the v2 instructions: cancel delegated 3 of 6, progress 4 of 5, chat sentences over-delegated 0 of 18. No instruction-free control group was run.
- Event identity (P6 with the rule items, three utterances in one call): the user `turn.created`/`turn.done` carry the same turn ID. `delegation.created` carries it as `user_bidi_turn_id`; its own `item.id` has a different prefix and value. `input_transcript.added` items are per-fragment IDs. `delegation.created` came before the user `turn.done` in the measured call (about 470 ms for the first utterance). The assistant turn began 3 to 9 ms after the user `turn.done` in all measured calls.
- P6 optional `--p6-items-file` seeds `initial_items`; without it P6 seeds none.
- Empty instructions, no items, one request per call (three calls each): weather delegated 2 of 3 and answered with a question about the city once; web search delegated 3 of 3. Upstream never gave weather or search content itself in these six calls. Real human speech and interruption were not tested.
- With the rule item present and no task running, upstream still said "stopped" after a withdrawal. Only Zoe can supply true task state.
- Synthetic speech sometimes shortens an utterance in transcription (a progress question was heard without its first syllable in one delegated turn).

## GPT-Live follow-up — 2026-11-12

Source: operator-supplied AVAS lab investigation following the runs above; exact follow-up gateway/Zoe revision not supplied. Public-document findings are source review, not measured AVAS compatibility. Samples are synthetic speech and tiny; observations were not rerun for this documentation update.

- Verified in supplied observations: accepted event types/shapes were `session.context.append`, `delegation.context.append`, `delegation.function_call_output.create`, and `response.item.create`. Public GPT-Live `session.thinking.append` and `session.commentary.append` were rejected with `invalid_value`. This supersedes the earlier inference from missing-`item` function-output errors; accepted transport does not prove spoken behavior for every payload.
- Verified in supplied public-doc review: GPT-Live calls the create field `input`, not `initial_items`. The `input` lab variant had no observations; it was not confirmed working.
- Rule placement: `instructions`, first user item, developer item, assistant item and second user item each delegated 7/9 to 9/9, with nine synthetic-speech calls per placement. Per-placement exact counts were not supplied; no ranking is established.
- The documented `Generate <|SpawnThinking|> when` template did not beat the current rule (two calls per cell). This is not a reliability estimate.
- Inferred: neither a better placement nor public/AVAS protocol equivalence follows from these samples. Human speech, production acceptance and gateway/upstream context ceilings remain unverified. Real-call attribution failures are recorded separately in the [evidence register](../../docs/acceptance/evidence.md#context-register-real-call-findings--2026-11-12).
