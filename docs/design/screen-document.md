# A2UI screen contract

Status: design accepted 2026-11-14; implementation pending. The Go contract and Swift renderer have not yet changed. Rationale: [ADR 0008](../architecture/decisions/0008-ambient-screen-client.md); delivery: [master plan](../master-plan.md).

## Protocol and renderer

Zoe uses [A2UI](https://github.com/a2ui-project/a2ui) v0.9.1 with the community [a2ui-swift renderer](https://github.com/BBC6BAE9/a2ui-swift), pinned to revision `16476ba2cb3fcb4c4bcb141dfa4c2804adbdedb0`. That renderer decodes only `v0.9` and `v0.9.1`. The specification repository marks v1.0 as current, but the renderer does not support it; upgrading depends on renderer support.

Only the A2UI basic catalog is used: `Text`, `Image`, `Icon`, `Video`, `AudioPlayer`, `Row`, `Column`, `List`, `Card`, `Tabs`, `Modal`, `Divider`, `Button`, `TextField`, `CheckBox`, `Slider`, `ChoicePicker`, `DateTimeInput`. Zoe defines no custom components; add them only when needed.

The Zoe-specific document format is removed without backward compatibility. Node fields `emphasis`, `tone`, `ttlSeconds`, `transition`, and `bind`, binding to client data sources, and patching by old node ID are removed.

## Surfaces

A surface is a display area the AI creates, updates, and closes. Main window, floating panel, menu-bar item, menu-bar popover, Dock badge, and additional windows are all surfaces; the client has no fixed regions. With no surface open, only the menu-bar item exists.

A2UI has no window, panel, menu-bar item, popover, or Dock concept. Zoe retains an outer `SurfaceSpec` with `{id, kind, window}` and an inner document containing a list of A2UI messages. `kind` is `window | panel | statusItem | popover | dockBadge`. `window` is optional and holds flat fields: `visible`, `width`, `height`, `x`, `y`, `display`, `floating`, `opacity`, `clickThrough`, `takesFocus`, `fullscreen`, `showInDock`, `showInMenuBar`, `material`. Sizes and positions are clamped to the visible area of the chosen display. A user's direct window action (close, drag, resize) wins over an AI window change that arrives immediately after it.

Surface tools: `surface.open(spec)`, `surface.update(id, patch)`, `surface.close(id)`, `surface.list()`. `surface.update` replaces or updates A2UI components/data of a surface instead of patching old node IDs.

## A2UI messages and actions

- `createSurface{surfaceId, catalogId, theme?, sendDataModel}`
- `updateComponents{surfaceId, components[]}`
- `updateDataModel{surfaceId, path?, value?}`
- `deleteSurface{surfaceId}`

Components form a flat list with an `id` and component name; the root component has ID `root`.

User actions return as `A2uiClientAction{name, surfaceId, sourceComponentId, timestamp, context}`. Zoe maps `name` to the opaque intent and `context` to `args`. The server still validates authority, scope, expected version, and expiry. Clients and documents never decide business outcomes.

Voice requests are accepted by the server under the impact levels in [ADR 0009](../architecture/decisions/0009-voice-steward-impact-policy.md); voice never confirms high-impact work.

## Local composition and delivery

The server no longer produces UI documents: delete `/v1/screen` and the `internal/screen` builder. No client ever called that endpoint. The client builds A2UI messages locally from authoritative tasks, approvals, and notifications, without ranking or invented content. Delivery remains `/v1/events` (SSE), `/v1/snapshot`, and `/v1/notifications`.

Default screen, replacing the current client default (not yet implemented): one `List` mixing tasks, approvals, and notifications, newest first (ties by ID). Each entry is a `Card` holding a `Text` title (task title, approval prompt, or notification title) and a `Text` status (task status label, approval status, or notification kind). With no entries the list is empty.

The removed server builder also produced a status line, notices, completed-result windows, and pending-decision panels. The client never displayed them, and this contract does not carry them over.

Not-decided future option: AI-driven server-pushed UI could use a new event type carrying A2UI messages. This is not the current delivery decision.

## Settings

Setting metadata is `{key, type, range, description, aiMutable, userOnlyReason}`. The AI may change every setting except those with `userOnlyReason`: instance key and login credentials, OS microphone permission, and the setting that revokes AI settings permission. Window and presence settings are ordinary AI-mutable settings. Every AI change shows a subtle notice and creates a reviewable, undoable audit record.

## Client tools

`surface.open`, `surface.update`, `surface.close`, `surface.list`, `settings.list`, `settings.read(key)`, `settings.write(key, value)`, `settings.undo(recordId)`, `voice.start()`, `voice.end()`, `voice.setSilenceTimeout(seconds)`, `textInput.show()`, `textInput.hide()`.

Presence floors that hold regardless of tools: the menu-bar item always exists, so a hidden app can be found again; and while the microphone is in use the menu-bar item shows that state.

## Open items

- Check in macOS CI whether a2ui-swift's `swift-json-schema` dependency resolves alongside ZoeKit's WebRTC dependency.
- Determine how the surface `catalogId` is registered so `createSurface` passes.
- Confirm licensing before shipping: the renderer's `LICENSE` file says MIT, while source headers say Apache 2.0, Google LLC.
