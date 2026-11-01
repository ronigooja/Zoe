# Screen-document contract (draft)

This is a draft contract for AI agents. The Go contracts in [`/contracts`](../../contracts/) are the implemented authority and do not yet include this model. Before server implementation, mirror and version this contract there. Rationale: [ADR 0008](../architecture/decisions/0008-ambient-screen-client.md).

## Surfaces

A surface is a display area the AI creates, updates, and closes. Each has its own screen document. Main window, floating panel, menu-bar item, menu-bar popover, Dock badge, and additional windows are all surfaces; the client has no fixed regions. With no surface open, only the menu-bar item exists.

`SurfaceSpec` is `{id, kind, document, window}`. `kind` is `window | panel | statusItem | popover | dockBadge`. `window` is optional and holds any of: `visible`, `size` (points), `position`, `display`, `level` (normal or floating), `opacity`, `clickThrough`, `takesFocus`, `fullscreen`, `showInDock`, `showInMenuBar`, `material`. Sizes and positions are clamped to the visible area of the chosen display. A user's direct window action (close, drag, resize) wins over an AI window change that arrives immediately after it.

Surface tools: `surface.open(spec)`, `surface.update(id, patch)`, `surface.close(id)`, `surface.list()`.

## Document shape

`ScreenDocument` is `{version, revision, root, data}`. `version` identifies the component-library contract; `revision` is the server ordering token; `root` is a `ScreenNode`; `data` is optional named data sources.

`ScreenNode` is `{id, type, props, children, emphasis, tone, ttlSeconds, transition, action, bind}`. `children` is an ordered list. `emphasis` is `low | normal | high`; `tone` is `neutral | info | attention | success | critical`; `ttlSeconds` and `transition` are optional. `action`, when present, is `{intent, args}` submitted opaquely to the server. `bind` names a data source (a `data` entry, or a client-provided source such as `tasks`, `notifications`, `voice`) so the node refreshes when that data changes without a new document. IDs are stable within a document. Patching by node ID is allowed.

The AI chooses semantic `emphasis` and `tone`; the client chooses colors, typography, and motion. Theme-level settings (accent color, density, type style) are separate settings with metadata, not node props.

## Primitives

- Layout: `stack` (`axis`, `spacing`, `alignment`), `grid` (`columns`), `split` (`orientation`, `ratio`), `group` (`label`), `spacer` (`minLength`), `canvas` (children placed by `anchor` or `x`/`y`), `layer` (children stacked), `scroll` (`axis`), `frame` (`minWidth`, `maxWidth`, `minHeight`, `maxHeight`), `background` (`material`, `opacity`, `gradient`).
- Content: `title` (`text`, `level`), `text` (`text`), `richText` (`markdown`), `list` (`items`), `table` (`columns`, `rows`), `progress` (`value`, `label`), `statusDot` (`state`, `label`), `image` (`url`, `alt`), `icon` (`name`), `link` (`label`, `url`), `countdown` (`until`, `label`), `divider`.
- Interactive: `button` (`label`, `style`), `choice` (`options`, `selected`), `input` (`label`, `value`, `placeholder`, `inputType`), `toggle` (`label`, `value`). `choice`, `input`, and `toggle` include their current value in `action.args` when submitted.

## Standard components

Standard components are named by the shape of the information, never by a business concept. They are built from primitives and have no special privileges. Each declares `purpose`, a props schema, and a `fallback` primitive rendering used when the client does not know it. Components adapt detail to the surface size (for example `metric` is a number in a status item, a trend card in a popover, a chart panel in a large window).

| Component | Information shape | Props |
|---|---|---|
| `metric` | A value with trend | `value`, `unit`, `delta`, `baseline`, `label` |
| `timeline` | Time-ordered events | `events[{time, title, state, detail}]` |
| `steps` | Staged progress | `steps[{label, state}]`, `current`, `fraction` |
| `decision` | A choice needing an answer | `question`, `options[{label, intent, args, consequence}]` |
| `compare` | Several objects by dimension | `items`, `dimensions`, `values` |
| `collection` | Same-kind items | `items`, `groupBy`, `sortBy`, `layout` (`list|grid|cards`) |
| `article` | Long text | `title`, `body`, `sections`, `sources` |
| `chart` | Numeric series | `kind`, `series`, `axes` |
| `place` | Location | `coordinate`, `name`, `annotations` |
| `media` | Image, audio, video | `kind`, `url`, `caption` |
| `notice` | One-off alert | `text`, `tone`, `action` |
| `facts` | Key-value detail | `pairs`, `groups` |
| `status` | Live state | `state`, `description`, `updatedAt` |
| `form` | User-filled fields | `fields[{key, type, label, validation}]`, `submit` |
| `quote` | Cited or quoted text | `text`, `source` |
| `code` | Short literal text such as a command or value, not a source browser | `text`, `language` |
| `gauge` | A value within a range | `value`, `min`, `max`, `zones` |
| `calendar` | Dates and events | `range`, `events` |
| `map` | Several places or a route | `points`, `route` |
| `gallery` | Many images | `items`, `layout` |
| `ranking` | Ordered scored items | `items[{label, score}]` |
| `checklist` | Items with done state | `items[{label, done}]` |

`decision` only shows options and submits an intent; authorization stays on the server. An approval is just a `decision`. When unsure, use `collection` or `facts`.

`components.describe()` returns the catalog the running client supports (type, `purpose`, props schema, example, library version) so the AI selects by information shape and knows what older clients lack.

Unknown types degrade to their `fallback`, or to readable text from `props` and children. A client must not infer a business concept from any type.

## Settings

Setting metadata is `{key, type, range, description, aiMutable, userOnlyReason}`. The AI may change every setting except those with `userOnlyReason`: instance key and login credentials, OS microphone permission, and the setting that revokes AI settings permission. Window and presence settings are ordinary AI-mutable settings. Every AI change shows a subtle notice and creates a reviewable, undoable audit record.

## Client tools

`surface.open`, `surface.update`, `surface.close`, `surface.list`, `components.describe`, `settings.list`, `settings.read(key)`, `settings.write(key, value)`, `settings.undo(recordId)`, `voice.start()`, `voice.end()`, `voice.setSilenceTimeout(seconds)`, `textInput.show()`, `textInput.hide()`.

Presence floors that hold regardless of tools: the menu-bar item always exists, so a hidden app can be found again; and while the microphone is in use the menu-bar item shows that state.

## Authority rule

Tool calls and node actions never decide business outcomes. They submit authenticated opaque intents plus `args`; the server validates authority, scope, expected version, and expiry, then returns authoritative state or a new document. Voice requests are accepted by the server under the impact levels in [ADR 0009](../architecture/decisions/0009-voice-steward-impact-policy.md); voice never confirms high-impact work. With no server document yet, the client shows a plain chronological list of authoritative tasks, approvals, and notifications through the same renderer, without ranking or invented content.
