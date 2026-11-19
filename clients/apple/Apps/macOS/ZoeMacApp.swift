import AppKit
import Combine
import SwiftUI
import ZoeUI
import ZoeKit

@main
struct ZoeMacApp {
    @MainActor static func main() {
        let application = NSApplication.shared
        let delegate = ZoeAppDelegate()
        application.delegate = delegate
        withExtendedLifetime(delegate) { application.run() }
    }
}

@MainActor final class ZoeAppDelegate: NSObject, NSApplicationDelegate {
    private let model = ZoeAppModel()
    private var statusItem: NSStatusItem?
    private var settingsWindow: NSWindow?
    private var textInputWindow: NSWindow?
    private var historyWindow: NSWindow?
    private let historyStore = UISurfaceHistoryStore(root: UISurfaceHistoryStore.defaultRoot())
    private lazy var historyModel = UISurfaceHistoryModel(store: historyStore, scope: { [weak self] in self?.model.historyScope })
    private var subscriptions = Set<AnyCancellable>()
    private lazy var surfaces = SurfaceManager(popoverAnchor: { [weak self] in self?.statusItem?.button })

    func applicationDidFinishLaunching(_ notification: Notification) {
        installMainMenu()
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        item.button?.image = NSImage(systemSymbolName: "mic", accessibilityDescription: "Zoe")
        let menu = NSMenu()
        let voice = NSMenuItem(title: "Voice", action: #selector(toggleVoice), keyEquivalent: "")
        voice.target = self
        menu.addItem(voice)
        let textInput = NSMenuItem(title: "Text Input", action: #selector(showTextInput), keyEquivalent: "")
        textInput.target = self
        menu.addItem(textInput)
        let history = NSMenuItem(title: "History", action: #selector(showHistory), keyEquivalent: "")
        history.target = self
        menu.addItem(history)
        let settings = NSMenuItem(title: "Settings", action: #selector(showSettings), keyEquivalent: "")
        settings.target = self
        menu.addItem(settings)
        let quit = NSMenuItem(title: "Quit", action: #selector(quitApplication), keyEquivalent: "")
        quit.target = self
        menu.addItem(quit)
        item.menu = menu
        statusItem = item
        model.screenVisibleSize = { [weak self] in
            (self?.statusItem?.button?.window?.screen ?? NSScreen.main)?.visibleFrame.size
        }
        NotificationCenter.default.publisher(for: NSApplication.didChangeScreenParametersNotification)
            .sink { [weak self] _ in
                Task { @MainActor [weak self] in await self?.model.reportScreenSize() }
            }.store(in: &subscriptions)
        surfaces.onUserClosed = { [weak self] in self?.model.noteUISurfaceUserClosed() }
        model.$popoverCloseDelaySeconds.sink { [weak self] in self?.surfaces.popoverCloseDelaySeconds = $0 }.store(in: &subscriptions)
        model.$windowCloseDelaySeconds.sink { [weak self] in self?.surfaces.windowCloseDelaySeconds = $0 }.store(in: &subscriptions)
        surfaces.onRenderError = { [weak self] _, errors in self?.model.noteUISurfaceRenderErrors(errors) }
        surfaces.onBoundAction = { [weak self] binding, action in
            self?.model.sendUISurfaceAction(binding: binding, action: action)
        }
        model.onUISurface = { [weak self] event in
            guard let self else { return false }
            return self.surfaces.display(event)
        }
        model.onUISurfaceInputState = { [weak self] in self?.surfaces.updateInputState($0) }
        model.isCurrentUIPresentation = { [weak self] event in
            self?.surfaces.isCurrentPresentation(event) == true
        }
        model.onUISurfacesClosed = { [weak self] ids in
            for id in ids { self?.surfaces.close(id: id) }
        }
        model.onUISurfacesReset = { [weak self] in self?.surfaces.reset() }
        let store = historyStore
        model.onUISurfaceDisplayed = { event, scope in
            try await store.append(scope: scope, id: event.renderedIdempotencyKey, container: event.container, messages: event.messages)
        }
        model.$continuousVoiceEnabled.sink { enabled in voice.state = enabled ? .on : .off }.store(in: &subscriptions)
        model.$voiceStatus.sink { status in
            let active: Bool
            if case .active = status { active = true } else { active = false }
            item.button?.image = NSImage(systemSymbolName: active ? "mic.fill" : "mic", accessibilityDescription: active ? "Microphone in use" : "Zoe")
        }.store(in: &subscriptions)
        Task { await model.start() }
    }

    /// LSUIElement apps have no main menu by default, so standard Edit shortcuts need one.
    private func installMainMenu() {
        let mainMenu = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "Quit Zoe", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        mainMenu.addItem(appItem)
        let editItem = NSMenuItem()
        let edit = NSMenu(title: "Edit")
        edit.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
        let redo = edit.addItem(withTitle: "Redo", action: Selector(("redo:")), keyEquivalent: "z")
        redo.keyEquivalentModifierMask = [.command, .shift]
        edit.addItem(NSMenuItem.separator())
        edit.addItem(withTitle: "Cut", action: Selector(("cut:")), keyEquivalent: "x")
        edit.addItem(withTitle: "Copy", action: Selector(("copy:")), keyEquivalent: "c")
        edit.addItem(withTitle: "Paste", action: Selector(("paste:")), keyEquivalent: "v")
        edit.addItem(withTitle: "Select All", action: Selector(("selectAll:")), keyEquivalent: "a")
        editItem.submenu = edit
        mainMenu.addItem(editItem)
        NSApp.mainMenu = mainMenu
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func application(_ application: NSApplication, open urls: [URL]) {
        for url in urls { model.receiveInstallationLink(url) }
        showSettings()
    }

    @objc private func toggleVoice() { model.setContinuousVoiceEnabled(!model.continuousVoiceEnabled) }

    @objc private func quitApplication() { NSApp.terminate(nil) }

    @objc private func showTextInput() {
        if textInputWindow == nil {
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 520, height: 360), styleMask: [.titled, .closable], backing: .buffered, defer: false)
            window.title = "Text Input"
            window.isReleasedWhenClosed = false
            window.contentViewController = NSHostingController(rootView: ConversationTextInputView(model: model, onCancel: { [weak window] in window?.close() }))
            window.center()
            textInputWindow = window
        }
        NSApp.activate(ignoringOtherApps: true)
        textInputWindow?.makeKeyAndOrderFront(nil)
    }

    @objc private func showHistory() {
        if historyWindow == nil {
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 420, height: 480), styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
            window.title = "History"
            window.isReleasedWhenClosed = false
            window.contentViewController = NSHostingController(rootView: UISurfaceHistoryView(model: historyModel))
            window.center()
            historyWindow = window
        } else {
            Task { await historyModel.reload() }
        }
        NSApp.activate(ignoringOtherApps: true)
        historyWindow?.makeKeyAndOrderFront(nil)
    }

    @objc private func showSettings() {
        if settingsWindow == nil {
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 520, height: 540), styleMask: [.titled, .closable], backing: .buffered, defer: false)
            window.title = "Settings"
            window.isReleasedWhenClosed = false
            window.contentViewController = NSHostingController(rootView: MacSettingsView(model: model))
            window.center()
            settingsWindow = window
        }
        NSApp.activate(ignoringOtherApps: true)
        settingsWindow?.makeKeyAndOrderFront(nil)
    }
}
