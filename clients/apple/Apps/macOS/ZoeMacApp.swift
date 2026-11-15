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
    private lazy var surfaces = SurfaceManager(popoverAnchor: { [weak self] in self?.statusItem?.button }, actionHandler: { [weak self] id, action in
        self?.model.sendUISurfaceAction(surfaceID: id, action: action)
    })

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
        surfaces.onUserClosed = { [weak self] in self?.model.noteUISurfaceUserClosed() }
        model.onUISurface = { [weak self] event in
            guard let self else { return false }
            if event.isDeletion {
                self.surfaces.close(id: event.surfaceID)
                return false
            }
            return self.surfaces.display(SurfaceSpec(id: event.surfaceID, kind: event.container, messages: event.messages))
        }
        model.onUISurfacesClosed = { [weak self] ids in
            for id in ids { self?.surfaces.close(id: id) }
        }
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
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 520, height: 360), styleMask: [.titled, .closable], backing: .buffered, defer: false)
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
