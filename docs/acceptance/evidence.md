# Acceptance evidence register

These retained observations still support current implementation conclusions. Each record retains its source version, environment, result, and limitations; earlier author-reported checks are not newly rerun by this documentation change. The [pre-restructure source record](https://github.com/ronigooja/Zoe/blob/95ee55a5bbd8dccfb7f14af9f9703bb83727da6c/docs/master-plan.md) supplies provenance for local logs and review reports that have no public CI artifact.

## Current OpenCode acceptance gates

The Zoe-owned runtime remains unaccepted until evidence covers: streaming provider finish and token/usage propagation through the bridge; in-flight crash recovery without duplicate effects; and runner-enforced tool-step/time/budget limits. The four uncertain historical runs were abandoned by operator decision, so no migration gate remains for them. These are open gates, not claims established by the historical records below.

Known limitations of the current source (PR #4, hosted CI passed):

- Runner unit coverage is reduced: the restored runner tests are about 15–20% of the pre-removal suite.
- `toolEvidence` is unverified and reads only the final assistant message, not the whole session.
- Legacy `runs.harness` and `tasks.harness` column defaults remain `'codex'`; production paths set the value explicitly.

## Historical superseded AO/Codex baseline

The AO/Codex records in this register are retained as historical evidence only. They are superseded by the Zoe-owned OpenCode runtime and must not be used as current deployment, acceptance, or recovery evidence.

## Evidence categories and acceptance boundary

This register is the authoritative source for the distinction between source review, fixtures, hosted CI, installation/startup, and end-to-end acceptance. A document, source review, fixture, CI result, installation result, health response, or component check is evidence of that activity only; it is not product acceptance or milestone acceptance unless the applicable acceptance gate is explicitly completed.

When adding a record, state the source revision, environment, action or command, result, and limitations; link immutable CI or source records and omit keys, account data, private message text, and raw audio. The [acceptance matrix](requirements.md#stable-requirement-matrix) records the remaining gates.

### Where to add a record

Append new dated records below the existing dated sections, keeping their original headings and anchors. Put lengthy command, revision, test-count, environment, and limitation details in a file under [`evidence/`](evidence/) and link it from the index; keep each component-verification table cell within 600 characters.

## Chat-first and continuous-voice baseline reconciliation

The approved documentation baseline removes the generic ordinary-result Accept/Reject workflow and the previous client interface description, pending a redesign. Continuous voice may be attempted automatically after connection and identity initialization, subject to OS microphone permission, audio policy, application lifecycle, network, account-pool, and server limits; text remains the fallback. The baseline also defines server-owned interaction state, waiting-task `task.append`, authoritative `assistant_message` content, scoped explicit conversational confirmation for high-impact actions, and `task.accept_result` as compatibility only. The API name `awaiting_user` remains documented as a compatibility name. Native clients do not present detailed error text: they show concise actionable states and automatically return redacted, encrypted diagnostics through the authenticated diagnostic endpoint when available; this applies to voice, connection, task, transport, infrastructure, storage, and other client error paths.

This record is a requirements and architecture reconciliation only. It does not establish that the Apple client, server contracts, continuous voice, permissions, account lifecycle, authoritative message persistence, confirmation semantics, or end-to-end flows are implemented or accepted. The existing component, hosted, installation, and real-device limitations below remain in force; no product milestone is accepted by this documentation change.

The concise-error/automatic-diagnostic rule is an approved product and architecture requirement, not an acceptance claim. Current diagnostic queue and authenticated upload coverage is recorded below; client-wide suppression of detailed error text, UI recovery behavior, and real-device upload remain implementation and acceptance work until separately verified.

## Component verification

Gateway creation-outcome attestation is implemented and unit-tested in the voice package; tests use an HTTP fake gateway.
The tests cover `never_established`, non-quarantine outcomes, 422 rejection, sweep idempotence, and source-session fencing.
This evidence does not verify the implementation against a real gateway or establish production voice acceptance.

| Area | Source/environment and observed checks | Acceptance limits |
|---|---|---|
| Control plane and runner | The integrated source, fixture, and independent capability checks passed at the recorded checkpoints; the live historical runtime attempt remains uncertain and production deployment, migration fencing, and complete scenario acceptance remain open. See the [full evidence record](evidence/control-plane-and-runner.md). | A successful real OpenCode task chain with provider/tool/artifact evidence and confirmed termination; recovery against a real OpenCode deployment; provider-side usage/cost evidence; production deployment and cross-host migration fencing; complete scenario acceptance. |
| Budget enforcement | Targeted owner and independent reviews passed local ledger/control checks, while provider-backed usage, tool-step, artifact, and hard-deadline enforcement remain unverified. See the [full evidence record](evidence/budget-enforcement.md). | Production per-provider-call usage, tool-step and artifact enforcement, hard-deadline fencing, and provider-backed cost accounting remain unverified; see the [full evidence record](evidence/budget-enforcement.md). |
| Memory and export | Component composition, bounded export, restore-candidate, cache/temp, and backup-manifest checks passed on the recorded local revisions; live OpenCode/provider behavior and physical external-store deletion were not established. See the [full evidence record](evidence/memory-and-export.md). | The maintenance user's current authority generation/head/hash remains the trust root; journal continuity does not prove every restore transition independently. Real Mem0/Qdrant cleanup and physical external-store deletion remain pending; unsupported resources stay `adapter_missing` and cannot be manually cleared. Production backup erasure, a measured retention guarantee, and restore rehearsal remain open. |
| Voice | `PATH=/usr/local/go/bin:$PATH go test ./...` passed on the integrated source candidate after commit `c0380c7`, including voice, acceptance, and server packages. This verifies local code paths only. | The selected Space3044 response does not expose account-lease identity or trusted account-scoped termination evidence. The adapter remains fail-closed; safe pool account reuse, real gateway behavior, interruption, device, and voice-to-task acceptance remain unverified. |
| Native Apple clients | Source review and the recorded 2026-10-24 macOS build/install/startup and reachability checks passed; APNs, current-key flows, iOS devices, and real-device voice/media acceptance remain open. See the [full evidence record](evidence/native-apple-clients.md). | Current-key authenticated macOS app flows, distribution signing/notarization, iPhone/iPad installation, APNs delivery, and real-device voice/media acceptance. |
| Apple interface redesign | The recorded Linux/API checks, 67-test Swift suite, fresh macOS/iOS builds, syntax and documentation checks passed at the stated checkpoints; the checks do not type-check Swift or render the UI. See the [full evidence record](evidence/apple-interface-redesign.md). | The installation refresh establishes fresh Swift tests and macOS/iOS app-target builds, macOS local installation/startup, and Launchpad registration only. Screenshots across the native interface requirements, core UI flow tests, VoiceOver/Dynamic Type/keyboard checks, real iPhone/iPad devices, and real services remain open. No complete redesign UI or product acceptance is claimed. |
| Capabilities | The integrated trigger→control/engine→historical fake runtime HTTP fixture passed through authorized acceptance, terminal report, and slot release. Owner and independent reviewer both passed the lifecycle test; the independent command was `go test ./internal/capabilities -run TestCapabilityTriggerRunnerAOResultChain -count=1`. Isolated Docker recovery and revocation checks passed on synthetic workloads (76.52 s and 26.62 s respectively). These local tests do not exercise real OpenCode or external business actions. | Real AO-backed trigger execution, real external actions, production service lifecycle/deployment and recovery. |
| Backup and recovery | The recorded Go and Python backup/export/recovery checks passed on candidate `d9f97d7e21b81bf62b4f7132698761ac3703bf85` and related local revisions; fixtures remain local and do not prove production recovery or deletion. See the [full evidence record](evidence/backup-and-recovery.md). | Preserve the operator-maintained current generation/head/hash as the trust root; journal continuity does not prove every restore transition independently. Complete real external adapter checkpoints and physical deletion; physical host migration; production host/systemd isolation; measured RPO/RTO; production backup/restore and full data-erasure acceptance. |
| Operator CLI and QR connection | Dependency version/checksums resolved without compilation. Shell syntax and whitespace checks passed for release/install changes. The later [hosted records](#hosted-runtime-and-installation-verification) establish integrated Go/race, staging, Swift, and app-build checks on their stated revisions. | Actual systemd/Compose change and rollback behavior, release installation, physical iPhone/iPad scanning, and real OpenCode/provider execution. No service deployment or external CLIProxyAPI change is established by this candidate. |
| Linux deployment | On 2026-10-24 the ARM64 images built and the final release started locally; runner authentication, Linux/Mac reachability, Qdrant readiness, and independent checks passed. See the [installation source record](https://github.com/ronigooja/Zoe/blob/95ee55a5bbd8dccfb7f14af9f9703bb83727da6c/docs/master-plan.md#historical-local-installation-evidence--2026-10-24). The pinned memory-model artifacts downloaded and hash verification passed; both Qdrant collections initialized. | Memory-job processing, real OpenCode/model task execution, production security/isolation acceptance, operational recovery, and migration. |

## Client voice diagnostics — 2026-10-29

Source review and local checks covered the authenticated Go endpoint, migration, redacted-field rejection, bounded event persistence, and Swift source type-checking for the diagnostics queue/API client files. The Go compile-only sweep passed with `GOCACHE=/tmp/zoe-gocache go test ./... -run '^$'`; focused diagnostics endpoint tests passed. Full Go execution remains environment-limited here by restricted listener, cache, and temporary filesystem permissions. Swift package tests and WebRTC app builds were not run; the new queue and API client sources were type-checked with `xcrun swiftc` on the local macOS SDK. Real-device voice, hosted CI, and end-to-end diagnostic upload remain open acceptance work.

## Instance-key and scoped native checks

[Apple run 40252480817](https://github.com/ronigooja/Zoe/actions/runs/40252480817) checked source `ede06a7686809b44e33981d23150466c8bdbe733` on a GitHub-hosted arm64 macOS 15.7.9 runner with Xcode 16.4 and Swift 6.1.2: 76 Swift tests, unsigned universal macOS archive, and unsigned iOS build passed with saved exit statuses 0. [Linux run 40252752759](https://github.com/ronigooja/Zoe/actions/runs/40252752759) checked `5679168d74ca1f8e271c2708088e7ff620b4f195` on GitHub-hosted Ubuntu, Go 1.26.9 linux/amd64, Linux 6.17.0-1022-azure: `go test ./...` and `go test -race ./internal/control ./cmd/zoe-server` passed. Apple source/workflow was identical between these two revisions. Exact commands are in the [Apple workflow at that revision](https://github.com/ronigooja/Zoe/blob/ede06a7686809b44e33981d23150466c8bdbe733/.github/workflows/apple.yml) and [Linux workflow](https://github.com/ronigooja/Zoe/blob/5679168d74ca1f8e271c2708088e7ff620b4f195/.github/workflows/linux.yml). Retrieved logs/exits/artifacts were recorded under `/tmp/zoe-single-key-verification/`. These checks support the current shared-key source baseline only; installation, protected transport, multi-device interaction, signing, real APNs, voice, and complete task flows remain open. Synthetic lifecycle fixtures performed no real external destruction. CI artifacts have limited retention; the immutable run/source links remain the provenance.

The earlier Settings repair at `042f289` passed 70 Swift tests, unsigned universal macOS archive, and unsigned iOS build in [Apple run 40204390633](https://github.com/ronigooja/Zoe/actions/runs/40204390633), on GitHub-hosted arm64 macOS 15.7.9, Xcode 16.4, Swift 6.1.2. Integrated `da29a8b` had the same committed tree. The artifact was ad-hoc signed, installed, and launched with strict signature/process checks. The user confirmed Command-comma and Account & Settings → Settings rendered correctly. This accepts only that reported blank-Settings presentation repair on that revision; it does not accept current connection, authorization, notification, export, or whole-interface flows. Installation evidence was moved to `~/Developer/Zoe/runs/zoe-settings-fix-ci-20261025/`, with `migration.json` preserving its historical paths. The [original scoped record](https://github.com/ronigooja/Zoe/blob/95ee55a5bbd8dccfb7f14af9f9703bb83727da6c/docs/master-plan.md#settings-window-repair--2026-10-25) provides provenance.

## Hosted runtime and installation verification

Hosted verification passed:

- [Linux run 40371027074](https://github.com/ronigooja/Zoe/actions/runs/40371027074), source `c31d8d3d3b7d6643768af946521f3be04a198b50`, passed `go test ./...`, the expanded race check across control/runner/AO/server/operator packages, and release/CLI staging checks. The initial race run found an unsynchronized counter in the existing AO timeout test; the fixture was corrected before this passing run. The final tests cover `409 worker_boot_failed` failure/capacity release and exact receipt replay; the same code with HTTP 503 remains uncertain.
- [Historical superseded AO run 40370123374](https://github.com/ronigooja/Zoe/actions/runs/40370123374), implementation source `5f99878b1ef684ea56c65f0342393fc67af0f703`, passed pinned patch application/tree fixtures, Codex/provider, session-manager, session-service, and HTTP-controller tests and built the ARM64 daemon. Runtime image sources were unchanged, so this run selected the binary-only artifact; the deployed `zoe-ao-codex:63cedecaedc2` image was retained.
- [Apple run 40370598915](https://github.com/ronigooja/Zoe/actions/runs/40370598915), the same implementation source, passed 82 Swift package tests, the unsigned macOS archive, and the unsigned iOS build. [Server run 40370133628](https://github.com/ronigooja/Zoe/actions/runs/40370133628) built the ARM64 server image and runner binary. Later source commits changed comments and tests only; deployed implementation matches this artifact source.

Artifact SHA-256 checks passed before deployment. At this recorded checkpoint, the server ran
`zoe-server:reconciliation-5f99878b1ef6`; systemd selects immutable runner
`/opt/zoe/runner/releases/reconciliation-5f99878/zoe-runner` (SHA-256
`944a9ac813f482880d503c93c3bd64e3e24375431b649362c3bd8814b531158a`)
and AO `/opt/zoe/ao/bin/ao-reconciliation-5f99878` (SHA-256
`127255213b445b2f1ac832d338f59e0bcecaa2fe05ce5582b29afc6f3fcc5bd6`).
The recorded control health endpoints returned `ok`; AO was
ready and its inventory reported complete empty sessions with a fresh process
start time. All three services were active, and the runner had zero restarts
at the final service check. Existing external CLIProxyAPI configuration was not
changed.

With the runner stopped and AO restarted, all four explicit per-run
reconciliation attempts on 2026-10-26 at 17:42:58 UTC correctly refused at Docker
inventory. An existing legacy AO-labeled container remained in `created` state, PID 0,
with no recorded process start; the conservative check also refuses any
uncorrelated managed runtime. Each refusal durably records the actor, run,
attempt/fence/instance, check time, AO incarnation and complete empty inventory,
and Docker refusal in `journal_reconciliations`. At this checkpoint, all four runs remained
`uncertain` and all four slots remained occupied, and the runner was restarted.
No journal, SQLite authority row, or container record was deleted. Recovery of
the old queue is **not accepted**; the retained container needs a separately
scoped lifecycle recovery decision before absence can be proved.

The CI macOS archive was copied to
`~/Developer/Zoe/runs/ao-reconciliation-20261026/`, signed ad hoc on the authorized
Mac with the installed application's existing no-entitlement signing boundary,
and installed at `/Applications/Zoe.app`. Strict signature verification and
installed/staged executable comparison passed, and a new `ZoeMac` process was
observed. The archive and signing logs are preserved in that task directory.
No local compilation or automated test suite ran. Native UI interaction and a
fresh minimum chat remain unverified; the user chose to perform chat verification.
No new legacy AO container/app-server/provider execution is claimed, and the retained
uncertain slots at that checkpoint prevented a fresh queued task from dispatching.

Controller CI logs and operational refusal outputs are retained under
`/tmp/zoe-reconciliation-20261026/`; persistent refusal evidence is in the runner
journal. See [deployment operations](../operations/deployment.md#zoe-runtime-uncertainty-reconciliation) for repeatable commands and acceptance boundaries.


## Chat recovery correction

The subsequent correction permits exact operator-selected never-started AO records and historical capability containers only after two stable complete inspections. It also gives AO background tasks their own identity/role and random launch labels so cleanup targets an exact launch. Ambiguous runtime evidence still fails closed. Sources: [runner reconciliation](../../internal/runner/reconciliation.go), the historical AO patch record (source revision and external archived review), and [original review record](https://github.com/ronigooja/Zoe/blob/95ee55a5bbd8dccfb7f14af9f9703bb83727da6c/docs/master-plan.md#chat-recovery-correction--2026-10-26).

**Source reviewed; remote recovery unverified.** The source review does not show these corrections deployed, the old queue reconciled, slots released, or a new live chat completed. The four previously refused runs must not be inferred recovered.

## Scoped CI verification

On source `d0a786ecac14681f5808ccdef1eb96dfdd02355e`,
[hosted run 40549291218](https://github.com/ronigooja/Zoe/actions/runs/40549291218)
passed scope-selection regressions (including renames, deletions, and unavailable
history), actionlint workflow syntax validation, documentation/checker regressions,
`go test ./...`, the existing authorization race package set, release/CLI-install
script regressions, Swift package tests, and independent unsigned macOS/iOS builds.
The Linux, Apple, and overall required-check gates passed. Checks ran on
GitHub-hosted Linux and macOS runners; no local build or test suite ran.

Independent source review findings were corrected before this run. Artifact
metadata showed only logs, and both release jobs were skipped as expected for
routine verification. This establishes the full selection path and current
component builds/tests; path-specific omissions are protected by selector
regressions. Manual release packaging, installation, external-service integration,
end-to-end acceptance, and remote branch-protection configuration were not
verified or changed. See [CI verification](../development/verification.md) for
scope and manual entry points.

## Voice deployment configuration repair — 2026-10-29

On the user-identified Zoe host `136.0.11.159`, the configured HTTPS gateway's
authenticated `/v1/health` and `/v1/voice/config` returned HTTP 200. The latter
contained the expected public configuration fields. The running server's old
`/etc/zoe/active.compose.yaml` omitted the voice secret mount and secret-file
mapping despite the host environment already configuring both. This left the
server without its gateway credential, triggering the generic unavailable response.

The deployment Compose file was backed up and updated with the voice secret
mount and mapping from the repository template. The first recreation exposed
root-only ownership of the private credential; ownership was corrected to the
server's UID/GID 65532 while retaining mode 0600. The recreated server was running
with zero restarts, `/healthz` returned `ok`, and a value-free inspection confirmed
that the server process loaded the voice credential. No gateway or CLIProxyAPI
configuration changed. No build or test suite ran. An authenticated request through
Zoe's public voice endpoint, macOS retry, and real microphone/WebRTC acceptance
remain unverified.

During this investigation, the operator explicitly ran the CLI key-generation
operation. The resulting private export became the authority database's current
key; the deployment copy was then synchronized from that export and the previous
copy was retained as a mode-0400 backup. Container recreation does not generate or
rotate an instance key: only the explicit CLI `key init`/`key rotate` operations do
so, and every client must reimport the resulting private export after rotation. The
matching key authenticated a local `/v1/voice/config` request and returned the
complete configuration. The key value is not recorded here.

## Zoe rename — 2026-10-28

On the local main worktree, exact case-preserving text replacement updated 265
files and renamed 68 files, including their parent directories, across all 299
tracked files. A byte-for-byte comparison against the expected transformation
passed for every tracked file, and a case-insensitive source scan found no old
project-name occurrences. Git whitespace validation passed.

This is source transformation evidence only. Hosted builds, tests, installation,
and end-to-end acceptance were not run for this rename. The existing Git remote
and external deployments were not renamed; rewritten repository URLs and new
application, credential, storage, and service identifiers require corresponding
external migration before deployment. The raster design reference was retained.

## Release candidate validation — 2026-10-29

The read-only `deploy/linux/deployment-candidate.sh` check validates a release
manifest's source revision, target architecture, artifact SHA-256 values and image
digests, and can render a supplied Compose file. Its regression script covers a
valid candidate and tampered-artifact rejection. The check emits installation and
rollback targets without connecting to or modifying a host. No release transfer,
service startup, health endpoint, external AO/model task, or end-to-end product
acceptance is claimed by this source-level validation.

## GHCR deployment image publication — 2026-10-29

On source revision `d0f806b`, source review changed the server and legacy AO release
workflows to push Linux images directly to GHCR and retain only image references
and immutable digests beside the transferred daemon artifacts. The deployment
examples and candidate validator now accept GHCR references and validate their
`sha256` digests; the candidate regression passed, shell syntax passed, and both
changed workflow files parsed with the local Ruby YAML parser. No hosted workflow
run, GHCR push/pull, host installation, service startup, or deployment rollback
was performed by this source-level check.

## Voice route and gateway probes — 2026-10-31

Environment: the voice gateway host, one-off probes against a copy of the account database, one pool account, one call per run; probe source on branch `codex/probe-session-update` of the gateway repository (built on GitHub-hosted runners, removed from the host afterwards). Results are structural; no tokens, account data, or audio were recorded.

- Gateway relay with idempotent session release (gateway `6b77a36`) deployed with `deploy-host.sh update`; the close-then-rewake loop ended its sessions and left the Zoe voice pool unblocked.
- The `/v1/voice` data route in use carries only typed-text and stop-speaking events from the client; no verified field exists for instructions, history, or custom tools.
- Codex-style route `POST /backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas` with a V3 session (`delegation: client`, `initial_items`): HTTP 201, SDP answer, `Location` with an `rtc_` call id, `oai-events` channel open. Sideband `wss://api.openai.com/v1/live/<call_id>`: HTTP 400 without `openai-alpha: quicksilver=v2`, HTTP 101 with it. Sideband carried session, audio-delta, usage, context-appended, transcript, and turn events.
- Not established: `delegation.created` for ordinary spoken requests (two synthetic utterances produced none), non-empty transcript text, recall of `initial_items`, media through an IPv6-egress account proxy, per-plan entitlement of pool accounts.
- Limitations: synthetic speech only, one account, no failure injection. No product milestone is accepted by these probes.

## Live voice call, AVAS route — 2026-11-05

Environment: production gateway image from gateway source `82aed1b` (`VOICE_UPSTREAM_ROUTE=avas`) and Zoe `16a8675`, one real call from the Mac client, one pool account. Observed in gateway logs and the Zoe database; no audio was recorded.

- Verified once: spoken request, gateway `delegation.created` → `user_audio_turn.final`, Zoe `zoe-steward` task, Steward answer (`awaiting_user`), `voice_result_deliveries` `delivered`, gateway `POST /v1/voice/sessions/{id}/context` 200, upstream spoke a summary of the answer (about 2 minutes from task creation to the Steward answer, about 19 s more until speech).
- Verified once: hangup ended the Zoe session (`ended`), the gateway recorded `terminated` media evidence and released the account lease, and the voice pool stayed unblocked.
- Verified once: after the gateway fix `79ca0ed`, sideband events reach the AVAS delegation mapper. The earlier silent call was caused by the observer never seeing sideband events.
- The result reached the call through `/context`, not sideband `delegation.context.append`. Whether upstream speaks `speakable` delegation content remains unverified.
- The spoken summary understated rain ("very low probability") against the Steward answer (peak probability 100%, heavy morning rain); the upstream rewrites results and Zoe does not control the wording.
- `POST /v1/voice/sessions/{id}/opening-context` returned 501 at call start; cause not investigated, so opening context was not delivered.
- Not exercised: the gateway 422 `upstream_rejected` path, account fallback, `GET /v1/voice/requests/{id}/outcome` against Zoe attestation, and the recovery sweep. The pool lock was cleared with one operator `voice.pool.reconcile` for session `1b086abb` (cause of the earlier upstream rejection unproven; the gateway logs of that call were purged).
- Limits: one call, one account, one request. No product milestone is accepted by this record.

## Speakable probe, AVAS route — 2026-11-06

Environment: temporary gateway image from branch `probe/speakable-v1` (head `ad18459`, not merged), one real call, one pool account. With the probe switch set, the gateway answered each client-target `delegation.created` with one fixed `speakable` `delegation.context.append` and did not hand the turn to Zoe.

- Verified once: upstream acknowledged the append (`delegation.context.appended`) 139 ms after `delegation.created`.
- Verified once: upstream spoke the fixed sentence verbatim, preceded by its own lead-in sentence; asked again, it quoted the same sentence verbatim.
- Not measured: time to first audio (frame timing only), long content, suppressing the lead-in, several delegations per call, barge-in. Playback of the roughly 30-character sentence took about 6 s of transcript frames; that is speaking time, not delay.
- The probe image replaced the previous gateway image and the Zoe handoff was inactive while it ran. Design consequences are in [ADR 0012](../architecture/decisions/0012-voice-conversation-layer.md).

## Instruction and routing probes, AVAS route — 2026-11-06

Environment: temporary gateway image from branch `probe/instruction-v1` (head `b7f7d83`, not merged), three real calls, one pool account. The gateway sent one session-level `session.context.append` (`speakable`, no delegation) when the sideband came up.

- Verified: upstream follows a fixed-answer instruction. With "answer a progress question with exactly this sentence", it answered "is it done yet" with that sentence twice, without delegating; it answered a greeting itself and delegated a research request.
- Verified: the same upstream invented progress statements when the instruction did not define the answer ("almost there"). Wording not specified by Zoe is not reliable.
- Verified: `delegation.created` carries the raw transcript. A tag-prefix instruction ("start the delegation with [progress]") produced no tags in five delegations. Upstream cannot route; Zoe must classify ([ADR 0012](../architecture/decisions/0012-voice-conversation-layer.md)).
- Observed once, unexplained: on one call an empty-text delegation and an upstream "bye" appeared before the user spoke, and the client data channel closed about 40 s later (`data_channel_failed`, a-to-b). Not reproduced on the next call; cause unknown, client diagnostics not read.
- Not exercised: pushing text while the user is speaking, several pushes in one call, opening context effect (it is sent as `session.initial_items` at creation; the separate `/opening-context` returns 501 on AVAS by design, and Zoe records `unsupported` without retrying).
- Limits: three calls, one account, one language. No product milestone is accepted by this record.

## Memory model and Qdrant state — 2026-11-06

Read-only checks on the Zoe host:

- Verified: the memory model endpoint configured for the memory worker rejects its key (HTTP 401, "Invalid API key" on `/models` and `/responses`). `memory_model_calls` is 0 and both Qdrant collections hold 0 points, so extraction has never completed against this endpoint. Cause of the key rejection unknown.
- Verified: `Repository.Search` and `AssembleContext` use SQLite keyword scoring; they do not query Qdrant.
- Verified: Zoe persisted 10 voice turns, 28 tasks, and 37 task entries from 2026-11-01; upstream-answered turns are not persisted anywhere.
- Not established: Qdrant semantic recall quality or latency, and any date-based recall.

Open measurements tied to [ADR 0009](../architecture/decisions/0009-voice-steward-impact-policy.md): steward latency and cost against the run ceiling, metering of OpenCode internal sub-agents, handoff event behavior with real speech, and transcript availability.

## Per-run permission probe on the live host — 2026-11-02

Environment: Zoe host with release `53daae1`, `zoe-opencode` v2.2.5-ronigooja.3 running as root, one synthetic task through `task.create`, model `cliproxyapi/gpt-6.1-sol`. No product milestone is accepted by this probe.

- Session created with agent `zoe-worker` and the `workspace_read_write` ruleset (deny-all first, no `external_directory` allow); `ZOE_MAX_RUNS` read back as 12.
- Allowed and observed: file creation inside the run workspace through `patch`, `echo` through `shell`.
- A `shell` redirect wrote a file outside the workspace. The ruleset did not prevent it; `external_directory` applies to file tools only. The probe file was removed afterwards.
- `websearch` ended with "Web search cancelled"; the cause is not established.
- Not exercised: a live `ask`, Worker question, expiry, or reply against a real OpenCode session.

Consequence recorded in [ADR 0011](../architecture/decisions/0011-profiles-permissions-and-context-hygiene.md): OpenCode permissions are the only enforcement layer by operator decision.

## Bounded recall component verification — 2026-11-06

Source: recall-route candidate based on `d444f5b`, local macOS, fake clock/gateway/model. The authorized serial command `nice -n 19 go test -count=1 -p 1 -parallel 2 -timeout 120s ./internal/voice/ ./internal/answer/ -run 'TestRecall|TestAnswer|TestHandoffProgress|TestHandoffTaskMemory|TestHandoffHighImpact|TestClassifierAndGateway'` passed. Route cases cover ordered holding/final delivery, no task, owner/workspace/date exclusion, requested transcript ranges, 16 KiB excerpts, one-extra-round limit, honest failures, handoff deduplication, speakable text and private-log exclusion. Disabling the recall branch made all eight original route cases fail at `recall_test.go:110`: `recall created task: tasks=1 resource=task`. Targeted vet, formatting and documentation checks passed. These are fixture/source checks only; no hosted run, real provider, live gateway speech or restart-recovery acceptance is established. Limits are recorded in the [recall contract](../architecture/decisions/0012-voice-conversation-layer.md#bounded-recall-route).
