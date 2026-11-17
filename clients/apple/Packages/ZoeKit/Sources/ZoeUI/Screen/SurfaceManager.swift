#if os(macOS)
import AppKit
import SwiftUI
import ZoeKit
import A2UISwiftCore
import A2UISwiftUI

/// On-demand A2UI containers, independent of the application's two-item status menu.
@MainActor public final class SurfaceManager: NSObject, SurfaceHost, NSWindowDelegate, NSPopoverDelegate {
    private struct Entry {
        let viewModel: SurfaceViewModel
        var window: NSWindow?
        var popover: NSPopover?
    }

    private var store = SurfaceStore()
    private var entries: [String: Entry] = [:]
    private let actionHandler: (String, ResolvedAction) -> Void
    public var onBoundAction: ((UISurfaceBinding, ResolvedAction) -> Void)?
    private var bindings: [String: UISurfaceBinding] = [:]
    private var revisions: [String: Int64] = [:]
    private var physicalBindings: [String: UISurfaceBinding] = [:]
    public func isCurrentPresentation(_ event: UISurfaceEvent) -> Bool {
        physicalBindings[event.surfaceID] == event.binding &&
            (event.deleted ? entries[event.surfaceID] == nil : entries[event.surfaceID] != nil)
    }
    private var renderFailed = Set<String>()
    // Test seam at the native presentation boundary, after real message processing.
    var presentationAcceptedForTesting: (() -> Bool)?
    func presentationVersionForTesting(_ id: String) -> Int64? { revisions[id] }
    private let popoverAnchor: () -> NSView?
    public var onUserClosed: (() -> Void)?
    public var onRenderError: ((String, [Error]) -> Void)?

    public init(popoverAnchor: @escaping () -> NSView?, actionHandler: @escaping (String, ResolvedAction) -> Void = { _, _ in }) {
        self.popoverAnchor = popoverAnchor
        self.actionHandler = actionHandler
        super.init()
    }

    public func open(_ spec: SurfaceSpec) {
        close(id: spec.id)
        store.open(spec)
        let model = SurfaceViewModel(catalog: basicCatalog)
        let errors = model.processMessages(store.surfaces[spec.id]!.messages)
        model.a2uiStyle = Self.fixedStyle
        if !errors.isEmpty { renderFailed.insert(spec.id); onRenderError?(spec.id, errors) }
        let controller = hostingController(model, id: spec.id, kind: spec.kind)
        var entry = Entry(viewModel: model)
        switch spec.kind {
        case .window:
            let size = clampedSize(spec.size, kind: .window)
            let window = NSWindow(contentRect: NSRect(origin: .zero, size: size), styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
            window.isReleasedWhenClosed = false
            window.identifier = NSUserInterfaceItemIdentifier(spec.id)
            window.delegate = self
            window.contentViewController = controller
            window.minSize = NSSize(width: 360, height: 300)
            window.setContentSize(size)
            window.center()
            NSApp.activate(ignoringOtherApps: true)
            window.makeKeyAndOrderFront(nil)
            entry.window = window
        case .popover:
            let popover = NSPopover()
            popover.delegate = self
            popover.behavior = .transient
            popover.contentViewController = controller
            popover.contentSize = clampedSize(spec.size, kind: .popover)
            entry.popover = popover
        }
        entries[spec.id] = entry
        if spec.kind == .popover, let anchor = popoverAnchor() {
            entry.popover?.show(relativeTo: anchor.bounds, of: anchor, preferredEdge: .minY)
        }
    }

    public func update(id: String, messages: [A2uiMessage]) {
        update(id: id, messages: messages, size: nil)
    }

    private func update(id: String, messages: [A2uiMessage], size: SurfaceSize?) {
        guard let entry = entries[id] else { return }
        store.update(id: id, messages: messages)
        var model = entry.viewModel
        var errors: [Error] = []
        for message in messages {
            if case .createSurface = message { model = SurfaceViewModel(catalog: basicCatalog) }
            errors.append(contentsOf: model.processMessages([message]))
            model.a2uiStyle = Self.fixedStyle
        }
        if !errors.isEmpty { renderFailed.insert(id); onRenderError?(id, errors) }
        entries[id] = Entry(viewModel: model, window: entry.window, popover: entry.popover)
        if let window = entry.window {
            let contentSize = window.contentRect(forFrameRect: window.frame).size
            window.contentViewController = hostingController(model, id: id, kind: .window)
            window.setContentSize(size.map { clampedSize($0, kind: .window) } ?? contentSize)
        }
        if let popover = entry.popover {
            let contentSize = popover.contentSize
            popover.contentViewController = hostingController(model, id: id, kind: .popover)
            popover.contentSize = size.map { clampedSize($0, kind: .popover) } ?? contentSize
        }
    }

    /// Only a newer server revision presents a dismissed surface again.
    @discardableResult public func display(_ event: UISurfaceEvent) -> Bool {
        guard event.surfaceVersion > (revisions[event.surfaceID] ?? 0) else { return false }
        if event.deleted {
            close(id: event.surfaceID)
            guard entries[event.surfaceID] == nil else { return false }
            revisions[event.surfaceID] = event.surfaceVersion
            physicalBindings[event.surfaceID] = event.binding
            bindings.removeValue(forKey: event.surfaceID)
            return true
        }
        let previousBinding = bindings[event.surfaceID]
        bindings[event.surfaceID] = event.binding
        guard display(SurfaceSpec(id: event.surfaceID, kind: event.container, messages: event.messages, size: event.size)) else {
            bindings[event.surfaceID] = previousBinding
            return false
        }
        revisions[event.surfaceID] = event.surfaceVersion
        physicalBindings[event.surfaceID] = event.binding
        return true
    }

    @discardableResult public func display(_ spec: SurfaceSpec) -> Bool {
        physicalBindings.removeValue(forKey: spec.id)
        renderFailed.remove(spec.id)
        if entries[spec.id] != nil, store.surfaces[spec.id]?.kind == spec.kind {
            update(id: spec.id, messages: spec.messages, size: spec.size)
        } else {
            open(spec)
        }
        guard let entry = entries[spec.id] else { return false }
        guard !renderFailed.contains(spec.id) else {
            close(id: spec.id)
            return false
        }
        if presentationAcceptedForTesting?() == false {
            close(id: spec.id)
            return false
        }
        switch spec.kind {
        case .window:
            NSApp.activate(ignoringOtherApps: true)
            entry.window?.makeKeyAndOrderFront(nil)
            guard let size = entry.window?.contentView?.frame.size,
                  size.width >= 50, size.height >= 50 else { return false }
            return entry.window?.isVisible == true
        case .popover:
            guard let anchor = popoverAnchor() else { return false }
            if entry.popover?.isShown != true {
                entry.popover?.show(relativeTo: anchor.bounds, of: anchor, preferredEdge: .minY)
            }
            return entry.popover?.isShown == true
        }
    }

    public func close(id: String) {
        physicalBindings.removeValue(forKey: id)
        store.close(id: id)
        guard let entry = entries.removeValue(forKey: id) else { return }
        entry.window?.delegate = nil
        entry.window?.close()
        entry.popover?.delegate = nil
        entry.popover?.performClose(nil)
    }

    /// Clear revision fences only when the authenticated connection scope is discarded.
    public func reset() {
        for id in Array(entries.keys) { close(id: id) }
        bindings.removeAll()
        revisions.removeAll()
        physicalBindings.removeAll()
    }

    public func windowWillClose(_ notification: Notification) {
        if let window = notification.object as? NSWindow, let id = window.identifier?.rawValue {
            onUserClosed?()
            close(id: id)
        }
    }

    public func popoverDidClose(_ notification: Notification) {
        onUserClosed?()
    }

    static var fixedStyle: A2UIStyle {
        A2UIStyle(primaryColor: .accentColor, textStyles: [
            "h1": .init(font: .system(size: 22), weight: .bold),
            "h2": .init(font: .system(size: 18), weight: .semibold),
            "h3": .init(font: .system(size: 15), weight: .semibold),
            "body": .init(font: .system(size: 13)),
            "caption": .init(font: .system(size: 11), color: .secondary)
        ], cardStyle: .init(padding: 16, cornerRadius: 12, shadowRadius: 1,
                           shadowColor: Color(nsColor: .separatorColor), shadowY: 0, backgroundColor: .clear))
    }

    func viewModel(for id: String) -> SurfaceViewModel? { entries[id]?.viewModel }
    func popoverContentSize(for id: String) -> NSSize? { entries[id]?.popover?.contentSize }

    private func clampedSize(_ requested: SurfaceSize?, kind: SurfaceKind) -> NSSize {
        let minimum = kind == .window ? NSSize(width: 320, height: 240) : NSSize(width: 280, height: 200)
        let fallback = kind == .window ? NSSize(width: 520, height: 640) : NSSize(width: 400, height: 480)
        let desired = requested.map { NSSize(width: $0.width, height: $0.height) } ?? fallback
        let visible = (kind == .popover ? (popoverAnchor()?.window?.screen ?? NSScreen.main) : NSScreen.main)?.visibleFrame.size ?? fallback
        return NSSize(width: min(max(desired.width, minimum.width), max(1, visible.width - 20)),
                      height: min(max(desired.height, minimum.height), max(1, visible.height - 20)))
    }

    private func hostingController(_ model: SurfaceViewModel, id: String, kind: SurfaceKind) -> NSViewController {
        let handler = capturedActionHandler(id: id)
        let controller = NSHostingController(rootView: A2UISurfaceView(viewModel: model, onAction: handler))
        controller.sizingOptions = []
        if kind == .popover {
            // Let NSPopover's system material show through; windows retain their normal background.
            controller.view.wantsLayer = true
            controller.view.layer?.backgroundColor = NSColor.clear.cgColor
        }
        return controller
    }

    // Shared with the native hosting view so fixtures exercise the actual renderer closure.
    func capturedActionHandler(id: String) -> (ResolvedAction) -> Void {
        // Capture the renderer's immutable identity, not the manager's newest mapping at dispatch time.
        let binding = bindings[id]
        return { [weak self] action in
            Task { @MainActor [weak self] in
                if let binding { self?.onBoundAction?(binding, action) }
                else { self?.actionHandler(id, action) }
            }
        }
    }
}
#endif
