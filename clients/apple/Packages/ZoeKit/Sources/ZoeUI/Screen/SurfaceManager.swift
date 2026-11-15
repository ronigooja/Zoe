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
        var statusItem: NSStatusItem?
    }

    private var store = SurfaceStore()
    private var entries: [String: Entry] = [:]
    private let actionHandler: (String, ResolvedAction) -> Void
    private let popoverAnchor: () -> NSView?
    public var onUserClosed: (() -> Void)?

    public init(popoverAnchor: @escaping () -> NSView?, actionHandler: @escaping (String, ResolvedAction) -> Void = { _, _ in }) {
        self.popoverAnchor = popoverAnchor
        self.actionHandler = actionHandler
        super.init()
    }

    public func open(_ spec: SurfaceSpec) {
        close(id: spec.id)
        store.open(spec)
        let model = SurfaceViewModel(catalog: basicCatalog)
        model.processMessages(store.surfaces[spec.id]!.messages)
        let controller = hostingController(model, id: spec.id)
        var entry = Entry(viewModel: model)
        switch spec.kind {
        case .window:
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 480, height: 400), styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
            window.isReleasedWhenClosed = false
            window.identifier = NSUserInterfaceItemIdentifier(spec.id)
            window.delegate = self
            window.contentViewController = controller
            window.center()
            NSApp.activate(ignoringOtherApps: true)
            window.makeKeyAndOrderFront(nil)
            entry.window = window
        case .statusItem, .popover:
            let popover = NSPopover()
            popover.delegate = self
            popover.behavior = .transient
            popover.contentSize = NSSize(width: 400, height: 360)
            popover.contentViewController = controller
            entry.popover = popover
            if spec.kind == .statusItem {
                let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
                item.button?.image = NSImage(systemSymbolName: "rectangle.on.rectangle", accessibilityDescription: "Zoe surface")
                item.button?.target = self
                item.button?.action = #selector(togglePanel(_:))
                entry.statusItem = item
            }
        }
        entries[spec.id] = entry
        if spec.kind == .popover, let anchor = popoverAnchor() {
            entry.popover?.show(relativeTo: anchor.bounds, of: anchor, preferredEdge: .minY)
        }
    }

    public func update(id: String, messages: [A2uiMessage]) {
        guard let entry = entries[id] else { return }
        store.update(id: id, messages: messages)
        var model = entry.viewModel
        for message in messages {
            if case .createSurface = message { model = SurfaceViewModel(catalog: basicCatalog) }
            model.processMessages([message])
        }
        entries[id] = Entry(viewModel: model, window: entry.window, popover: entry.popover, statusItem: entry.statusItem)
        entry.window?.contentViewController = hostingController(model, id: id)
        entry.popover?.contentViewController = hostingController(model, id: id)
    }

    /// A dismissed popover keeps its renderer; every new server state presents it again.
    @discardableResult public func display(_ spec: SurfaceSpec) -> Bool {
        if entries[spec.id] != nil, store.surfaces[spec.id]?.kind == spec.kind {
            update(id: spec.id, messages: spec.messages)
        } else {
            open(spec)
        }
        guard let entry = entries[spec.id] else { return false }
        switch spec.kind {
        case .window:
            NSApp.activate(ignoringOtherApps: true)
            entry.window?.makeKeyAndOrderFront(nil)
            return entry.window?.isVisible == true
        case .popover:
            guard let anchor = popoverAnchor() else { return false }
            if entry.popover?.isShown != true {
                entry.popover?.show(relativeTo: anchor.bounds, of: anchor, preferredEdge: .minY)
            }
            return entry.popover?.isShown == true
        case .statusItem:
            return entry.statusItem?.isVisible == true
        }
    }

    public func close(id: String) {
        store.close(id: id)
        guard let entry = entries.removeValue(forKey: id) else { return }
        entry.window?.delegate = nil
        entry.window?.close()
        entry.popover?.delegate = nil
        entry.popover?.performClose(nil)
        if let item = entry.statusItem { NSStatusBar.system.removeStatusItem(item) }
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

    @objc private func togglePanel(_ sender: NSStatusBarButton) {
        guard let popover = entries.values.first(where: { $0.statusItem?.button === sender })?.popover else { return }
        if popover.isShown { popover.performClose(nil) }
        else { popover.show(relativeTo: sender.bounds, of: sender, preferredEdge: .minY) }
    }

    private func hostingController(_ model: SurfaceViewModel, id: String) -> NSViewController {
        NSHostingController(rootView: A2UISurfaceView(viewModel: model, onAction: { [weak self] action in
            Task { @MainActor [weak self] in self?.actionHandler(id, action) }
        }))
    }
}
#endif
