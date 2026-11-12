# ADR 0012: Zoe-owned conversation layer for real-time voice

Status: direction proposed; routing and bounded recall decisions recorded 2026-11-06. Progress and recall have component implementations; real voice acceptance remains open. Extends [ADR 0009](0009-voice-steward-impact-policy.md). Evidence: [evidence register](../../acceptance/evidence.md#live-voice-call-avas-route--2026-11-05).

Status note (2026-11-12): routing/context, impact and held-confirmation policy, opening instructions, Steward references/actions, progress, reply language and memory cadence are **superseded-by-0013 (target)**, [Accepted, not implemented](0013-context-register.md). The descriptions below retain the deployed `bb07671` baseline, not implementation claims for 0013. Recall evidence and transport boundaries remain unless explicitly replaced; held-task controls are deployed history, not the target.

## Context

Observed on the AVAS route (one call each, single account):

- At that historical checkpoint every committed voice turn created a `zoe-steward` task. A task answered about 2 minutes later; during that time the upstream model could only improvise (a filler such as "soon").
- The upstream model rewrote a long Steward answer and contradicted it (rain probability).
- Memory is loaded once at call start as opening context ([ADR 0006](0006-memory-authority-and-derived-stores.md)). There is no memory access during a call.
- Probe on 2026-11-06: after `delegation.created`, a `delegation.context.append` on channel `speakable` was confirmed by upstream in 139 ms and spoken verbatim, preceded by an upstream-added lead-in sentence. Speech of about 30 characters took about 6 s to play; this is playback time, not waiting.

The problem is control, not speed: the upstream model leads the conversation and Zoe is an asynchronous side effect.

## Decision

1. Zoe owns what is said. The upstream model listens and speaks; it does not decide content that Zoe can answer.
2. Every client-target delegation is routed by the Zoe control plane into one of two paths:
   - **Fast path**: answered within a latency budget from local authoritative data only (memory, task state, simple facts). The answer is returned as a short `speakable` context append.
   - **Slow path**: starts or appends to a `zoe-steward` task as in [ADR 0009](0009-voice-steward-impact-policy.md). The immediate `speakable` reply states that work has started. Attach behavior is **superseded-by-0013 (target)**; deployed and interim behavior is defined in the [task-manager contract](#deployed-roster-aware-task-manager).
3. Deployed progress chooses four fixed sentences from task status/age only, without result content or delivery state. Scope task links to the current voice session, workspace and owner and exclude tasks created more than `Config.ProgressMaxTaskAge` ago (default `DefaultProgressMaxTaskAge`, six hours). Current-call running tasks report elapsed seconds; otherwise current-call completed or awaiting-user tasks updated within 30 minutes report the result ready, and no qualifying task reports nothing running. Apply the same query at admission and delivery recomputation. This can misreport a clarifying question as a delivered result; the register/Steward progress route is **superseded-by-0013 (target)**, not deployed.
4. Results are written for speech: short sentences, key figures first, one `speakable` append per chunk. Steward output that is long is summarized by Zoe, not by the upstream model.
5. Delivery channel: when a call is live, results use the gateway's sideband or `/context` channel; otherwise a notification. Spoken delivery never marks a task complete; only authoritative task events do ([ADR 0009](0009-voice-steward-impact-policy.md) item 5).
6. Authority is unchanged: voice and the conversation layer create no commands directly. Impact-level rules of [ADR 0009](0009-voice-steward-impact-policy.md) apply to the slow path only; the fast path is read-only.
7. The gateway stays transport-only (media, accounts, sideband). It carries no conversation logic.

When creating a voice session on the AVAS route, Zoe sends two things in the create request: `instructions` (identity and speech style only) and `opening_context` items, which the gateway forwards as upstream `initial_items` (role `user`). The first item is the fixed delegation rule: cancel, progress, and any question about the user personally are delegated, not answered by the model. At most five shallow memory facts (`key:content`) and the current tasks (`active`, `queued`, and `waiting`/`awaiting_user` updated within 30 minutes, de-duplicated by title) follow, within the item and character budgets; the rule is never dropped. Delivery is recorded as `delivered`, `unverified`, or `unsupported` from the gateway response. No post-create push or retry is used. This is transport evidence only, not proof that upstream obeys (configuration: [deployment](../../operations/deployment.md#voice-delegation-instruction)). Measured behavior is in [lab results](../../../tools/voice_lab/RESULTS.md).

The opening-context source is **superseded-by-0013 (target)**; the deployed snapshot above has no persistent conversation register.


## Routing: stateless triage (decided 2026-11-06)

Observed on the AVAS route ([evidence](../../acceptance/evidence.md#instruction-and-routing-probes-avas-route--2026-11-06)): an instruction sent at call start controls what upstream says, but not what it puts into `delegation.created`, which is the raw transcript. Upstream cannot label intent, and it delegates every request, including "is it done yet".

Decision:

1. Deployed: for client-target delegations, the upstream voice model alone decides when to delegate; Zoe does not classify chat. Zoe routes each received delegation with a stateless LLM call returning `progress`, `recall`, `memory_write`, `memory_read`, `task`, or `cancel`. The legacy `memory` category is unknown and follows the existing retry/unclassified slow task fallback. Only when exactly one eligible held request exists, also offer `confirm` and `decline`, with its pending question as data beside the current utterance. The prompt contains definitions only, without example sentences. No history or accumulated context is sent. No keyword rules infer read/write or agreement semantics. See [held-task controls](#held-task-controls). Triage deletion and its three-route replacement are **superseded-by-0013 (target)**, not deployed; `unclear` is not a target route.
2. The model endpoint, model, and key file are configured like the memory model (`ZOE_TRIAGE_BASE_URL`, `ZOE_TRIAGE_MODEL`, `ZOE_TRIAGE_API_KEY_FILE`); empty base URL disables triage. The connection is long-lived and reused; requests are independent.
3. For committed-turn classification, retry a failed or unrecognized result once within the remaining classification budget. The triage HTTP timeout defaults to 3000 ms (`ZOE_TRIAGE_TIMEOUT_MS`, range 200–10000). Exhausted failures persist `unclassified` and retain the existing slow task path. Failure never drops a request.
4. Deployed impact classification follows [ADR 0009](0009-voice-steward-impact-policy.md). A category never grants authority; high-impact requests keep the waiting/confirmation path regardless of category. This policy is superseded for the target by ADR 0013.
5. Classification and gateway calls never run inside a database transaction.
6. Fast paths read local evidence only: `progress` reads the task table; `recall` follows the bounded read-only route below; `memory_read` uses the existing owner-scoped context assembler with the question as query. Memory routes do not create Worker tasks. Memory writes remain blocked as described below.
7. Historical instruction-style wording is no longer the deployed fixed-reply contract: Zoe sends plain Chinese statements, including fixed error/progress speech. Upstream can still add or rewrite wording; fixed payloads are not a speech-verbatim guarantee.

The recall date resolver exists in `internal/recall/dates`; weeks start on Monday and the caller timezone is `Asia/Shanghai`.

Historical prerequisites observed before transcript ingestion:

- Recall needs stored history. Zoe persists only the turns delegated to it (about 10 voice turns) and task entries; upstream-answered turns are never forwarded. `RecordVoiceOutcome` stores a summary source without date or subject; the server timezone is `Asia/Shanghai`.
- Semantic recall needs working extraction. The memory model key is currently rejected (HTTP 401 "Invalid API key" from the configured endpoint), `memory_model_calls` is 0, and both Qdrant collections hold 0 points. Qdrant retrieval is not wired into `Repository.Search`, which is SQLite keyword scoring.

The stateless classifier routes committed voice handoffs; failures retain the slow path. High-impact admission remains authoritative.

When a handoff arrives before classification completes, wait outside the command
transaction for the durable category and decision. `ZOE_VOICE_CLASSIFICATION_TIMEOUT_MS`
configures both the classification deadline and handoff wait budget (default 35000 ms,
range 100–60000). Both triage attempts share the remaining budget, without a
separate fast-path sub-deadline. Progress text is recomputed immediately before
every send, so delayed classification does not freeze elapsed seconds. Classification
and handoff waiting stay outside transactions and do not hold up unrelated turns.
Run Steward only for
`task`; progress, recall, memory_read and memory_write do not pay for a Steward call.
Do not run it speculatively: non-task requests would consume unnecessary model calls.

When the handoff budget expires before a durable decision exists, return retryable
`503 triage_timeout` without a receipt, task or reply. Log the turn row ID and wait
milliseconds with `budget_hit=true`; no schema migration is needed. A completed
model failure persists `unclassified` and retains the existing slow fallback.
Late classification only persists routing metadata; the retried handoff alone
admits effects, under the existing stable-turn uniqueness keys. Classification
logs include `triage_ms`, `steward_ms`, `total_ms`, budget status and storage-error
status, `attempts`, and `reason` (`none`, `triage_timeout`, `triage_error`,
`steward_timeout`, or `steward_error`), never
sentence text or model errors.

## Bounded recall route

When triage selects `recall` and the impact gate allows the read-only path, persist a holding reply and a turn-idempotent recall job without creating a task. Run model work outside transactions only after the holding delivery receipt. Reuse progress delivery leases, receipts, generation fencing and gateway retry limits for both plain `/context` messages. Recall text has a fixed read-as-is instruction; malformed markup or empty answers become the unavailable-check sentence defined below.

Resolve dates with the resolver above. Select summaries by start time in the half-open range and scope through session owner and workspace. Without a resolved date, select newest summaries; `ZOE_RECALL_SUMMARY_LIMIT` defaults to 20 (maximum 100). `ZOE_RECALL_HOLDING_TEXT` overrides the short default holding sentence. The existing `ZOE_ANSWER_*` client configuration supplies the model and total recall deadline (`ZOE_ANSWER_TIMEOUT_MS`, default 8000).

The answer model receives summary/excerpt data, never evidence instructions. A transcript request may name only selected summary IDs; read only their inclusive sequence ranges. Excerpts total at most 16 KiB of UTF-8 including role labels, with at most one additional model round. When no summaries exist or the answer reports `not_found`, say "I could not find that in our conversation records." When configuration, storage, timezone, model, deadline or validation failures prevent checking, say "I could not check the records right now, please try again." Both fixed sentences retain the existing English fallback. Logs contain job IDs, candidate counts, status codes and answer-call `elapsed_ms` only; private questions, summaries, transcript text and model error bodies are never logged.

Recall workers share a service-owned cancellation context and a four-slot semaphore, acquired before a durable claim. Stop cancels and joins them before storage closes; shutdown leaves unfinished jobs for recovery. Running jobs recover to the honest fallback at startup or after a clock-injected lease (default twice the total recall timeout). The claim timestamp uses the holding receipt's `updated_at`. Recovery atomically inserts the deterministic final reply ID and marks the job terminal, preserving any existing answer. Final persistence retries three times, then retries the fallback three times; persistent storage failures leave the running job eligible for later recovery. Pending jobs retain normal holding-first dispatch.

This is component behavior, not real-provider or gateway-speech acceptance. Durable holding/final delivery rows retain progress transport retry semantics, including an ambiguous-send duplication risk after a lost acknowledgement; delivery is not exactly once.

## Personal memory routes

When triage selects `memory_read` and the hard impact gate allows it, reuse the
recall job lifecycle, holding-first delivery, per-turn uniqueness, cancellation,
leases, and bounded worker slots. Distinguish the route by its holding receipt
intent. Query the existing context assembler with workspace, owner as recipient,
question, and the default memory token budget. This is source-verified SQLite
retrieval, not Qdrant semantic search. Use the existing `ZOE_ANSWER_*` client with
a separate memory prompt: the question and items are untrusted data, never
instructions; output only `answer` or `not_found`. Empty retrieval or model
`not_found` sends a fixed plain Chinese statement that nothing is saved.
Configuration, retrieval, model, malformed-output, deadline, and recovered-job
failures send a fixed plain Chinese statement that the check was not possible.
An answer is sent as a short Chinese lead-in plus the answer text. Every sentence
sent upstream is a plain statement with no command to the model and no English
prefix. Logs contain IDs, counts, and fixed statuses only. The read runs with the
narrowest scope it needs (`memory:read:personal`) for the owner only.

When triage selects `memory_write`, the current component sends a fixed plain Chinese
statement that it cannot save once per turn through the plain fast-reply
queue, without a Worker task or save claim. Write ingestion is blocked:
`Repository.RecordVoiceOutcome` creates a source-only extract job, but
`derivedService.materializeTx` requires an existing current claim/revision and
rejects source-only jobs as stale. `CreateProposal` accepts already-extracted
subject/key/content against an existing source, not raw user sentences. Missing
contracts are raw-source materialization with owner scope, a turn-idempotent
transactional ingestion entry point, and a successful extraction/save receipt
that distinguishes worker/model unavailability. Do not create placeholder claims
or report a queued/unprocessable job as saved. The requested successful write
route and confirmation remain unimplemented; no live memory config is changed.

These routes have component evidence only; real memory-provider availability,
prompt-injection resistance in model behavior, and gateway speech acceptance
remain unverified.

## Stored call summary generation

When a session transitions to `ended`, migration 212 transactionally enqueues one job keyed by session ID and control-plane generation, including hangup, reaper, and context rotation. Existing historical ended sessions are not backfilled. A single context-cancellable goroutine in `zoe-server` reads the entire raw transcript outside transactions and calls `internal/summarize` using `ZOE_SUMMARY_*`. No separate service is required.

When a job is due, `ZOE_SUMMARY_SETTLE_SECONDS` delays the initial read after closure (default 60, range 0–600). A missing model leaves nonempty jobs pending with `model_not_configured`, without attempts or repeated error logging; configuring the model and restarting resumes them. Empty calls complete without model calls or rows. Model failures, timeouts, and invalid output retry at 1, 2, 4, and 8 minutes, then fail on attempt 5 with a fixed reason code. Model execution is limited to 10 minutes; abandoned durable claims expire after 11 minutes. Shutdown cancellation consumes no attempt.

When validating model output, strict JSON decoding rejects missing/unknown fields and trailing data. Topics and summaries must contain non-whitespace text. Each inclusive range must have both endpoints in the real transcript and `first_seq <= last_seq`; ranges must be ordered by `first_seq` and non-overlapping. Coverage is not required: gaps before, between, and after ranges are allowed, and missing interior transcript sequences do not invalidate a range with real endpoints. Empty output is invalid if any turn contains non-whitespace text (using Go `strings.TrimSpace`); zero-turn or whitespace-only calls complete without a model call or summary rows, even without model configuration. The model client and worker share this validator. A transaction replaces all session summary rows and marks completion together; each row's timestamps come from its first/last raw turns. Storage diagnostics never include raw errors or transcript/model text.

When new (not duplicate) turns arrive during or after the first summary, schedule exactly one replacement revision after the same settle delay, retaining previous rows until replacement commits. Multiple late turns coalesce into that revision. Turns arriving during or after the replacement remain in the authoritative transcript but do not trigger a third pass. The existing 10-minute ingestion window is unchanged. This bounded regeneration rule applies equally to initially empty calls; summary recall must not assume the derived store is exhaustive.

## Delegation is upstream's responsibility (decided 2026-11-11)

Zoe classifies only the sentences upstream delegated. It does not classify every final user transcript.

Reasons:

- Triage is stateless and sees one sentence. Without history it cannot tell, for example, a withdrawal of a task from a refusal of an offer. Classifying every sentence would increase misclassification, and the triage definitions have no "not a request" category, so unrelated speech would become tasks.
- The rule item in `initial_items` is the mechanism that moves cancel, progress, and personal questions toward delegation. Measured with synthetic speech: personal facts 23/24 delegated; with the combined rule, cancel 3/6, progress 4/5, and chat 0/18 over-delegated. No instruction-free control group was run and no real human speech was measured, so the rule's reliability is not established.
- Known gap: a cancel or progress question that upstream answers itself never reaches Zoe, and a running task is not stopped. This is accepted and observed through live calls.

Considered and not built: gateway emission of `user_audio_turn.final` for every final transcript, with a stable ID. Measured IDs, for later use: the user turn ID is stable from `turn.created` to `turn.done`; a delegation carries it as `user_bidi_turn_id`; the delegation `item.id` is a different value, and the gateway currently uses that value as the final-turn ID. `delegation.created` arrived about 470 ms before the user `turn.done` in one call. Upstream began speaking within about 10 ms after `turn.done`, so classification of a non-delegated sentence could only correct afterwards, never precede upstream's answer.

When Steward times out or fails, Zoe creates no task. It records the decision `error` on the turn and sends a fixed plain Chinese statement that processing failed and the developer should be told. The failure detail stays in logs (`steward_call` with `configured_ms`, `effective_ms`, `elapsed_ms`, and a sanitized `error_kind`), never in speech. High-impact requests keep the confirmation path regardless.

## Open questions (to measure before implementation)

<a id="stateless-task-manager"></a>

### Deployed roster-aware task manager

Deployed on `bb07671`: when a delegation reaches the low-impact task path, Steward
receives the current sentence and a newest-first roster of at most ten tasks.
It uses actions `new | attach | answer | none` and per-request aliases `t1..t10`;
titles are bounded to 60 runes, pending questions to 120, result excerpts to 200.
There is no accumulated conversation context. Migration 217 adds
`voice_turns.steward_action` and `steward_task_id`. Low-impact Steward `ask` no
longer creates a waiting task. High-impact requests bypass Steward and retain
the explicit-confirmation waiting path. Failure/timeout creates no task and
sends fixed speech meaning "processing error, report to the developer".
The roster references, register input and attach semantics are
**superseded-by-0013 (target)**; the alias-free design is not implemented.

Deployed `attach` to queued/active/waiting work uses normal stop plus a new task
with merged goal (old goal plus new sentence). A complete task produces a new task
carrying the previous result. Retain this interim behavior until the runner
delivers appended entries to running work; the target, completed source investigation and remaining live-verification limits
are defined in [ADR 0013 decision 5](0013-context-register.md#5-steward-and-task-references).

When configured, `ZOE_STEWARD_BASE_URL`, `MODEL`, `API_KEY_FILE`, `API_STYLE`,
`REASONING_EFFORT`, `THINKING`, and `TIMEOUT_MS` (each with the `ZOE_STEWARD_`
prefix) select an independent model. Base URLs do not require `/v1`; auto style
tries `/responses` then `/chat/completions` on unsupported-route responses.
The default Steward HTTP and whole-decision timeout is 30000 ms (range
200-60000), within the shared classification deadline defined in
[routing](#routing-stateless-triage-decided-2026-11-06). Both API-style attempts
share the decision deadline. The total budget is not a guarantee that triage and
Steward can both consume their full timeout; operators can raise it explicitly.
Errors, timeouts, disabled configuration and unknown outputs create no task.
Hard high-impact markers take precedence and preserve explicit confirmation.
The Mac API client uses shared `URLSession` without a custom request timeout;
about 60 s of client waiting is inferred from its default, not measured live.

### Held-task controls

This section describes deployed `bb07671` controls. Target cancellation and Worker answers replace these controls under [ADR 0013](0013-context-register.md#5-steward-and-task-references); target impact confirmation is deleted.

When a live call has exactly one held task, accept model-classified `confirm` or
`decline` only for that owner, workspace, session and generation. Eligible tasks
are `waiting` or `awaiting_user`, with an `awaiting_message` or
`explicit_confirmation` question, created during this session within
`Config.ConfirmationWindow` (default ten minutes). Migration 216 persists task
ID, version and question on the answering turn before model I/O. Recheck all three
inside handoff's transaction; stale, ambiguous or other-session bindings never
authorize anything. No sentence keywords decide agreement or refusal.

For high-impact held admission, `confirm` reuses `task.append` with a canonical
server-owned confirmation. A hard-impact admission creates a `task:admit` scoped challenge:
its run reference is `admission:<task-id>`, not a runtime run; its digest binds
task ID, goal, question, expected version, scope and expiry. The existing append
validator records `task_confirmations`, clears the interaction and queues work.
This authorizes only starting this task, never future runtime permissions.
Runtime requests retain a real run/action digest, `runtime:permission` scope,
version and expiry. No broader voice principal or endpoint permission is added.

When triage returns `decline`, reuse `task.stop` with a fixed plain Chinese reply.
For `cancel`, stop only the newest nonterminal task linked to this owner and live
generation through the normal stop path, then state the true state afterwards.
Upstream cannot be prevented from speaking its own words, so Zoe supplies the
facts and never lets a stop be reported before it happened. States: no task
linked (nothing is running); stop requested and the live run still winding down;
task confirmed canceled; task already complete or failed (stated as such, not as
stopped). Stop cancels tasks without live runs directly; live runs receive the
existing desired-stop control and become canceled only after runner termination.
A progress question while a stop is pending says the stop was requested, not that
work continues. Stable-turn retries do not repeat effects.

When a held answer is unknown or times out, no task starts or resumes. Re-ask
at most once per question/version with fixed text. A handoff deadline before
durable classification still returns retryable `503 triage_timeout`. Without an
eligible question, confirmation categories are not offered or accepted. Questions
and fixed replies use the durable queue; logs contain IDs and timing, never
utterance or question text. Component coverage is not live speech acceptance.

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
