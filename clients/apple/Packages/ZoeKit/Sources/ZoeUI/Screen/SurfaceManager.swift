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
    private var awaitingInput = Set<String>()
    private var lostFocusAt: [String: Date] = [:]
    private var closeTasks: [String: Task<Void, Never>] = [:]
    private var localMonitor: Any?
    private var globalMonitor: Any?
    public var popoverCloseDelaySeconds: Double = 60 { didSet { rescheduleClosures() } }
    public var windowCloseDelaySeconds: Double = 60 { didSet { rescheduleClosures() } }
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
        for name in [NSWindow.didBecomeKeyNotification, NSWindow.didResignKeyNotification,
                     NSApplication.didBecomeActiveNotification, NSApplication.didResignActiveNotification] {
            NotificationCenter.default.addObserver(self, selector: #selector(focusChanged), name: name, object: nil)
        }
        NSWorkspace.shared.notificationCenter.addObserver(self, selector: #selector(spaceChanged), name: NSWorkspace.activeSpaceDidChangeNotification, object: nil)
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
            // Explicit dismissal separates outside clicks from delayed non-click focus loss.
            // Transient behavior cannot provide that distinction for an LSUIElement app.
            popover.behavior = .applicationDefined
            popover.contentViewController = controller
            popover.contentSize = clampedSize(spec.size, kind: .popover)
            entry.popover = popover
        }
        entries[spec.id] = entry
        if spec.kind == .popover, let anchor = popoverAnchor() {
            NSApp.activate(ignoringOtherApps: true)
            entry.popover?.show(relativeTo: anchor.bounds, of: anchor, preferredEdge: .minY)
            entry.popover?.contentViewController?.view.window?.makeKey()
        }
        refreshEventMonitors()
        focusChanged(Notification(name: NSWindow.didBecomeKeyNotification))
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
        updateInputState(event)
        return true
    }

    /// Snapshot input state can change without a new presentation revision (answer received).
    public func updateInputState(_ event: UISurfaceEvent) {
        guard entries[event.surfaceID] != nil else { return }
        if event.awaitingInput { awaitingInput.insert(event.surfaceID) }
        else { awaitingInput.remove(event.surfaceID) }
        scheduleClosure(id: event.surfaceID)
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
                entry.popover?.contentViewController?.view.window?.makeKey()
            }
            refreshEventMonitors()
            return entry.popover?.isShown == true
        }
    }

    public func close(id: String) {
        closeTasks.removeValue(forKey: id)?.cancel()
        lostFocusAt.removeValue(forKey: id)
        awaitingInput.remove(id)
        physicalBindings.removeValue(forKey: id)
        store.close(id: id)
        guard let entry = entries.removeValue(forKey: id) else { return }
        entry.window?.delegate = nil
        entry.window?.close()
        entry.popover?.delegate = nil
        entry.popover?.performClose(nil)
        refreshEventMonitors()
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
        if let popover = notification.object as? NSPopover,
           let id = entries.first(where: { $0.value.popover === popover })?.key {
            close(id: id)
        }
        onUserClosed?()
    }

    private func dismiss(id: String) {
        guard let entry = entries[id] else { return }
        if let popover = entry.popover, popover.isShown { popover.performClose(nil) }
        else if let window = entry.window { window.performClose(nil) }
    }

    private func refreshEventMonitors() {
        let needsMonitors = entries.values.contains { $0.popover?.isShown == true }
        if !needsMonitors {
            if let localMonitor { NSEvent.removeMonitor(localMonitor) }
            if let globalMonitor { NSEvent.removeMonitor(globalMonitor) }
            localMonitor = nil
            globalMonitor = nil
        } else if localMonitor == nil {
            localMonitor = NSEvent.addLocalMonitorForEvents(matching: [.leftMouseDown, .rightMouseDown, .keyDown]) { [weak self] event in
                MainActor.assumeIsolated {
                    let dismissed = self?.handleDismissal(event, global: false) ?? false
                    return dismissed && event.type == .keyDown ? nil : event
                }
            }
            globalMonitor = NSEvent.addGlobalMonitorForEvents(matching: [.leftMouseDown, .rightMouseDown]) { [weak self] event in
                MainActor.assumeIsolated { _ = self?.handleDismissal(event, global: true) }
            }
        }
    }

    private func handleDismissal(_ event: NSEvent, global: Bool) -> Bool {
        var dismissed = false
        for (id, entry) in Array(entries) where entry.popover?.isShown == true {
            let inside = !global && event.window === entry.popover?.contentViewController?.view.window
            if SurfaceClosePolicy.shouldDismissPopover(isEscape: event.type == .keyDown && event.keyCode == 53,
                isMouseDown: event.type == .leftMouseDown || event.type == .rightMouseDown, isInside: inside) {
                dismiss(id: id)
                dismissed = true
            }
        }
        return dismissed
    }

    @objc private func focusChanged(_ notification: Notification) {
        for (id, entry) in entries {
            let window = entry.window ?? entry.popover?.contentViewController?.view.window
            let focused = NSApp.isActive && window?.isKeyWindow == true
            if focused { lostFocusAt.removeValue(forKey: id) }
            else if lostFocusAt[id] == nil { lostFocusAt[id] = Date() }
            scheduleClosure(id: id)
        }
    }

    @objc private func spaceChanged(_ notification: Notification) {
        focusChanged(notification)
    }

    private func rescheduleClosures() {
        for id in entries.keys { scheduleClosure(id: id) }
    }

    private func scheduleClosure(id: String) {
        closeTasks.removeValue(forKey: id)?.cancel()
        guard let entry = entries[id], let deadline = SurfaceClosePolicy.deadline(lostFocusAt: lostFocusAt[id],
            delay: entry.popover == nil ? windowCloseDelaySeconds : popoverCloseDelaySeconds,
            awaitingInput: awaitingInput.contains(id)) else { return }
        let seconds = max(0, deadline.timeIntervalSinceNow)
        closeTasks[id] = Task { @MainActor [weak self] in
            do { try await Task.sleep(for: .seconds(seconds)) } catch { return }
            guard !Task.isCancelled, let self, !self.awaitingInput.contains(id), self.lostFocusAt[id] != nil else { return }
            self.dismiss(id: id)
        }
    }

    // Host-owned spacing in points; rendering facts in internal/uictl/controller.go
    // and docs/design/screen-document.md track these values and fixedStyle together.
    static let surfacePadding: CGFloat = 8
    static let cardPadding: CGFloat = 8
    static let leafMargin: CGFloat = 4

    static var fixedStyle: A2UIStyle {
        A2UIStyle(primaryColor: .accentColor, textStyles: [
            "h1": .init(font: .system(size: 22), weight: .bold),
            "h2": .init(font: .system(size: 18), weight: .semibold),
            "h3": .init(font: .system(size: 15), weight: .semibold),
            "h4": .init(font: .system(size: 14), weight: .semibold),
            "h5": .init(font: .system(size: 13), weight: .semibold),
            "body": .init(font: .system(size: 13)),
            "caption": .init(font: .system(size: 11), color: .secondary)
        ], cardStyle: .init(padding: cardPadding, cornerRadius: 12, backgroundColor: .clear),
           leafMargin: leafMargin)
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

    // Single definition of the container content, shared with the offline render harness.
    @ViewBuilder static func surfaceRootView(_ model: SurfaceViewModel, onAction: @escaping (ResolvedAction) -> Void) -> some View {
        ScrollView(.vertical) {
            A2UISurfaceView(viewModel: model, scrolls: false, onAction: onAction)
                .padding(surfacePadding)
        }
    }

    private func hostingController(_ model: SurfaceViewModel, id: String, kind: SurfaceKind) -> NSViewController {
        let controller = NSHostingController(rootView: Self.surfaceRootView(model, onAction: capturedActionHandler(id: id)))
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
