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

## Unresolved boundaries

- Spoken intent confirmation recognizes only `confirm`, `confirm and continue`, `yes, proceed`, and the three exact Chinese equivalents in `control.IsExplicitConfirmation`. Bare yes/no and spoken cancellation are not implemented here.
- Hard-impact admission still lacks a real task/run/action digest, expected-version scope, and expiry challenge. Ordinary append is rejected as `confirmation_invalid`; the test retains this negative authorization check. No spoken yes/no resume/cancel mechanism was invented.
- `classifyCommittedTurn` runs triage before its high-impact rule; high impact skips Steward model I/O, not triage. The eventual single update persists category and decision together, including `unclassified`/`ask` on triage failure. Progress skips Steward model I/O but persists `dispatch`, not an empty decision. Therefore the reported persistent NULL category/empty decision cannot be attributed to either short-circuit alone. Initial row-read failure or ignored classification-write failure remains unobservable in existing logs; live command receipts, handoff events, and timing are required to distinguish them.
- `ExecuteCommand` already owns the sole database connection before `waitForTriageCache`; this stalls classifier persistence until the wait ends. The reproduction still passes because its cache/stored-category route resolves recall. This verified contention is not proof of the reported missing recall job and was not changed speculatively.

These are component/source-review observations only; the acceptance boundary is defined in the [evidence register](../evidence.md#evidence-categories-and-acceptance-boundary).
