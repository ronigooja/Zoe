# ADR 0012: Zoe-owned conversation layer for real-time voice

Status: direction proposed; routing and bounded recall decisions recorded 2026-11-06. Progress and recall have component implementations; real voice acceptance remains open. Extends [ADR 0009](0009-voice-steward-impact-policy.md). Evidence: [evidence register](../../acceptance/evidence.md#live-voice-call-avas-route--2026-11-05).

## Context

Observed on the AVAS route (one call each, single account):

- Today every committed voice turn creates a `zoe-steward` task. A task answered about 2 minutes later; during that time the upstream model could only improvise (a filler such as "soon").
- The upstream model rewrote a long Steward answer and contradicted it (rain probability).
- Memory is loaded once at call start as opening context ([ADR 0006](0006-memory-authority-and-derived-stores.md)). There is no memory access during a call.
- Probe on 2026-11-06: after `delegation.created`, a `delegation.context.append` on channel `speakable` was confirmed by upstream in 139 ms and spoken verbatim, preceded by an upstream-added lead-in sentence. Speech of about 30 characters took about 6 s to play; this is playback time, not waiting.

The problem is control, not speed: the upstream model leads the conversation and Zoe is an asynchronous side effect.

## Decision

1. Zoe owns what is said. The upstream model listens and speaks; it does not decide content that Zoe can answer.
2. Every client-target delegation is routed by the Zoe control plane into one of two paths:
   - **Fast path**: answered within a latency budget from local authoritative data only (memory, task state, simple facts). The answer is returned as a short `speakable` context append.
   - **Slow path**: starts or appends to a `zoe-steward` task as in [ADR 0009](0009-voice-steward-impact-policy.md). The immediate `speakable` reply states that work has started.
3. Progress is a first-class input. A question about a running task (for example "is it done yet") is a fast-path question answered from the task record, never from model guesswork. When answering progress, scope task links to the current voice session, workspace and owner and exclude tasks created more than `Config.ProgressMaxTaskAge` ago (default `DefaultProgressMaxTaskAge`, six hours). Current-call running tasks report elapsed seconds; otherwise current-call completed or awaiting-user tasks updated within 30 minutes report the result ready, and no qualifying task reports nothing running. Apply the same query at admission and delivery recomputation.
4. Results are written for speech: short sentences, key figures first, one `speakable` append per chunk. Steward output that is long is summarized by Zoe, not by the upstream model.
5. Delivery channel: when a call is live, results use the gateway's sideband or `/context` channel; otherwise a notification. Spoken delivery never marks a task complete; only authoritative task events do ([ADR 0009](0009-voice-steward-impact-policy.md) item 5).
6. Authority is unchanged: voice and the conversation layer create no commands directly. Impact-level rules of [ADR 0009](0009-voice-steward-impact-policy.md) apply to the slow path only; the fast path is read-only.
7. The gateway stays transport-only (media, accounts, sideband). It carries no conversation logic.

When creating a voice session, Zoe pushes its standing delegation instruction through the gateway before returning SDP, records delivery per session/media generation/instruction hash, and retries a failed push once on connection; this is transport evidence only, not proof that upstream obeys (configuration: [deployment](../../operations/deployment.md#voice-delegation-instruction)).


## Routing: stateless triage (decided 2026-11-06)

Observed on the AVAS route ([evidence](../../acceptance/evidence.md#instruction-and-routing-probes-avas-route--2026-11-06)): an instruction sent at call start controls what upstream says, but not what it puts into `delegation.created`, which is the raw transcript. Upstream cannot label intent, and it delegates every request, including "is it done yet".

Decision:

1. For client-target delegations, the upstream voice model alone decides when to delegate; Zoe does not classify chat. Zoe routes each received delegation with `zoe-triage`: a stateless LLM call that receives only the current sentence and returns one word: `progress`, `recall`, `memory`, or `task`. Any delegation outside the first three categories is `task`. The prompt contains definitions only, without example sentences. No history, no accumulated context.
2. The model endpoint, model, and key file are configured like the memory model (`ZOE_TRIAGE_BASE_URL`, `ZOE_TRIAGE_MODEL`, `ZOE_TRIAGE_API_KEY_FILE`); empty base URL disables triage. The connection is long-lived and reused; requests are independent.
3. For committed-turn classification, retry a failed or unrecognized result once within the remaining classification budget. The triage HTTP timeout defaults to 3000 ms (`ZOE_TRIAGE_TIMEOUT_MS`, range 200–10000). Exhausted failures persist `unclassified` and retain the existing slow task path. Failure never drops a request.
4. The impact classification of [ADR 0009](0009-voice-steward-impact-policy.md) stays authoritative. A category never grants authority; high-impact requests keep the waiting/confirmation path regardless of category.
5. Classification and gateway calls never run inside a database transaction.
6. Fast paths read local evidence only: `progress` reads the task table; `recall` follows the bounded read-only route below; `memory` remains unimplemented. Recall does not create a task or fall back to a semantic search.
7. Upstream owns wording: Zoe pushes short instruction-style `speakable` text (including a holding sentence during slow work) and upstream phrases it. Verified once only; see the evidence.

The recall date resolver exists in `internal/recall/dates`; weeks start on Monday and the caller timezone is `Asia/Shanghai`.

Historical prerequisites observed before transcript ingestion:

- Recall needs stored history. Zoe persists only the turns delegated to it (about 10 voice turns) and task entries; upstream-answered turns are never forwarded. `RecordVoiceOutcome` stores a summary source without date or subject; the server timezone is `Asia/Shanghai`.
- Semantic recall needs working extraction. The memory model key is currently rejected (HTTP 401 "Invalid API key" from the configured endpoint), `memory_model_calls` is 0, and both Qdrant collections hold 0 points. Qdrant retrieval is not wired into `Repository.Search`, which is SQLite keyword scoring.

The stateless classifier routes committed voice handoffs; failures retain the slow path. High-impact admission remains authoritative.

When a handoff arrives before classification completes, wait outside the command
transaction for the durable category and decision. `ZOE_VOICE_CLASSIFICATION_TIMEOUT_MS`
configures both the classification deadline and handoff wait budget (default 4000,
range 100–30000). Both triage attempts share the remaining budget, without a
separate fast-path sub-deadline. Progress text is recomputed immediately before
every send, so delayed classification does not freeze elapsed seconds. Classification
and handoff waiting stay outside transactions and do not hold up unrelated turns.
Run Steward only for
`task`; progress, recall and memory do not pay for a Steward call. Do not run it
speculatively: non-task requests would consume unnecessary model calls. Memory
still uses ordinary task admission; it has no implemented fast route.

When the handoff budget expires before a durable decision exists, return retryable
`503 triage_timeout` without a receipt, task or reply. Log the turn row ID and wait
milliseconds with `budget_hit=true`; no schema migration is needed. A completed
model failure persists `unclassified` and retains the existing slow fallback.
Late classification only persists routing metadata; the retried handoff alone
admits effects, under the existing stable-turn uniqueness keys. Classification
logs include `triage_ms`, `steward_ms`, `total_ms`, budget status and storage-error
status, `attempts`, and `reason` (`none`, `triage_timeout`, or `triage_error`), never
sentence text or model errors.

## Bounded recall route

When triage selects `recall` and the impact gate allows the read-only path, persist a holding reply and a turn-idempotent recall job without creating a task. Run model work outside transactions only after the holding delivery receipt. Reuse progress delivery leases, receipts, generation fencing and gateway retry limits for both plain `/context` messages. Recall text has a fixed read-as-is instruction; malformed markup or empty answers become the honest not-found sentence.

Resolve dates with the resolver above. Select summaries by start time in the half-open range and scope through session owner and workspace. Without a resolved date, select newest summaries; `ZOE_RECALL_SUMMARY_LIMIT` defaults to 20 (maximum 100). `ZOE_RECALL_HOLDING_TEXT` overrides the short default holding sentence. The existing `ZOE_ANSWER_*` client configuration supplies the model and total recall deadline (`ZOE_ANSWER_TIMEOUT_MS`, default 8000).

The answer model receives summary/excerpt data, never evidence instructions. A transcript request may name only selected summary IDs; read only their inclusive sequence ranges. Excerpts total at most 16 KiB of UTF-8 including role labels, with at most one additional model round. No summaries, no model, errors, deadlines, unsupported results and repeated transcript requests produce the honest not-found sentence. Logs contain job IDs, candidate counts and status codes only; private questions, summaries, transcript text and model error bodies are never logged.

Recall workers share a service-owned cancellation context and a four-slot semaphore, acquired before a durable claim. Stop cancels and joins them before storage closes; shutdown leaves unfinished jobs for recovery. Running jobs recover to the honest fallback at startup or after a clock-injected lease (default twice the total recall timeout). The claim timestamp uses the holding receipt's `updated_at`. Recovery atomically inserts the deterministic final reply ID and marks the job terminal, preserving any existing answer. Final persistence retries three times, then retries the fallback three times; persistent storage failures leave the running job eligible for later recovery. Pending jobs retain normal holding-first dispatch.

This is component behavior, not real-provider or gateway-speech acceptance. Durable holding/final delivery rows retain progress transport retry semantics, including an ambiguous-send duplication risk after a lost acknowledgement; delivery is not exactly once.

## Stored call summary generation

When a session transitions to `ended`, migration 212 transactionally enqueues one job keyed by session ID and control-plane generation, including hangup, reaper, and context rotation. Existing historical ended sessions are not backfilled. A single context-cancellable goroutine in `zoe-server` reads the entire raw transcript outside transactions and calls `internal/summarize` using `ZOE_SUMMARY_*`. No separate service is required.

When a job is due, `ZOE_SUMMARY_SETTLE_SECONDS` delays the initial read after closure (default 60, range 0–600). A missing model leaves nonempty jobs pending with `model_not_configured`, without attempts or repeated error logging; configuring the model and restarting resumes them. Empty calls complete without model calls or rows. Model failures, timeouts, and invalid output retry at 1, 2, 4, and 8 minutes, then fail on attempt 5 with a fixed reason code. Model execution is limited to 10 minutes; abandoned durable claims expire after 11 minutes. Shutdown cancellation consumes no attempt.

When validating model output, strict JSON decoding rejects missing/unknown fields and trailing data. Topics and summaries must contain non-whitespace text. Each inclusive range must have both endpoints in the real transcript and `first_seq <= last_seq`; ranges must be ordered by `first_seq` and non-overlapping. Coverage is not required: gaps before, between, and after ranges are allowed, and missing interior transcript sequences do not invalidate a range with real endpoints. Empty output is invalid if any turn contains non-whitespace text (using Go `strings.TrimSpace`); zero-turn or whitespace-only calls complete without a model call or summary rows, even without model configuration. The model client and worker share this validator. A transaction replaces all session summary rows and marks completion together; each row's timestamps come from its first/last raw turns. Storage diagnostics never include raw errors or transcript/model text.

When new (not duplicate) turns arrive during or after the first summary, schedule exactly one replacement revision after the same settle delay, retaining previous rows until replacement commits. Multiple late turns coalesce into that revision. Turns arriving during or after the replacement remain in the authoritative transcript but do not trigger a third pass. The existing 10-minute ingestion window is unchanged. This bounded regeneration rule applies equally to initially empty calls; summary recall must not assume the derived store is exhaustive.

## Open questions (to measure before implementation)

### Stateless task manager

When a delegation reaches the task path, the Zoe-owned Steward receives only the
current transcript sentence, with no history, memory, project layer, result
validation, or result rewriting. Its exact output is `dispatch` or `ask`;
whitespace is stripped, but case changes, punctuation, explanations and unknown
values are rejected. Zoe supplies a fixed English or Chinese intent question;
unsupported languages currently use the English fallback.

When configured, `ZOE_STEWARD_BASE_URL`, `MODEL`, `API_KEY_FILE`, `API_STYLE`,
`REASONING_EFFORT`, `THINKING`, and `TIMEOUT_MS` (each with the `ZOE_STEWARD_`
prefix) select an independent model. Base URLs do not require `/v1`; auto style
tries `/responses` then `/chat/completions` on unsupported-route responses.
The default Steward HTTP timeout is 2 seconds, within the shared classification
deadline defined in [routing](#routing-stateless-triage-decided-2026-11-06).
Errors, timeouts, disabled configuration and unknown outputs dispatch ordinary
requests. Existing hard high-impact markers take precedence and always ask.

When Steward asks an intent question, the task uses existing `awaiting_message`
and `task.append` resume semantics. Exact server confirmation phrases in a later
turn on the same owned call append to the waiting task. Questions use the durable
voice fast-reply delivery queue with the fixed read-as-is instruction. When a hard
high-impact hold is admitted, enqueue its question through the same queue once
per stable turn, followed by an explicit paused/not-yet-authorizable notice; do
not imply that work has started. A high-impact hold remains the existing scoped
`explicit_confirmation` boundary; creating a valid pre-run scoped challenge is
still unimplemented and cannot be replaced by an intent reply.

When Worker executes a task, the compatibility profile selects the widest
existing tier, `external_paths`, including edit, shell and subagents; the existing
OpenCode harness and isolation-policy identifiers remain unchanged. The task
model is configured, never hardcoded. **Risk:** this permits host-changing shell
work and is not a sandbox or per-tool approval boundary. External-directory
rules remain directory-scoped and unknown MCP tools remain denied by the
existing ruleset. This is a bootstrap permission policy, not security acceptance.

- Fast-path time to first audio end to end, not only upstream acknowledgement.
- Whether long `speakable` content is summarized or altered by upstream; whether the lead-in sentence can be suppressed.
- Behavior with several delegations in one call and with barge-in.
- Which memory reads are allowed on the fast path and their size bound.

## Consequences

- Replaces "create a task for every turn" with routing; task creation becomes a decision.
- Adds a read-only fast-path service in the control plane with a latency budget and a fallback to the slow path on timeout.
- The AVAS route depends on a reverse-engineered protocol with no stability contract; each upstream change must be re-measured. If upstream cannot be made to speak Zoe's text verbatim, the fallback is Zoe-owned speech recognition and synthesis, which is a separate decision.
