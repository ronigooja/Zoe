# Zoe delivery plan

**Status:** implementation is underway; no product milestone is accepted. See the [evidence register](acceptance/evidence.md#evidence-categories-and-acceptance-boundary) for evidence categories and the acceptance boundary. Detailed requirements live in [product](product.md), technical ownership in [architecture](architecture.md), and recorded choices in [ADRs](architecture/decisions/).

## Milestones and acceptance

Milestones sequence delivery without reducing product scope. The [acceptance matrix](acceptance/requirements.md) maps every stable requirement to milestones, evidence, and gaps; [the evidence register](acceptance/evidence.md) records source versions, environments, results, and limits.

| Milestone | Scope | Exit gate | Status |
|---|---|---|---|
| 0. Documentation baseline | Current requirements, architecture, decisions, procedures, and traceability | Complete unique requirements and working source links; honest evidence boundaries | Current requirements and provenance migrated; independent review complete; hosted documentation checks gate changes |
| 1. Contracts and trusted task foundation | Control plane, runner, command/task/event authority, shared instance-key authentication, native shells | Atomic durable receipt/work/outbox under faults; dedupe and stale-version rejection; key rotation/denial; authenticated runner flow; three native clients; single-instance fencing | Source and hosted checks exist; product acceptance open |
| 2. Runtime, voice, and memory integration | Zoe-owned OpenCode runtime, turn/receipt bridge, controlled memory and derived index | OpenCode runtime dispatch/report authorization; configurable `ZOE_MAX_RUNS` cap; automatic continuous voice with truthful OS/audio/lifecycle/account/server boundaries; safe voice lease release; authority-first memory and tombstone/rebuild behavior | Authenticated server-owned `voice.turn.handoff` boundary and the strict `user_audio_turn.final` downstream consumer are implemented; real gateway AVAS event production, warm Steward response, and real voice-to-runtime acceptance remain open |
| 3. Complete task loop with two runtimes | Request, coordination, evidence, decisions, stop, recovery, server-authored result/message | The Zoe-owned OpenCode runtime boundary; scenarios A and D; waiting-task `task.append`; authoritative `assistant_message`; scoped explicit confirmation for high-impact actions; ordinary results without Accept/Reject workflow; provider/tool/artifact evidence and confirmed termination; no duplicated irreversible effects | Open; OpenCode completion and recovery evidence remain required |
| 4. Long-running services | Authorized lifecycle, deterministic triggers, escalation, notices | Actual process outcomes; service survives client/Super exit; scenarios B and E; bounded retry/dedupe | Source and synthetic checks exist; real external actions open |
| 5. Reliability and Apple delivery | Data lifecycle, security, performance, native delivery | Measured restore/RPO/RTO, migration fencing, security negatives, real-device voice/APNs, A–E and all required IDs/quality dimensions | Open |

## Current status and blockers

The bounded recall route has local fake-gateway/model regression coverage; see [its contract](architecture/decisions/0012-voice-conversation-layer.md#bounded-recall-route). Hosted integration, real speech/provider behavior and in-flight restart recovery remain open.

The [2026-10-29 voice deployment repair](acceptance/evidence.md#voice-deployment-configuration-repair--2026-10-29)
restored the missing gateway credential mount and startup configuration on the
current Zoe host. Server startup and gateway configuration checks passed;
authenticated client and real media acceptance remain open.

The project rename to Zoe is applied in source and paths; see the
[rename evidence](acceptance/evidence.md#zoe-rename--2026-10-28). Hosted validation
and external repository/deployment migration remain pending.

| Area | Available evidence | Required next evidence |
|---|---|---|
| Control and Zoe OpenCode runtime | Hosted Go/race and historical runtime checks; deployed health/readiness observations | Real authorized OpenCode/provider/tool/artifact chain and stop; deployed chat recovery is source-reviewed, remote-unverified; four uncertain historical runs were abandoned by operator decision |
| Budget enforcement | Reservation/settlement and unknown-usage ledger source checks | Actual OpenCode per-provider-call and cost reporting; production pre-call tool-step and bounded artifact enforcement; online deadline fencing |
| Memory and data lifecycle | Go/Python/component fixtures, export validation, deletion manifests and gated restore checks | Real Mem0/Qdrant and external-store cleanup; measured production backup erasure and restore; unsupported adapters remain closed |
| Voice | Turn identity/dedupe, authenticated `voice.turn.handoff`, low-impact Steward-profile admission, conservative server impact policy, and generation-fenced cleanup source checks | Warm Steward response/result delivery, server-owned scoped confirmation context for high-impact work, automatic startup/reconnect and truthful OS permission/audio policy/lifecycle/network/account/server states; text fallback; trusted gateway account identity and account-scoped termination evidence; safe reuse, interruption, and real-device voice-to-task acceptance |
| Apple clients | Hosted Swift tests/macOS archive/iOS build and limited Mac installation evidence | No composer microphone or generic result Accept/Reject UI; server-authored result/message rendering; current-key authenticated flows, screenshots/accessibility/core interactions, real iPhone/iPad/APNs/voice, distribution signing and delivery |
| Capabilities | Admission/revocation and synthetic lifecycle fixtures | Real OpenCode-trigger execution and external service lifecycle/recovery |

Evidence does not by itself accept a milestone; see the [evidence register](acceptance/evidence.md#evidence-categories-and-acceptance-boundary) and [acceptance matrix](acceptance/requirements.md).

CI scope optimization is implemented and its full hosted selection passed:
independent Apple/Go/race/script checks, routine builds without release packaging,
and a gate requiring every selected group. See [verification procedures](development/verification.md)
and [retained evidence](acceptance/evidence.md#scoped-ci-verification).
Manual release packaging and remote branch-protection configuration remain outside
this verification; product milestone acceptance is unchanged.

## Delivery dependencies

Runtime work depends on the Zoe-owned OpenCode runtime and supported adapters; model routes depend on the existing external CLIProxyAPI; voice depends on Space3044 gateway lifecycle evidence and trusted media transport. Apple delivery depends on signing, platform permissions, Keychain, APNs, and real devices. Integration must recheck each dependency's actual compatibility without changing the external CLIProxyAPI deployment.

SQLite WAL/FULL is a single-instance persistence choice, not multi-host database availability. Production reliability, isolation, numeric service-quality thresholds, and measured recovery targets remain unaccepted. Quality requirements and their acceptance boundaries remain in [product chapter 20](product.md#20-overall-quality-requirements) and [acceptance](acceptance/requirements.md).
