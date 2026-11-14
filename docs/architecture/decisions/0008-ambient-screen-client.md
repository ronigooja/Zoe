# ADR 0008: Ambient macOS screen client

Status: design accepted 2026-11-14; implementation pending (Go contract and Swift renderer not yet changed). The previous conversation-centered direction and its ADR 0003 were removed; iPhone and iPad have no approved interface design until a later task. The [master plan](../../master-plan.md) owns delivery status and the [evidence register](../../acceptance/evidence.md) owns evidence boundaries.

2026-11-14: removed the Zoe standard-component library: the server builder produced six node types and the client default screen four, and no client read the server output. Use the A2UI basic catalog instead; add custom components only when needed, with none defined now. No backward compatibility with the removed format is required.

## Context

The macOS client should reduce required operation rather than reproduce a chat app. A generic display must support changing work without granting the client outcome authority.

## Decision

Make macOS an ambient screen with no sidebar, tabs, or persistent composer, and no fixed regions. Continuous voice is on by default and ends after the configurable silence timeout (default 60 seconds when no voice interaction is in progress); server context supports later continuation. Text is a transient fallback.

The AI controls its own presentation through Zoe surfaces: windows, panels, menu-bar items, popovers, and Dock badges that it opens, updates, and closes. Zoe owns this outer platform shell; A2UI owns the inner messages. Use only the A2UI basic catalog rendered by a2ui-swift, with protocol version and fixed renderer revision defined in the [A2UI screen contract](../../design/screen-document.md#protocol-and-renderer).

The client composes messages locally from authoritative tasks, approvals, and notifications; the server does not generate UI documents. Existing delivery remains unchanged as defined in the [composition and delivery contract](../../design/screen-document.md#local-composition-and-delivery). A2UI actions submit opaque intents and args for server validation, never client-decided outcomes.

Retain the surface, settings, voice, text-input, and silence-timeout tools, but remove the component-description tool. Settings metadata, user-only exceptions, change notices, audit/undo, and the menu-bar presence and microphone-use floors follow the [contract](../../design/screen-document.md#settings).

Implementation direction: a2ui-swift renders content; AppKit owns windows (`NSPanel`/`NSWindow` hosting `NSHostingView`, `NSStatusItem`, `NSPopover`, activation policy, `dockTile`) through a macOS-only `SurfaceManager`, because SwiftUI scenes cannot be created freely at runtime. Settings use the SwiftUI `Settings` scene. Models and the surface-host protocol stay in cross-platform ZoeKit; AppKit code is under `#if os(macOS)`. Window behavior cannot be unit tested; it needs a GitHub macOS runner build plus manual device checks and must not be reported as verified before then.

## Consequences

The client remains responsible for presentation while the server controls authoritative content and business policy. Protocol upgrades depend on renderer support rather than a Zoe component library. High-impact actions still require server-validated scoped confirmation ([ADR 0009](0009-voice-steward-impact-policy.md)); the client never decides outcomes. Dependency compatibility, catalog registration, and licensing remain [open items](../../design/screen-document.md#open-items), not implementation or verification claims.

## Sources

[Product §3](../../product.md#3-client-requirements); [A2UI screen contract](../../design/screen-document.md); [client architecture](../../architecture.md#ambient-macos-screen-and-client-tools-boundary).
