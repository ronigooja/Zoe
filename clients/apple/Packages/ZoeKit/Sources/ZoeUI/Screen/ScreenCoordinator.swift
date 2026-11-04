#if os(macOS)
import AppKit
import SwiftUI
import ZoeKit

@MainActor
public final class ScreenCoordinator: ObservableObject {
    public let model: ZoeAppModel
    public let registry: SettingsRegistry
    public let surfaces: SurfaceManager
    public let silence = SilenceController()
    private var ticker: Task<Void, Never>?
    private var voiceWakeObserver: NSObjectProtocol?
    private var shortcutMonitor: Any?
    private var document: ScreenDocument?
    @Published public var textInputVisible = false
    @Published public var sendState: String?
    @Published public var aiChange: SettingAuditRecord?

    public init(model: ZoeAppModel) {
        self.model = model
        self.registry = SettingsRegistry()
        self.surfaces = SurfaceManager()
        registry.registerDefaults()
        if registry.value(for: "voice.enabled")?.boolValue == true { model.setContinuousVoiceEnabled(true) }
        voiceWakeObserver = NotificationCenter.default.addObserver(forName: .zoeWakeVoice, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in await self?.wakeVoice() }
        }
        surfaces.setActionHandler { [weak self] action in self?.perform(action) }
        registry.onAIChange = { [weak self] record in
            self?.aiChange = record
            self?.sendState = "Settings changed"
            if record.key == "voice.enabled", case let .bool(enabled) = record.newValue { self?.model.setContinuousVoiceEnabled(enabled) }
        }
        shortcutMonitor = NSEvent.addGlobalMonitorForEvents(matching: .keyDown) { [weak self] event in
            guard event.modifierFlags.contains([.command, .shift]), event.keyCode == 49 else { return }
            Task { @MainActor in await self?.wakeVoice() }
        }
    }

    deinit {
        ticker?.cancel()
        if let voiceWakeObserver { NotificationCenter.default.removeObserver(voiceWakeObserver) }
        if let shortcutMonitor { NSEvent.removeMonitor(shortcutMonitor) }
    }

    public func start() {
        ticker?.cancel()
        ticker = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(1))
                guard let self else { return }
                if self.silence.tick() == .timedOut { await self.model.endVoiceSession() }
            }
        }
    }

    public func openDefault() {
        let revision = Int64(model.snapshot?.eventCursor ?? "") ?? 0
        let document = DefaultScreenBuilder.build(tasks: model.tasks, approvals: model.approvals, notifications: model.notifications, revision: revision)
        self.document = document
        surfaces.open(SurfaceSpec(id: "main", kind: .window, document: document))
    }

    public func refreshDefault() { openDefault() }

    public func perform(_ action: ScreenAction) {
        Task { @MainActor [weak self] in
            guard let self else { return }
            let router = ScreenToolRouter(
                host: surfaces,
                settings: registry,
                startVoice: { _ = Task { @MainActor [weak self] in await self?.wakeVoice() } },
                endVoice: { _ = Task { @MainActor [weak self] in await self?.model.endVoiceSession() } },
                setSilenceTimeout: { [weak self] seconds in self?.silence.timeoutSeconds = seconds },
                showTextInput: { [weak self] in self?.showTextInput() },
                hideTextInput: { [weak self] in self?.hideTextInput() }
            )
            _ = await router.handle(tool: action.intent, args: action.args)
        }
    }

    public func showTextInput() { textInputVisible = true; silence.setInteractionInProgress(true) }
    public func hideTextInput() { textInputVisible = false; silence.setInteractionInProgress(false) }
    public func undoAIChange() {
        guard let aiChange else { return }
        if registry.undo(recordID: aiChange.id), aiChange.key == "voice.enabled", case let .bool(enabled) = aiChange.oldValue { model.setContinuousVoiceEnabled(enabled) }
        self.aiChange = nil
        sendState = nil
    }
    public func setDraft(_ text: String) { model.updateTaskDraft(text, for: model.selectedTaskID) }

    public func sendDraft() {
        guard sendState == nil || sendState == "Accepted" || sendState == "Failed" else { return }
        sendState = "Confirming"
        Task { @MainActor in
            switch await model.sendTaskDraft() {
            case .accepted: sendState = "Accepted"
            case .failed(let message): sendState = message
            case .unknown(_, let message): sendState = message
            }
        }
    }

    public func wakeVoice() async {
        silence.noteVoiceActivity()
        if model.voiceStatus.isActive { return }
        await model.startVoice()
    }

    private func voice(_ command: String) async {
        switch command {
        case "start": await wakeVoice()
        case "end": await model.endVoiceSession()
        default: break
        }
    }
}

extension VoiceIntegrationStatus {
    var isActive: Bool { if case .active = self { return true }; return false }
}

private extension JSONValue {
    var boolValue: Bool? { if case let .bool(value) = self { return value }; return nil }
}
#endif
