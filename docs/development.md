# Development and verification

Use this page for task entry, documentation ownership, verification selection,
and reporting. [Data flows](design/data-flows.md) is the design overview; its
linked components define behavior, not proof of implementation or acceptance.
[Operations](operations.md) owns reusable administration procedures.

## Selective task entry

Use the overview's [task navigation](design/data-flows.md#read-by-task) to choose
contracts. Module setup lives in the [Apple](../clients/apple/README.md) and
[memory worker](../services/memory/README.md) guides; deployment tasks start in
[operations](operations.md).

Read the repository and applicable directory instructions before editing. Inspect
source and tests for current behavior; a source link in a contract is an entry
point, not a conformance claim. Bound the change and its verification before
editing; update the owning contract when the approved behavior changes.

## Documentation ownership and lifecycle

- Define each behavior, schema, default, and authority boundary once in its
  owning flow component; link to it instead of maintaining competing specs.
- Keep this page responsible for development policy and cross-component
  verification obligations, and operations pages responsible for executable
  procedures, prerequisites, failure handling, and limitations.
- Module READMEs explain local entry points and implementation constraints, not
  a second product or architecture baseline.
- Update or remove obsolete statements and links with the change. Do not create
  dated task journals, parallel plans, evidence archives, or historical copies of
  current documentation. Git history preserves superseded documentation.
- Report evidence with the change or immutable workflow/source record. Preserve
  revision, environment, action, outcome, and limitations without copying private
  logs, credentials, account data, message text, or raw audio into the repository.

## Product verification obligations

Within the [approved scope](design/data-flows.md#authority-and-scope), Zoe does
not recreate an OS, IDE or terminal desktop. Preserve usable external work
products and replaceable models/services without claiming unimplemented
connectors or team features.

The component contracts own detailed flows. Verify these cross-component
journeys as obligations, not completed milestones:

| Journey | Required outcome |
| --- | --- |
| A — Software development | Voice/text intent enters real OpenCode execution with linked project/task/run/workspace and artifacts; work survives client closure; another device reads actual progress; additions/continuation keep context; stopping has verified termination. Native-tool session handoff is optional, not a substitute for runtime evidence. |
| B — Durable automation | Distinguish a finite lookup from an authorized installed mail-monitoring service; verify configuration, permissions, health, pause/resume/update/removal and per-trigger records. Deterministic checks run independently, semantic cases use Workers, complex cases escalate, and source-linked notices avoid repetition. A generated program is not deployment authorization. |
| C — Project memory | Recover dated decisions, changed approaches and recorded reasons across devices; distinguish confirmed facts, execution evidence and inference. Report missing/conflicting sources as uncertainty rather than inventing rationale; verify project access boundaries. This goal does not imply memory export or deletion is implemented. |
| D — Parallel work | Specialists can work independently in parallel; preserve dependencies, shared-resource ownership, failure/cancel/recovery records and truthful consolidated results. Coordination resolves conflicts without uncoordinated writes. |
| E — Failure and recovery | Detect unattended network/API/process failure, retain events while clients are absent, recover only when safe, and escalate unresolved effects rather than blindly repeat them. Verify source fencing and linked state after migration. |

Knowledge and context goals remain broader than demonstrated worker behavior:
retain source/decision timing and validity, distinguish global/project/task/raw
history, retrieve only authorized relevant context, and surface missing evidence
without invention. The current worker-owned memory does not provide logical
memory export, user-facing deletion, or old claim-schema migration/restore.
Generic backup/deletion journals do not prove Mem0/Qdrant recovery or physical
backup erasure; inspect [data lifecycle limitations](operations/data-lifecycle.md)
before planning migration or destruction.

All fifteen quality dimensions require explicit evidence:

| Dimension | Verification obligation |
| --- | --- |
| Accuracy | Results match execution evidence; inference is not confirmed fact. |
| Continuity | Projects, tasks, and services outlive conversations and client closure. |
| Persistence | Important records and memory survive process restarts and supported recovery. |
| Reliability | Fault detection, safe retry, uncertain outcomes, and recovery are exercised. |
| Extensibility | Adding a model, Worker, service, or capability respects existing contracts. |
| Independence | Replaceable model/service boundaries do not silently change authority. |
| Interoperability | Preserve usable native work products and supported continuation; human session handoff remains optional. |
| Cross-device consistency | Devices reconcile one versioned authority and expose stale/offline state. |
| Cross-platform support | Exercise native macOS/iPhone/iPad behavior against the Linux backend. |
| Controllability | Users can understand scope, authorize, pause, stop, and recover work. |
| Security | Check permissions, isolation, private data, secret handling, and external-service protection. |
| Observability | Trace actual runtime state, exceptions, results, usage, and their sources. |
| Cost efficiency | Measure unnecessary model calls, repeated context, limits, and provider-backed usage/cost. |
| Ease of use | Natural interaction, concise actionable errors, English/Simplified Chinese, system appearance/region, and native accessibility require interaction evidence. |
| Long-term evolution | New capabilities integrate without rebuilding the core or breaking retained data. |

Include VoiceOver, keyboard/focus, Dynamic Type, reduced motion, both languages
and appearances, and truthful permission/background/reconnect states in native
verification. Real-device voice includes a 30-minute mixed Mandarin–English call
on each real iPhone and iPad while locked, interruption, audio-route changes,
reconnect and durable-task continuity. APNs delivery and durable-inbox refresh
need separate checks. These are targets, not reported results. Unspecified
quality thresholds remain open; daily backup, RPO 24 hours and RTO two hours are
unverified targets, not guarantees.

## Evidence and acceptance

| Evidence | Establishes only |
| --- | --- |
| Design/documentation | Approved intent or a documented procedure. |
| Source review | Behavior found in a named source revision; not that it ran. |
| Schema/unit/fixture | The exercised synthetic contract or component behavior, including its omissions. |
| CI/tests | Named checks in the recorded environment/revision; not deployment or real-provider success. |
| Build/artifact integrity | Compilation/package creation and verified hashes; not installation. |
| Installation/startup | Installed revision, signing/process checks and startup; not interaction or task success. |
| Health/readiness | The probed service's bounded readiness condition; not authentication, execution, or memory success. |
| Transport/receiver receipt | The documented local delivery/commit boundary, not upstream consumption or speech. |
| Render acknowledgement | Matching surface/document revision actually rendered; not task completion, spoken output or full usability. |
| Real-device/end-to-end | Only the observed user journey against the named real services/devices, with remaining gates explicit. |

Do not infer milestone or product acceptance from a lower category. Record the
source SHA, environment/device/service revisions, exact action or command,
selected scope, outcome and limitations. Link immutable run/source evidence;
state not-run, skipped, blocked and unverified coverage separately. For actual
completion require runtime terminal evidence, external-effect reconciliation,
and the applicable user-visible result, not a model saying “done.” Provider
streaming finish/usage, in-flight crash recovery, exact stop, and tool/time/budget
enforcement need real-runtime evidence; mocks cannot close those gates.

## CI selection and execution policy

[ci_scope.py](../tools/ci_scope.py) is the executable scope selector, protected by
[scope regressions](../tools/test_ci_scope.py). [ci.yml](../.github/workflows/ci.yml)
handles pull requests and pushes to `main` and `codex/**`, using the complete
changed-path set, deleted files and both rename sides. Missing history and
unknown paths select all existing groups; this does not create coverage for a
new component. Mixed changes combine selections; source comments follow their
source component's scope.

| Change | Existing selected checks |
| --- | --- |
| Markdown, documentation assets/checker | Documentation checks |
| macOS app directory | macOS build |
| iOS app directory | iOS build |
| Other Apple/shared package/project/resources | Swift tests and both app builds |
| Go source | Full Go suite and existing authorization/concurrency race package set |
| Control/voice/memory/capabilities API, shared protocol, integration fixtures, Go dependencies | Go/race plus Swift tests and both app builds |
| Linux deployment shell scripts | Release-script and CLI-installation regressions |
| CI configuration, selector, unknown paths, unavailable history | All existing groups |

Apple builds, Swift tests, Go tests, race tests and scripts are independent jobs.
Routine checks do not package releases; Python services and external runtime
coverage still require task-specific checks. **Required verification** checks
scope selection and every selected group's success, failing selected jobs that
are skipped, failed or cancelled. Requiring that gate in branch protection is a
separate repository-administration action.

Run Linux builds/tests over SSH on an approved host:
`root@38.47.101.8`, `root@64.118.133.169`, or `root@45.45.217.235`.
Run Apple builds/tests on GitHub-hosted macOS. Ask before using other execution
environments, except small permitted local tests. Check `$HOME/Agents/environment`
before installing dependencies. Redirect lengthy command output to private
temporary logs; inspect failure excerpts only and report concise results.

**Existing automation conflicts with this Linux policy:** Linux jobs in
`ci.yml`, `linux.yml`, `server-release.yml` and `opencode-image.yml` currently use
GitHub-hosted Ubuntu; Apple packaging also has a hosted Linux step. This page
does not change workflows or authorize triggering those builds/tests. Resolve
authorization and host placement before dispatching a workflow containing them.

### Commands and release scope

Run these from the repository root in the authorized environment, selecting
checks by impact/risk. Full Go coverage and the race set remain unchanged:

```sh
python3 tools/check_docs.py
python3 -m unittest discover -s tools -p test_check_docs.py
python3 -m unittest discover -s tools -p test_ci_scope.py
go test ./...
go test -race ./internal/control ./internal/runner ./internal/agents/opencode ./cmd/zoe-server ./internal/ops ./cmd/zoe
swift test --package-path clients/apple/Packages/ZoeKit
```

The workflows own exact SDK, flags, fixtures and packaging commands:
[documentation](../.github/workflows/docs.yml),
[Linux](../.github/workflows/linux.yml), and
[Apple](../.github/workflows/apple.yml). Use
[Apple validation help](../clients/apple/tools/validate-apple-build.sh) for isolated build
options; package tests are not app-target builds or screenshots.

The following are existing workflow interfaces, **not authorization to dispatch**:

```sh
gh workflow run ci.yml --ref main
gh workflow run apple.yml --ref main -f release=true
gh workflow run linux.yml --ref main -f release=true
```

Manual scoped verification selects all existing groups when impact is unclear.
Manual platform workflows without `release=true` perform full platform checks
without release packaging. Apple release produces `Zoe.dmg` and `SHA256SUMS` in
`apple-macos-<full-sha>` with three-day retention, after Swift tests and macOS
build pass. It uses the repository self-signed identity (secret references
`ZOE_SIGNING_P12`, `ZOE_SIGNING_P12_PASSWORD`) for privacy-grant continuity; it is
not notarized and Gatekeeper approval remains separate. Formal release tags on
`main` drive the release manifest linking reviewed source, architectures, hashes
and image digests. Failed/partial jobs are not a complete release. Server and
runtime artifacts, candidate validation, installation, and deployment remain
separate; follow [deployment operations](operations/deployment.md).
