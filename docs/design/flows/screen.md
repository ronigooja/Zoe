# Screen presentation and event recovery contract

## Purpose and boundary

Approved target, 2026-11-16, retaining complementary native-renderer/platform obligations; not implementation, deployment, or acceptance evidence. The [approved overview](../data-flows.md) establishes precedence. UI_Controller uses full requests and authoritative task facts to generate server-driven A2UI presentations. The client renders and reports outcomes; neither client nor document creates business results or authority. Older local-only composition, server-document prohibition, and undecided-server-push descriptions do not apply.

[Results](results.md) owns authoritative text and voice delivery; [execution](execution.md) owns research/work. Screen-only content needing research must enter the task path: UI_Controller does no research or invented-result generation. A stale surface is not the current task result.

## Inputs and outputs

UI_Controller receives full request(s), task state/result/question, current surfaces, and current screen facts, without duplicate raw-utterance input. Output carries A2UI content, outer container choice (popover/window), optional size, optional awaiting-input linkage to `task_id`/`question_id`, and feedback. `ui.screen` supplies current displays/visible bounds; surface state supplies identities/revisions.

Render ACK includes surface/document identity, revision, and actual outcome. Bound A2UI input includes task/question, surface revision, submitted full value, and command idempotency identity. It routes directly to the bound task/question: no voice relay, register pass, or semantic task reselection. Screen input can complete while voice is unavailable.

### Protocol and renderer

Use A2UI v0.9.1 and community [a2ui-swift](https://github.com/BBC6BAE9/a2ui-swift), pinned to `16476ba2cb3fcb4c4bcb141dfa4c2804adbdedb0`. Renderer decodes only `v0.9` and `v0.9.1`; upgrading to specification v1.0 depends on renderer support. Only basic catalog components are allowed: `Text`, `Image`, `Icon`, `Video`, `AudioPlayer`, `Row`, `Column`, `List`, `Card`, `Tabs`, `Modal`, `Divider`, `Button`, `TextField`, `CheckBox`, `Slider`, `ChoicePicker`, `DateTimeInput`. Define no custom components unless needed and approved.

Each UI-controller message has exactly one operation:

- `createSurface{surfaceId, catalogId, theme?, sendDataModel}`
- `updateComponents{surfaceId, components[]}`
- `updateDataModel{surfaceId, path?, value?}`
- `deleteSurface{surfaceId}`

`version` is server-owned. Model output may omit it or supply any string; non-string is invalid. Before storage/event delivery set every message to `version: "v0.9.1"`; other unknown message keys remain invalid. Components form a flat list with `id` and component name; root ID is `root`.

The old Zoe document format has no backward compatibility: no node `emphasis`, `tone`, `ttlSeconds`, `transition`, or `bind`, client-data-source binding, or old-node-ID patching. `surface.update` updates/replaces A2UI components/data instead.

User actions are `A2uiClientAction{name, surfaceId, sourceComponentId, timestamp, context}`: map `name` to opaque intent and `context` to `args`. Server validates authority, scope, expected version, expiry, ownership, binding/revision, current pending state, and idempotency; clients/documents never decide outcomes.

### Native shell and surfaces

A surface is an AI-created/updated/closed display area: main/additional windows, floating panel, menu-bar item/popover, and Dock badge. The ambient macOS shell has no fixed sidebar, tabs, persistent composer, or fixed client regions; content may still use basic-catalog components. With no surface open, the menu-bar item remains. A2UI itself has no window/panel/menu-bar/popover/Dock concept. Outer `SurfaceSpec` is `{id, kind, window}`; `kind` is `window | panel | statusItem | popover | dockBadge`. Optional `window` has flat fields `visible`, `width`, `height`, `x`, `y`, `display`, `floating`, `opacity`, `clickThrough`, `takesFocus`, `fullscreen`, `showInDock`, `showInMenuBar`, `material`.

Use consistent native style; clamp size/position to chosen display's visible bounds. Direct user close/drag/resize wins over an immediately following AI window change. Popovers are transient below the menu-bar icon; windows are titled/resizable/opaque. Colors/translucency are host-owned, not model-controlled. System semantic colors adapt to both appearances.

macOS `SurfaceManager.swift` owns style constants: surface padding 8 pt, card padding 8 pt, leaf margin 4 pt. Host adds a vertical scroll view and explicit surface padding around the non-scrolling library surface; overflow scrolls vertically. A card containing a leaf adds `8 + 4 + 8 + 4 = 24` pt per edge. Card primary-label fill opacity is 6%, corner radius 12 pt, no shadow. Clear popover hosting layers reveal system material; windows keep opaque background. Text system fonts: h1 22 pt bold; h2 18, h3 15, h4 14, h5 13 pt semibold; body 13 pt; caption 11 pt secondary. Unspecified variant uses body override. Leaf/outlined containers receive leaf margin; structural Row/Column/List do not.

`SurfaceManager.fixedStyle` and adjacent spacing constants are rendering-fact authority. Keep `RenderingEnvironment` in `internal/uictl/controller.go`, Swift style tests, and these facts synchronized. Controller receives environment definitions, not layout rules. Keep `OptionalProperties` and validator-parity tests synchronized with `internal/uictl/components.go` when optional properties/enums change.

## Required behavior

### Presentation, input, dismissal

Persist pending presentations until matching current render ACK; generation/dispatch is not display success. Success feedback is allowed **only after render ACK**. Rejected/failed/stale ACK never announces success. Preserve submitted full input; stale/duplicate input cannot answer another question or create another execution.

On macOS, outside left/right click or Esc dismisses popovers immediately, including pending-question popovers: local dismissal, not answer/server deletion. Other focus loss (app switch, Spaces, other key window) starts popover delay; window resign-key/app-deactivation starts separate window delay. Focus return cancels countdown. Persist separate General-settings delays in seconds: each default 60, clamp 0–3600, 0 disables timed closing. `awaiting_input` never closes on a timer.

Launch/reconnect recovery presents only `awaiting_input` surfaces with server-validated task/question bindings plus updates to surfaces still presented in this connection scope. Always apply tombstones; do not hide already-visible surfaces solely because they are not pending. Track suppressed snapshot revisions without reporting native render success. Live events keep normal presentation behavior. Local dismissal/filtering leaves server data intact.

### Tools, settings, presence, and native obligations

Client tools: `surface.open(spec)`, `surface.update(id, patch)`, `surface.close(id)`, `surface.list()`, `settings.list`, `settings.read(key)`, `settings.write(key, value)`, `settings.undo(recordId)`, `voice.start()`, `voice.end()`, `voice.setSilenceTimeout(seconds)`, `textInput.show()`, `textInput.hide()`.

Setting metadata is `{key, type, range, description, aiMutable, userOnlyReason}`. AI may change all settings except those with `userOnlyReason`: instance key/login credentials, OS microphone permission, and revocation of AI settings access. Window/presence settings are ordinarily AI-mutable. Every AI setting change shows a subtle notice and reviewable/undoable audit record. Menu-bar item always exists and shows microphone use even when hidden. Command-Comma opens Settings; a subtle control reveals transient text fallback, also AI-showable/hideable. [Configuration](configuration.md) owns connection/key management; [media](media.md) owns truthful voice states and silence activity.

Keep native, concise, useful-density typography/grouping and accessible controls, not technical infrastructure panels. Ordinary results have no client Accept/Reject workflow. No native source-code editor/browser, diff viewer, terminal, or test console, including optional/hidden variants; programming summaries cover goal, stage, outcome, validation, and decisions, with work products available through authorized artifacts/external tools. English/Simplified Chinese, system appearance, regional dates/counts, and original server-content language remain obligations. Native errors are concise/actionable; [configuration](configuration.md#client-diagnostics) owns redacted automatic diagnostics. iPhone/iPad interface design remains unapproved; preserve common authority/accessibility boundaries without inventing an interface.

### Event and full-state recovery

SSE uses stable event identity/cursor and surface/result revisions. Deduplicate; apply only current revisions; repair gaps/reconnect from authoritative snapshots. Resume with `Last-Event-ID`; replay cannot reopen obsolete results, duplicate actions, or reset delivery state. History deduplicates authoritative message IDs.

Without consumed global cursor, or on SSE `snapshot_required`, use authenticated `GET /v1/recovery` (`client:read`). Ignore client owner/workspace selectors. Endpoint is read-only, performs no upstream HTTP, and validates generation/reads all fields in one SQLite read transaction. Exact top-level keys are `cursor` (decimal string), `snapshot` (existing `contracts.Snapshot`), `ui` (existing owner-scoped UI snapshot), and `voice`:

```typescript
type RecoveryVoice = {
  current_session: { id: string; generation: number; state: string } | null;
  pool: { blocked: boolean; reason_code: string };
};
```

This describes the existing contracts, not a replacement API schema: `snapshot` includes authority, projects, tasks, approvals, runs, and execution capacity. Owner's active voice session is selected, otherwise newest; return no other owner's session/lease/account/device identity. `cursor`, `snapshot.event_cursor`, and `ui.cursor` are the same checkpoint. Responses are `Cache-Control: no-store`; no schema migration.

Changing connection generation cancels its ACK task. Journal replay checks caller validity before/after receipt lookup and before submission; coordinator keeps original API client/workspace. Cancellation cannot retract a sent/accepted request or establish atomic no-send; uncertain requests retain original receipt/idempotency identity.

Recovery retains authoritative control state, reconciles exact voice ID/generation/active state, UI revisions/tombstones, and separately pending command ACKs. ACK lookup/delivery failure never blocks authoritative state/cursor adoption. Stage control/pool/UI tracking until render and local voice shutdown finish, then synchronously commit tracking/checkpoint under current connection generation. Rendering failure retains old model/cursor, marks unavailable, and retries. Track successful physical renders separately to avoid repeating them; failed revisions remain retryable. Native display/audio effects are not physically atomic or rolled back: partial display may remain while unavailable, obsolete audio is reported stopped rather than revived. UI-only repair never advances global cursor.

## Persistence and idempotency

Durable UI request/job records retain source/result/question identity, current surface revision, and delivery state. Persist surface/document revision, originating request/result IDs, awaiting-input linkage, ACK, and feedback dedupe by presentation identity/revision. Bound input commits durable command receipt and task/question entry. Maintain recoverable cursor and presentation/result identities with authoritative desired state; local display tracking is not a competing business authority.

## Failure and recovery

Missing/stale screen facts cannot establish a current presentation. Failed rendering is not success. Reconnect repairs missing state without duplicate presentation/feedback. Reject invalid ownership/binding/revision/pending state/idempotency; no semantic reselection on bound-input failure. A command submission is accepted, failed, or unknown: retain drafts through unknown acceptance and reconcile the original journaled receipt; clear only on durable acceptance or the connection/identity reset defined by [configuration](configuration.md#instance-key-and-native-connection). Configuration also owns validated-connection behavior on transport/authentication failure.

## Implementation locations

[UI commands](../../../internal/voice/ui_commands.go), [UI jobs](../../../internal/voice/ui_jobs.go), [feedback](../../../internal/voice/ui_feedback.go), [UI controller](../../../internal/uictl/), [HTTP contracts](../../../contracts/http.go), [Apple client](../../../clients/apple/Packages/ZoeKit/Sources/).

## Verification obligations

Verify authoritative result equality, request preservation, no research/Worker on redisplay, ACK-gated feedback, direct bound input without voice, stale/duplicate input/surface rejection, native styling/clamp/user precedence, render failure/retry, pending-only recovery/dismissal, SSE replay/gap/checkpoint repair, and generation/unknown-ACK semantics. Separately cover menu-bar recovery/mic presence, multiple surfaces, component/data updates, Mac 840×620 and desktop sizes, empty/loading/update states, text fallback, settings notice/audit/undo, opaque actions, stale/offline/failure, rotated keys/clearing/identity changes, both languages/appearances, VoiceOver, keyboard focus, reduced motion, Dynamic Type, and real device interaction.

Renderer dependency coexistence (`swift-json-schema` with WebRTC), `catalogId` registration, and MIT license file versus Apache 2.0/Google source-header licensing require resolution before shipping; do not infer resolution from this target. Report verification using [evidence and acceptance](../../development.md#evidence-and-acceptance).
