#if os(macOS)
import AppKit
import SwiftUI
import ZoeKit
import A2UISwiftCore
import A2UISwiftUI

final class ZoePanel: NSPanel {
    var permitsKey = true
    override var canBecomeKey: Bool { permitsKey }
    override var canBecomeMain: Bool { permitsKey }
}

public final class SurfaceManager: NSObject, SurfaceHost, NSWindowDelegate {
    private struct Entry {
        var spec: SurfaceSpec
        var window: NSWindow?
        var popover: NSPopover?
        var statusItem: NSStatusItem?
        var viewModel: SurfaceViewModel
        var lastUserWindowAction: Date?
    }

    private var entries: [String: Entry] = [:]
    private let statusBar = NSStatusBar.system
    private var floorStatusItem: NSStatusItem!
    private var actionHandler: (ScreenAction) -> Void

    public init(actionHandler: @escaping (ScreenAction) -> Void = { _ in }) {
        self.actionHandler = actionHandler
        super.init()
        installStatusFloor()
    }

    func setActionHandler(_ handler: @escaping (ScreenAction) -> Void) { actionHandler = handler }

    public func open(_ spec: SurfaceSpec) {
        if entries[spec.id] != nil { update(id: spec.id, messages: spec.messages, window: spec.window); return }
        let viewModel = SurfaceViewModel(catalog: basicCatalog)
        // Ignore lifecycle teardown before the first create on a newly opened native surface.
        let initialMessages = spec.messages.drop(while: { if case .deleteSurface = $0 { return true }; return false })
        viewModel.processMessages(Array(initialMessages))
        let entry = Entry(spec: spec, window: nil, popover: nil, statusItem: nil, viewModel: viewModel, lastUserWindowAction: nil)
        entries[spec.id] = entry
        switch spec.kind {
        case .window, .panel: openWindow(spec)
        case .statusItem: openStatusItem(spec)
        case .popover: openPopover(spec)
        case .dockBadge: renderDockBadge(spec)
        }
    }

    public func update(id: String, messages: [A2uiMessage]?, window: SurfaceWindowSpec?) {
        guard var entry = entries[id] else { return }
        if let messages {
            // A new create starts a fresh renderer, including its data model and subscriptions.
            // The native window survives so user positioning and the action-wins guard survive too.
            if messages.contains(where: { if case .createSurface = $0 { return true }; return false }) {
                entry.spec.messages = messages
            } else {
                entry.spec.messages += messages
            }
            for message in messages {
                if case .createSurface = message { entry.viewModel = SurfaceViewModel(catalog: basicCatalog) }
                entry.viewModel.processMessages([message])
            }
        }
        if let window {
            let recentUserAction = entry.lastUserWindowAction.map { Date().timeIntervalSince($0) < 2 } ?? false
            if !recentUserAction { entry.spec.window = window }
        }
        entries[id] = entry
        switch entry.spec.kind {
        case .window, .panel:
            entry.window?.contentViewController = hostingController(for: entry.spec, viewModel: entry.viewModel)
            applyWindow(entry.spec, to: entry.window)
        case .statusItem: updateStatusItem(entry.spec, item: entry.statusItem)
        case .popover: entry.popover?.contentViewController = hostingController(for: entry.spec, viewModel: entry.viewModel)
        case .dockBadge: renderDockBadge(entry.spec)
        }
    }

    public func close(id: String) {
        guard let entry = entries.removeValue(forKey: id) else { return }
        entry.window?.delegate = nil
        entry.window?.close()
        entry.popover?.performClose(nil)
        if let item = entry.statusItem { statusBar.removeStatusItem(item) }
    }

    public func openSurfaceIDs() -> [String] { Array(entries.keys) }

    public func showMain(messages: [A2uiMessage], perform: ((ScreenAction) -> Void)? = nil) {
        let spec = SurfaceSpec(id: "main", kind: .window, messages: messages)
        open(spec)
        if let perform { _ = perform }
    }

    public func markVoiceActive(_ active: Bool) {
        floorStatusItem.button?.image = NSImage(systemSymbolName: active ? "mic.fill" : "circle.fill", accessibilityDescription: active ? "Microphone in use" : "Zoe")
        floorStatusItem.button?.toolTip = active ? "Microphone in use" : "Zoe"
    }

    private func installStatusFloor() {
        floorStatusItem = statusBar.statusItem(withLength: NSStatusItem.squareLength)
        floorStatusItem.button?.image = NSImage(systemSymbolName: "circle.fill", accessibilityDescription: "Zoe")
        floorStatusItem.button?.target = self
        floorStatusItem.button?.action = #selector(statusFloorClicked)
        floorStatusItem.button?.toolTip = "Zoe"
    }

    @objc private func statusFloorClicked() {
        if let popover = entries.values.compactMap(\.popover).first {
            if popover.isShown { popover.performClose(nil) }
            else if let button = floorStatusItem.button { popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY) }
        }
        NotificationCenter.default.post(name: .zoeWakeVoice, object: nil)
    }

    private func openWindow(_ spec: SurfaceSpec) {
        let panel: NSWindow
        if spec.kind == .panel {
            var style: NSWindow.StyleMask = [.titled, .closable, .resizable, .fullSizeContentView]
            if spec.window.takesFocus == false { style.insert(.nonactivatingPanel) }
            panel = ZoePanel(contentRect: .zero, styleMask: style, backing: .buffered, defer: false)
        }
        else { panel = NSWindow(contentRect: .zero, styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false) }
        panel.delegate = self
        panel.identifier = NSUserInterfaceItemIdentifier(spec.id)
        panel.contentViewController = hostingController(for: spec, viewModel: entries[spec.id]!.viewModel)
        entries[spec.id]?.window = panel
        applyWindow(spec, to: panel)
    }

    private func applyWindow(_ spec: SurfaceSpec, to window: NSWindow?) {
        guard let window else { return }
        let display = NSScreen.screens[safe: spec.window.display ?? 0] ?? NSScreen.main
        let visible = display?.visibleFrame ?? NSScreen.main?.visibleFrame ?? CGRect(x: 0, y: 0, width: 800, height: 600)
        let width = min(max(240, spec.window.width ?? 640), visible.width)
        let height = min(max(120, spec.window.height ?? 480), visible.height)
        var frame = CGRect(x: spec.window.x ?? visible.midX - width / 2, y: spec.window.y ?? visible.midY - height / 2, width: width, height: height)
        frame.origin.x = min(max(frame.origin.x, visible.minX), visible.maxX - frame.width)
        frame.origin.y = min(max(frame.origin.y, visible.minY), visible.maxY - frame.height)
        window.setFrame(frame, display: true)
        window.level = spec.window.floating == true ? .floating : .normal
        window.alphaValue = CGFloat(max(0, min(1, spec.window.opacity ?? 1)))
        window.ignoresMouseEvents = spec.window.clickThrough == true
        if let panel = window as? ZoePanel { panel.permitsKey = spec.window.takesFocus != false }
        if spec.window.fullscreen == true { window.toggleFullScreen(nil) }
        if spec.window.showInDock == true { NSApp.setActivationPolicy(.regular) }
        if spec.window.showInDock == false { NSApp.setActivationPolicy(.accessory) }
        let transparent = spec.window.material != nil
        window.isOpaque = !transparent
        window.backgroundColor = transparent ? .clear : .windowBackgroundColor
        if spec.window.visible != false { window.makeKeyAndOrderFront(nil) } else { window.orderOut(nil) }
    }

    private func openStatusItem(_ spec: SurfaceSpec) {
        let item = statusBar.statusItem(withLength: NSStatusItem.variableLength)
        updateStatusItem(spec, item: item)
        entries[spec.id]?.statusItem = item
    }

    private func updateStatusItem(_ spec: SurfaceSpec, item: NSStatusItem?) {
        guard let button = item?.button else { return }
        button.image = statusImage(for: spec)
        button.title = firstTextLabel(in: spec.messages) ?? ""
        button.toolTip = spec.id
        button.target = self
        button.action = #selector(statusItemClicked(_:))
    }

    @objc private func statusItemClicked(_ sender: NSStatusBarButton) {
        guard let id = entries.first(where: { $0.value.statusItem?.button === sender })?.key, let popover = entries[id]?.popover else { return }
        popover.show(relativeTo: sender.bounds, of: sender, preferredEdge: .minY)
    }

    private func openPopover(_ spec: SurfaceSpec) {
        let popover = NSPopover()
        popover.behavior = .transient
        popover.contentViewController = hostingController(for: spec, viewModel: entries[spec.id]!.viewModel)
        entries[spec.id]?.popover = popover
        if let button = floorStatusItem.button { popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY) }
    }

    private func renderDockBadge(_ spec: SurfaceSpec) {
        let image = NSImage(size: NSSize(width: 128, height: 128), flipped: false) { rect in
            NSColor.controlAccentColor.setFill(); NSBezierPath(roundedRect: rect.insetBy(dx: 8, dy: 8), xRadius: 20, yRadius: 20).fill(); return true
        }
        NSApp.dockTile.contentView = NSImageView(image: image)
        NSApp.dockTile.badgeLabel = firstTextLabel(in: spec.messages)
        NSApp.dockTile.display()
    }

    private func hostingController(for spec: SurfaceSpec, viewModel: SurfaceViewModel) -> NSViewController {
        let base = surfaceView(viewModel)
        let view = Group { if let material = spec.window.material { base.background(MaterialView(material: material, opacity: spec.window.opacity ?? 1)) } else { base } }
        return NSHostingController(rootView: view)
    }

    private func statusImage(for spec: SurfaceSpec) -> NSImage {
        guard let viewModel = entries[spec.id]?.viewModel else { return NSImage(size: .zero) }
        let hosting = NSHostingView(rootView: surfaceView(viewModel).frame(width: 20, height: 20))
        hosting.frame = CGRect(x: 0, y: 0, width: 20, height: 20)
        hosting.layoutSubtreeIfNeeded()
        let image = NSImage(size: hosting.bounds.size)
        guard let representation = hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds) else { return image }
        hosting.cacheDisplay(in: hosting.bounds, to: representation)
        image.addRepresentation(representation)
        return image
    }

    private func surfaceView(_ viewModel: SurfaceViewModel) -> some View {
        A2UISurfaceView(viewModel: viewModel, onAction: { [weak self] action in
            let screenAction = ScreenAction(action)
            Task { @MainActor [weak self] in self?.actionHandler(screenAction) }
        })
    }

    public func windowWillClose(_ notification: Notification) {
        guard let window = notification.object as? NSWindow, let id = window.identifier?.rawValue, var entry = entries[id] else { return }
        entry.lastUserWindowAction = Date(); entries[id] = entry
    }

    public func windowDidMove(_ notification: Notification) { recordWindowAction(notification) }
    public func windowDidResize(_ notification: Notification) { recordWindowAction(notification) }
    private func recordWindowAction(_ notification: Notification) { guard let window = notification.object as? NSWindow, let id = window.identifier?.rawValue, var entry = entries[id] else { return }; entry.lastUserWindowAction = Date(); entries[id] = entry }
}

private extension Array {
    subscript(safe index: Index) -> Element? { indices.contains(index) ? self[index] : nil }
}

public extension Notification.Name { static let zoeWakeVoice = Notification.Name("ZoeWakeVoice") }
#endif
