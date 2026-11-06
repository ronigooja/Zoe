# ADR 0012: Zoe-owned conversation layer for real-time voice

Status: proposed direction; routing decision recorded 2026-11-06; component implementation is described below, not product acceptance. Extends [ADR 0009](0009-voice-steward-impact-policy.md). Evidence: [evidence register](../../acceptance/evidence.md#live-voice-call-avas-route--2026-11-05).

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
3. Progress is a first-class input. A question about a running task (for example "is it done yet") is a fast-path question answered from the task record, never from model guesswork.
4. Results are written for speech: short sentences, key figures first, one `speakable` append per chunk. Steward output that is long is summarized by Zoe, not by the upstream model.
5. Delivery channel: when a call is live, results use the gateway's sideband or `/context` channel; otherwise a notification. Spoken delivery never marks a task complete; only authoritative task events do ([ADR 0009](0009-voice-steward-impact-policy.md) item 5).
6. Authority is unchanged: voice and the conversation layer create no commands directly. Impact-level rules of [ADR 0009](0009-voice-steward-impact-policy.md) apply to the slow path only; the fast path is read-only.
7. The gateway stays transport-only (media, accounts, sideband). It carries no conversation logic.


## Routing: stateless triage (decided 2026-11-06)

Observed on the AVAS route ([evidence](../../acceptance/evidence.md#instruction-and-routing-probes-avas-route--2026-11-06)): an instruction sent at call start controls what upstream says, but not what it puts into `delegation.created`, which is the raw transcript. Upstream cannot label intent, and it delegates every request, including "is it done yet".

Decision:

1. For client-target delegations, the upstream voice model alone decides when to delegate; Zoe does not classify chat. Zoe routes each received delegation with `zoe-triage`: a stateless LLM call that receives only the current sentence and returns one word: `progress`, `recall`, `memory`, or `task`. Any delegation outside the first three categories is `task`. The prompt contains definitions only, without example sentences. No history, no accumulated context.
2. The model endpoint, model, and key file are configured like the memory model (`ZOE_TRIAGE_BASE_URL`, `ZOE_TRIAGE_MODEL`, `ZOE_TRIAGE_API_KEY_FILE`); empty base URL disables triage. The connection is long-lived and reused; requests are independent.
3. Any error, timeout (default 2 s, no retry), or unrecognized output means `task`: the existing slow path. Failure never blocks or drops a request.
4. The impact classification of [ADR 0009](0009-voice-steward-impact-policy.md) stays authoritative. A category never grants authority; high-impact requests keep the waiting/confirmation path regardless of category.
5. Classification and gateway calls never run inside a database transaction.
6. Fast paths read local data only: `progress` reads the task table; `recall` reads dated call summaries and falls back to a Steward Qdrant search with a spoken holding sentence; `memory` reads memory. Only `progress` is in the first change; `recall` and `memory` are not implemented.
7. Upstream owns wording: Zoe pushes short instruction-style `speakable` text (including a holding sentence during slow work) and upstream phrases it. Verified once only; see the evidence.

The recall date resolver exists in `internal/recall/dates`; weeks start on Monday and the caller timezone is `Asia/Shanghai`.

Prerequisites not yet met:

- Recall needs stored history. Zoe persists only the turns delegated to it (about 10 voice turns) and task entries; upstream-answered turns are never forwarded. `RecordVoiceOutcome` stores a summary source without date or subject; the server timezone is `Asia/Shanghai`.
- Semantic recall needs working extraction. The memory model key is currently rejected (HTTP 401 "Invalid API key" from the configured endpoint), `memory_model_calls` is 0, and both Qdrant collections hold 0 points. Qdrant retrieval is not wired into `Repository.Search`, which is SQLite keyword scoring.

The stateless `zoe-triage` classifier is a configurable, startup-loaded client only; it is not wired into voice behavior. When enabled, it classifies one sentence through the configured Responses endpoint and callers retain the slow-path fallback on any failure.

## Stored call summary generation

When a session transitions to `ended`, migration 212 transactionally enqueues one job keyed by session ID and control-plane generation, including hangup, reaper, and context rotation. Existing historical ended sessions are not backfilled. A single context-cancellable goroutine in `zoe-server` reads the entire raw transcript outside transactions and calls `internal/summarize` using `ZOE_SUMMARY_*`. No separate service is required.

When a job is due, `ZOE_SUMMARY_SETTLE_SECONDS` delays the initial read after closure (default 60, range 0–600). A missing model leaves nonempty jobs pending with `model_not_configured`, without attempts or repeated error logging; configuring the model and restarting resumes them. Empty calls complete without model calls or rows. Model failures, timeouts, and invalid output retry at 1, 2, 4, and 8 minutes, then fail on attempt 5 with a fixed reason code. Model execution is limited to 10 minutes; abandoned durable claims expire after 11 minutes. Shutdown cancellation consumes no attempt.

When validating model output, strict JSON decoding rejects missing/unknown fields and trailing data. Topics and summaries must contain non-whitespace text. Each inclusive range must have both endpoints in the real transcript and `first_seq <= last_seq`; ranges must be ordered by `first_seq` and non-overlapping. Coverage is not required: gaps before, between, and after ranges are allowed, and missing interior transcript sequences do not invalidate a range with real endpoints. Empty output is invalid if any turn contains non-whitespace text (using Go `strings.TrimSpace`); zero-turn or whitespace-only calls complete without a model call or summary rows, even without model configuration. The model client and worker share this validator. A transaction replaces all session summary rows and marks completion together; each row's timestamps come from its first/last raw turns. Storage diagnostics never include raw errors or transcript/model text.

When new (not duplicate) turns arrive during or after the first summary, schedule exactly one replacement revision after the same settle delay, retaining previous rows until replacement commits. Multiple late turns coalesce into that revision. Turns arriving during or after the replacement remain in the authoritative transcript but do not trigger a third pass. The existing 10-minute ingestion window is unchanged. This bounded regeneration rule applies equally to initially empty calls; summary recall must not assume the derived store is exhaustive.

## Open questions (to measure before implementation)

- Fast-path time to first audio end to end, not only upstream acknowledgement.
- Whether long `speakable` content is summarized or altered by upstream; whether the lead-in sentence can be suppressed.
- Behavior with several delegations in one call and with barge-in.
- Which memory reads are allowed on the fast path and their size bound.

## Consequences

- Replaces "create a task for every turn" with routing; task creation becomes a decision.
- Adds a read-only fast-path service in the control plane with a latency budget and a fallback to the slow path on timeout.
- The AVAS route depends on a reverse-engineered protocol with no stability contract; each upstream change must be re-measured. If upstream cannot be made to speak Zoe's text verbatim, the fallback is Zoe-owned speech recognition and synthesis, which is a separate decision.
