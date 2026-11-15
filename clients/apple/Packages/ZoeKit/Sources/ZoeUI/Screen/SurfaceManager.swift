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
        if !errors.isEmpty { onRenderError?(spec.id, errors) }
        let controller = hostingController(model, id: spec.id)
        var entry = Entry(viewModel: model)
        switch spec.kind {
        case .window:
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 480, height: 400), styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
            window.isReleasedWhenClosed = false
            window.identifier = NSUserInterfaceItemIdentifier(spec.id)
            window.delegate = self
            window.contentViewController = controller
            window.minSize = NSSize(width: 320, height: 240)
            window.setContentSize(NSSize(width: 480, height: 400))
            window.center()
            NSApp.activate(ignoringOtherApps: true)
            window.makeKeyAndOrderFront(nil)
            entry.window = window
        case .popover:
            let popover = NSPopover()
            popover.delegate = self
            popover.behavior = .transient
            popover.contentViewController = controller
            popover.contentSize = NSSize(width: 400, height: 360)
            entry.popover = popover
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
        var errors: [Error] = []
        for message in messages {
            if case .createSurface = message { model = SurfaceViewModel(catalog: basicCatalog) }
            errors.append(contentsOf: model.processMessages([message]))
        }
        if !errors.isEmpty { onRenderError?(id, errors) }
        entries[id] = Entry(viewModel: model, window: entry.window, popover: entry.popover)
        if let window = entry.window {
            let contentSize = window.contentRect(forFrameRect: window.frame).size
            window.contentViewController = hostingController(model, id: id)
            window.setContentSize(contentSize)
        }
        if let popover = entry.popover {
            popover.contentViewController = hostingController(model, id: id)
            popover.contentSize = NSSize(width: 400, height: 360)
        }
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
        store.close(id: id)
        guard let entry = entries.removeValue(forKey: id) else { return }
        entry.window?.delegate = nil
        entry.window?.close()
        entry.popover?.delegate = nil
        entry.popover?.performClose(nil)
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

    private func hostingController(_ model: SurfaceViewModel, id: String) -> NSViewController {
        let controller = NSHostingController(rootView: A2UISurfaceView(viewModel: model, onAction: { [weak self] action in
            Task { @MainActor [weak self] in self?.actionHandler(id, action) }
        }))
        controller.sizingOptions = []
        return controller
    }
}
#endif
