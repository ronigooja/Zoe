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
    private var subscriptions = Set<AnyCancellable>()
    private lazy var surfaces = SurfaceManager(popoverAnchor: { [weak self] in self?.statusItem?.button })

    func applicationDidFinishLaunching(_ notification: Notification) {
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        item.button?.image = NSImage(systemSymbolName: "mic", accessibilityDescription: "Zoe")
        let menu = NSMenu()
        let voice = NSMenuItem(title: "Voice", action: #selector(toggleVoice), keyEquivalent: "")
        voice.target = self
        menu.addItem(voice)
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
        model.$continuousVoiceEnabled.sink { enabled in voice.state = enabled ? .on : .off }.store(in: &subscriptions)
        model.$voiceStatus.sink { status in
            let active: Bool
            if case .active = status { active = true } else { active = false }
            item.button?.image = NSImage(systemSymbolName: active ? "mic.fill" : "mic", accessibilityDescription: active ? "Microphone in use" : "Zoe")
        }.store(in: &subscriptions)
        Task { await model.start() }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func application(_ application: NSApplication, open urls: [URL]) {
        for url in urls { model.receiveInstallationLink(url) }
        showSettings()
    }

    @objc private func toggleVoice() { model.setContinuousVoiceEnabled(!model.continuousVoiceEnabled) }

    @objc private func quitApplication() { NSApp.terminate(nil) }

    @objc private func showSettings() {
        if settingsWindow == nil {
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 520, height: 480), styleMask: [.titled, .closable], backing: .buffered, defer: false)
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
