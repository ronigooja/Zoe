# Live handoff component investigation — 2026-11-06

Baseline: `aa18a081cf4c25a8dcc9c336d207b43b31012c0c` (`origin/main`). Environment: local macOS; synthetic gateway, model, and previous-call summary only.

When assessing the reported recall failure, `TestLiveRecallHTTPProductionAnswerWiring` passed on unchanged production code. It uses the reported recall sentence, a delayed classifier returning recall, Steward dispatch, `answer.FromEnv` with a fake HTTP model, and another ended session owned by the same caller. It checks one holding, one recall job, no task, and a summary-grounded delivered answer. The reported missing-job root cause is **not reproduced**; this is not a recall repair or live acceptance.

Source review verified that the sentence matches no high-impact marker, answer configuration is passed into `voice.Config`, and Compose forwards answer/summary base URL, model, key-file, reasoning, thinking, and timeout variables with mounted secrets. Compose does not forward the optional `ZOE_SUMMARY_SETTLE_SECONDS` override; the application default still applies. Recall has no nil-answerer admission early return. Migration 213 has no holding foreign key; no such foreign-key explanation is supported.

`TestLiveHoldQuestionDeliveredOnce` failed against unchanged production code at `live_handoff_test.go:122` for Steward delivery without the read-as-is prefix and at `:118` for zero hard-impact question rows (line numbers before formatting). The production hard-impact conditional intentionally suppressed the question insert. The repair queues both holds once per stable turn, adds read-as-is instructions, and explicitly states that hard-impact requests remain paused and cannot yet be authorized by spoken assent. Scoped execution authorization is unchanged.

The following single-package command passed after the repair, without a SQLite busy failure:

```sh
nice -n 19 go test -count=1 -p 1 -parallel 2 -timeout 120s ./internal/voice -run '^(TestLiveRecallHTTPProductionAnswerWiring|TestLiveHoldQuestionDeliveredOnce|TestStewardSpokenConfirmationUsesExistingAppend|TestStewardAskDeliversOnceAndExistingAppendResumes|TestVoiceHandoffHTTPFailClosedForHighImpactRequests)$'
```

The before-repair invocation selected only the two `TestLive*` tests and failed as described. Tests were serialized; no builds, race tests, whole-repository tests, hosted CI, deployment, or real-provider calls were performed. Formatting and `git diff --check` passed.

## Delayed-classification follow-up

After the live receipt correction, the same baseline implementation with a
1200 ms injected classifier delay, 1500 ms Steward delay, and handoff 300 ms
after commit failed at pre-format `live_handoff_test.go:90`:
`holding=0 jobs=0 tasks=1`. The earlier fast fake therefore did not reproduce
the timing defect. This supersedes the earlier unresolved recall conclusion.

Source-verified mechanism: `ExecuteCommand` reads receipt/auth state inside a
transaction before invoking handoff; storage has one connection. Handoff's
3100 ms cache wait holds that connection while classification's 3000 ms context
and fresh 1000 ms write context compete for it. A classifier result cannot be
persisted/cached in time; an empty category falls through to task admission.
Classification then may write a late recall category, without rerouting the
already-created task. The supplied 134 ms live receipt interval is not explained
by this source path and was not independently verified.

The repair adds bounded command preflight outside the transaction, durable
category polling, a default four-second configurable total budget, task-only
Steward calls, and redacted timing logs. Budget expiry returns retryable
`triage_timeout` without effects, while completed model failures retain slow
fallback. No speculative Steward calls are made. Existing stable-turn uniqueness
and task links fence retry effects; late classification itself creates none.

The first focused post-change run passed delayed recall, budget-expiry/no-effect,
late-result retry/deduplication, log redaction, B delivery, existing spoken
confirmation, classifier-failure fallback and stale-generation checks, but the
overall invocation failed on an old test expecting zero high-impact replies.
That stale B assertion now expects the one explicit-confirmation reply. No
SQLite busy failure occurred in that run. Additional non-task/Steward checks
cover progress/recall/memory skipping Steward and memory retaining task admission.

The follow-up single-package voice invocation passed the new category/Steward
matrix, corrected high-impact assertion, existing replay routes, all
`TestRecallRoute` modes and the progress HTTP route. The single-package control
preflight test passed, proving preparation can use the sole DB connection and
reject without handler execution or a receipt. Focused server composition and
triage-wiring tests passed. Every invocation used the prescribed nice/count/p/
parallel/timeout constraints; there were no SQLite busy failures. No builds,
hosted runs, race tests or whole-repository tests were run.

## Remaining boundaries (initial observations)

- Spoken intent confirmation recognizes only `confirm`, `confirm and continue`, `yes, proceed`, and the three exact Chinese equivalents in `control.IsExplicitConfirmation`. Bare yes/no and spoken cancellation are not implemented here.
- Hard-impact admission still lacks a real task/run/action digest, expected-version scope, and expiry challenge. Ordinary append is rejected as `confirmation_invalid`; the test retains this negative authorization check. No spoken yes/no resume/cancel mechanism was invented.
- `classifyCommittedTurn` runs triage before its high-impact rule; high impact skips Steward model I/O, not triage. The eventual single update persists category and decision together, including `unclassified`/`ask` on triage failure. Progress skips Steward model I/O but persists `dispatch`, not an empty decision. Therefore the reported persistent NULL category/empty decision cannot be attributed to either short-circuit alone. Initial row-read failure or ignored classification-write failure remains unobservable in existing logs; live command receipts, handoff events, and timing are required to distinguish them.
- The initial fast reproduction did not expose connection contention. The delayed follow-up above supersedes that observation and establishes the component failure and repair; the live 134 ms receipt interval remains unexplained.

These are component/source-review observations only; the acceptance boundary is defined in the [evidence register](../evidence.md#evidence-categories-and-acceptance-boundary).
