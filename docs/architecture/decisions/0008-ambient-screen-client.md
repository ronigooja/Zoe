# ADR 0008: Ambient macOS screen client

Status: accepted macOS architecture baseline; implementation and acceptance remain open. The previous conversation-centered direction and its ADR 0003 were removed; iPhone and iPad have no approved interface design until a later task. The [master plan](../../master-plan.md) owns delivery status and the [evidence register](../../acceptance/evidence.md) owns evidence boundaries.

## Context

The macOS client should reduce required operation rather than reproduce a chat app. A generic server-authored display must support changing work without embedding business concepts or granting the client outcome authority.

## Decision

Make macOS an ambient screen with no sidebar, tabs, or persistent composer, and no fixed regions. Continuous voice is on by default and ends after the configurable silence timeout (default 60 seconds when no voice interaction is in progress); server context supports later continuation. Text is a transient fallback.

The AI controls its own presentation through surfaces: windows, panels, menu-bar items, popovers, and Dock badges that it opens, updates, and closes, including size, position, display, level, opacity, click-through, focus, fullscreen, and Dock visibility. Each surface renders a versioned generic screen document built from layout and content primitives plus standard components named by information shape (metric, timeline, decision, collection, and so on), with client-owned visuals, data binding, size-adaptive detail, and fallback degradation for unknown types. Interactive nodes submit opaque intents; the server decides outcomes. The client exposes surface, component-catalog, settings, voice, text-bar, and silence-timeout tools. Every setting is AI-mutable except instance key and credentials, OS microphone permission, and revoking AI settings access; AI changes are noticed and undoable. Only two floors remain: the menu-bar item always exists, and it shows microphone use. Details: [the draft contract](../../design/screen-document.md).

Implementation: SwiftUI renders content; AppKit owns windows (`NSPanel`/`NSWindow` hosting `NSHostingView`, `NSStatusItem`, `NSPopover`, activation policy, `dockTile`) through a macOS-only `SurfaceManager`, because SwiftUI scenes cannot be created freely at runtime. Settings use the SwiftUI `Settings` scene. Models and the surface-host protocol stay in cross-platform ZoeKit; AppKit code is under `#if os(macOS)`. Window behavior cannot be unit tested; it needs a GitHub macOS runner build plus manual device checks and must not be reported as verified before then.

## Consequences

The client can remain small, accessible, and adaptable while the server controls authoritative content and business policy. The renderer and component library become versioned compatibility surfaces. High-impact actions still require server-validated scoped confirmation ([ADR 0009](0009-voice-steward-impact-policy.md)); the client never decides outcomes. The draft contract must be mirrored into `/contracts` before server implementation.

## Sources

[Product §3](../../product.md#3-client-requirements); [ambient screen contract draft](../../design/screen-document.md); [client architecture](../../architecture.md#ambient-macos-screen-and-client-tools-boundary).
