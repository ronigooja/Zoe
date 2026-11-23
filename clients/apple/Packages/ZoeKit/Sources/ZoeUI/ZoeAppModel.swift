import Combine
import CryptoKit
import ZoeKit
import Foundation
import WebRTC
import A2UISwiftCore
#if os(macOS)
import AppKit
#endif

@MainActor
public final class ZoeAppModel: ObservableObject {
    /// Returns true only once the native host has displayed the replacement surface.
    public var onUISurface: ((UISurfaceEvent) -> Bool)?
    public var isCurrentUIPresentation: ((UISurfaceEvent) -> Bool)?
    public var onUISurfacesClosed: (([String]) -> Void)?
    public var onUISurfacesReset: (() -> Void)?
    private var uiSurfaceRequests = UISurfaceRequests()
    // Physical render evidence survives failed recovery, not connection changes.
    // It is separate from the committed authoritative model and never reserves failures.
    private var recoveryRendered: [UISurfaceBinding: UISurfaceEvent] = [:]
    private var sessionPresentedSurfaces: [String: UISurfaceEvent] = [:]
    public var onUISurfaceInputState: ((UISurfaceEvent) -> Void)?
    /// Called only after the native host reports a successful display of the event. A thrown error is recorded as a diagnostic.
    public var onUISurfaceDisplayed: ((UISurfaceEvent, UISurfaceHistoryScope) async throws -> Void)?
    private var recoveryACKTask: Task<Void, Never>?
    #if os(macOS)
    public var screenVisibleSize: () -> NSSize? = { NSScreen.main?.visibleFrame.size }
    private var lastReportedScreenSize: SurfaceSize?
    #endif
    @Published public internal(set) var serverAddress: String
    @Published public private(set) var isConnecting = false
    @Published public private(set) var notice: String?
    @Published private(set) var savedKeyText = SavedKeyMask.text(for: nil)
    @Published public private(set) var isBusy = false
    @Published public private(set) var authentication: InstanceConnectionState = .checking
    @Published public private(set) var eventRecoveryRequired = false
    @Published public private(set) var authoritativeControlSnapshot: RecoveryControlSnapshot?
    @Published public private(set) var recoveredVoicePool: RecoveryVoice.Pool?
    @Published public private(set) var pendingConnectionImport: InstallationConnection?
    @Published public private(set) var connectionImportError: String?
    private let installationID: String
    private var connectionCleanupWarning: String?
    @Published public private(set) var connectionEpoch: UInt64 = 0
    @Published public private(set) var voiceConfig: VoiceConfig?
    @Published public private(set) var preferredVoice: String?
    @Published public private(set) var voicePoolStatus: VoicePoolStatus?
    @Published public private(set) var voiceStatus: VoiceIntegrationStatus = .idle
    @Published public private(set) var continuousVoiceEnabled: Bool
    @Published public private(set) var silenceHangupEnabled: Bool
    @Published public private(set) var silenceTimeoutSeconds: Double
    @Published public private(set) var popoverCloseDelaySeconds: Double
    @Published public private(set) var windowCloseDelaySeconds: Double
    @Published public private(set) var newUISurfaceBehavior: NewUISurfaceBehavior
    private let defaults: UserDefaults
    private let continuousVoiceKey = "zoe.voice.continuous"
    private var voiceReadinessRevision: UInt64 = 0
    private let pendingVoiceHangupKey = "zoe.voice.pendingGatewayHangups"
    private var api: APIClient?
    private var identity: (any InstanceKeyStoring)?
    private var journal: CommandJournal?
    private var coordinator: CommandCoordinator?
    private var eventTask: Task<Void, Never>?
    private var syncLoop: ClientSyncLoop?
    private var streamBackoff: UInt64 = 1
    private var connectionGeneration = ConnectionGeneration()
#if DEBUG
    var voiceReadinessOverrideForTesting: (() async throws -> (VoiceConfig, VoicePoolStatus))?
    func installVoiceConfigForTesting(_ config: VoiceConfig) { voiceConfig = config }
    var connectionAPIOverrideForTesting: ((URL, InstanceKey, String) throws -> APIClient)?
    var keyStoreOverrideForTesting: ((URL) throws -> any InstanceKeyStoring)?
    var suppressConnectionFollowupsForTesting = false
    var voiceDiagnosticObserverForTesting: ((String, String) -> Void)?
    var voiceCensusSummaryForTesting: String { voiceEventCensus.summary() }
    func reportVoiceCensusForTesting() { reportVoiceCensusIfNeeded() }
    func tickVoiceSilenceForTesting() async { await checkVoiceSilence() }
    var voiceLocalMediaStopObserverForTesting: (() -> Void)?
    var voiceStatusObserverForTesting: ((VoiceIntegrationStatus) -> Void)?
    var voiceHangupOverrideForTesting: ((String) async throws -> Void)?
    var voiceOfferOverrideForTesting: (() async throws -> String)?
    var voiceAnswerOverrideForTesting: ((String) async throws -> Void)?
    func handleEventForTesting(_ envelope: EventEnvelope, expectedConnectionEpoch: UInt64? = nil) async {
        await handle(.event(envelope), expectedGeneration: expectedConnectionEpoch ?? connectionGeneration.value, sourceAPI: nil)
    }
    var eventCursorForTesting: String? { eventCursor }
    var recoveryApplicationGateForTesting: (() async -> Void)?
    var recoveryCommitGateForTesting: (() async -> Void)?
    func recoverGlobalStateForTesting(api: APIClient) async throws {
        try await recoverGlobalState(using: api, generation: connectionGeneration.value)
    }
    func waitForRecoveryACKForTesting() async {
        await recoveryACKTask?.value
    }
    func installRecoveryCoordinatorForTesting(_ coordinator: CommandCoordinator) {
        self.coordinator = coordinator
    }
    func advanceConnectionForTesting() { recoveryACKTask?.cancel(); connectionGeneration.advance() }
    var consumedEventObserverForTesting: ((String) -> Void)?
    func receiveEventConnectionForTesting(api: APIClient) async throws {
        let generation = connectionGeneration.value
        try await receiveEventConnection(using: api, generation: generation) { [weak self] push in
            await self?.handle(push, expectedGeneration: generation, sourceAPI: nil)
        }
    }
    func setAuthenticationForTesting(_ state: InstanceConnectionState) {
        authentication = state
    }

    func setSnapshotForTesting(_ snapshot: Snapshot?) {
        instanceLineageID = snapshot?.authority.instanceLineageId
        eventCursor = snapshot?.eventCursor
    }

    @discardableResult
    func installVoiceSessionForTesting(_ response: VoiceDialResponse, api: APIClient, origin: URL = URL(string: "https://gateway.example")!) -> UInt64 {
        let session = GatewayCall(response: response, origin: origin)
        let epoch = voiceMediaFence.beginAttempt()
        resetVoiceActivity()
        voiceSession = session
        voiceSessionMediaEpoch = epoch
        voiceSessionMediaClosed = false
        voiceAPI = api
        voiceStatus = .active(sessionID: session.voiceSessionId, mediaGeneration: session.generation)
        noteVoiceConnected()
        return epoch
    }

    func beginVoiceAttemptForTesting() -> UInt64 {
        voiceStatus = .connecting
        return voiceMediaFence.beginAttempt()
    }

    func canContinueVoiceAttemptForTesting(_ mediaEpoch: UInt64) -> Bool {
        voiceMediaFence.accepts(mediaEpoch)
    }

    func handleVoiceMediaLossForTesting(_ loss: VoiceMediaLoss, mediaEpoch: UInt64) async {
        await handleVoiceMediaLoss(loss, mediaEpoch: mediaEpoch)
    }

    func wireVoiceTransportForTesting(_ transport: LibWebRTCTransport, mediaEpoch: UInt64) {
        wireVoiceTransport(transport, mediaEpoch: mediaEpoch)
    }

    func receiveVoiceDataForTesting(_ data: Data, generation: UInt64? = nil, mediaEpoch: UInt64? = nil) async {
        receiveVoiceData(data, generation: generation ?? connectionGeneration.value, mediaEpoch: mediaEpoch ?? voiceMediaEpoch)
        await Task.yield()
    }

    func configureVoiceCoordinatorForTesting(api: APIClient, journal: CommandJournal) {
        self.api = api
        coordinator = CommandCoordinator(api: api, journal: journal)
    }

    var hasPendingVoiceHangupForTesting: Bool { !pendingVoiceHangups().isEmpty }

    func retryPendingVoiceHangupForTesting(api: APIClient, origin: URL = URL(string: "https://gateway.example")!) async {
        await retryPendingVoiceHangups(api: api, origin: origin, generation: connectionGeneration.value, mediaEpoch: voiceMediaEpoch)
    }
#endif
    private var configuredServerURL: URL?
    private var voiceTransport: LibWebRTCTransport?
    private var voiceAudioSession: VoiceAudioSessionAdapter?
    private struct GatewayCall {
        let response: VoiceDialResponse
        let origin: URL

        var voiceSessionId: String { response.voiceSessionId }
        var generation: Int64 { response.generation }
        var handle: String { response.gatewaySessionId }
    }
    private var voiceSession: GatewayCall?
    private var voiceMediaFence = VoiceMediaEpochFence()
    private var voiceMediaEpoch: UInt64 { voiceMediaFence.current }
    private var voiceSessionMediaEpoch: UInt64?
    private var voiceSessionMediaClosed = false
    private var voiceStartMediaLoss: (epoch: UInt64, reason: String)?
    private var voiceStartInProgress = false
    private var voiceStartedAt: Date?
    private var diagnosticConnectedAt: Date?
    private var silenceResetDiagnostics = SilenceResetDiagnostics()
    private var voiceActivityCoalescer = VoiceActivityCoalescer()
    private var voiceActivityClassifier = VoiceActivityClassifier()
    private var negotiatingVoiceEpoch: UInt64?
    private var earlyVoiceEndEpoch: UInt64?
    private var voiceEventCensus = EventCensusDiagnostics()
    private var voiceInnerEventCensus = EventCensusDiagnostics(completeTypes: true)
    private var censusEndRecorded = false
    private var censusEarlyRecorded = false
    private var censusNextReportElapsed: TimeInterval = 60
    private final class StreamDiagnostics {
        var received: EventsReceivedDiagnostics
        init(now: Date) { received = EventsReceivedDiagnostics(now: now) }
        func report(api: APIClient, force: Bool = false) {
            if let summary = received.report(now: Date(), force: force) {
                api.recordDiagnostic(phase: "events_received", requestPath: "/v1/events", errorSummary: summary, level: .info)
            }
        }
    }
    private var voiceAttemptEndReason: VoiceEndReason?
    private var recordedAppStart = false
    private var voiceShutdownEpochs = Set<UInt64>()
    private var hangupInFlight = Set<PendingVoiceHangup>()
    private var voiceAPI: APIClient?
    private var recordedVoiceMediaLosses = Set<String>()
    private var voiceHangupRetryTasks: [PendingVoiceHangup: Task<Void, Never>] = [:]
    #if os(macOS)
    private var terminationObserver: NSObjectProtocol?
    #endif
    public init(defaults: UserDefaults = .standard, silenceClock: SilenceClock = SystemSilenceClock()) {
        self.defaults = defaults
        let savedEnabled = defaults.object(forKey: "zoe.voice.silenceHangupEnabled") == nil ? true : defaults.bool(forKey: "zoe.voice.silenceHangupEnabled")
        let savedTimeout = (defaults.object(forKey: "zoe.voice.silenceTimeoutSeconds") as? NSNumber)?.doubleValue ?? 60
        let timeout = savedTimeout.isFinite ? min(3600, max(10, savedTimeout)) : 60
        silenceHangupEnabled = savedEnabled
        silenceTimeoutSeconds = timeout
        silence = SilenceController(timeoutSeconds: timeout, isEnabled: savedEnabled, clock: silenceClock)
        // A legacy Zoe hangup has no gateway handle and cannot be replayed.
        defaults.removeObject(forKey: "zoe.voice.pendingHangup")
        popoverCloseDelaySeconds = SurfaceClosePolicy.validatedDelay((defaults.object(forKey: "zoe.ui.popoverCloseDelaySeconds") as? NSNumber)?.doubleValue ?? 60)
        windowCloseDelaySeconds = SurfaceClosePolicy.validatedDelay((defaults.object(forKey: "zoe.ui.windowCloseDelaySeconds") as? NSNumber)?.doubleValue ?? 60)
        newUISurfaceBehavior = defaults.string(forKey: "zoe.ui.newUISurfaceBehavior")
            .flatMap(NewUISurfaceBehavior.init(rawValue:)) ?? .addWindow
        serverAddress = defaults.string(forKey: "zoe.instance.serverAddress") ?? ""
        preferredVoice = nil
        let savedInstallation = defaults.string(forKey: "zoe.installationID")
        let identifier = savedInstallation.flatMap(UUID.init(uuidString:))?.uuidString.lowercased() ?? UUID().uuidString.lowercased()
        installationID = identifier
        defaults.set(identifier, forKey: "zoe.installationID")
        continuousVoiceEnabled = defaults.bool(forKey: continuousVoiceKey)
        loadVoicePreference()
        voiceMonitorTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(1))
                guard let self, !Task.isCancelled else { return }
                self.reportVoiceCensusIfNeeded()
                await self.checkVoiceSilence()
            }
        }
#if os(macOS)
        terminationObserver = NotificationCenter.default.addObserver(forName: NSApplication.willTerminateNotification, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in
                (self?.api ?? self?.voiceAPI)?.triggerDiagnosticUpload(timeout: 1)
                await self?.terminateVoiceForAppExit()
            }
        }
#endif
    }

    deinit {
        eventTask?.cancel()
        voiceMonitorTask?.cancel()
        for task in voiceHangupRetryTasks.values { task.cancel() }
        #if os(macOS)
        if let terminationObserver { NotificationCenter.default.removeObserver(terminationObserver) }
        #endif
        if let syncLoop { Task { await syncLoop.cancel() } }
    }
    public var canEndVoiceSession: Bool { voiceSession != nil }

    public func start() async {
        guard !eventRecoveryRequired else { return }
        guard !isConnecting, !isBusy else { return }
        guard !serverAddress.isEmpty else { authentication = .disconnected; return }
        isConnecting = true
        defer { isConnecting = false }
        notice = nil
        if !authentication.isConnected { authentication = .checking }
        do {
            if syncLoop == nil {
                let unauthenticatedAPI = try APIClient(serverURL: try ServerAddress.normalize(serverAddress), installationID: installationID)
                let loop = ClientSyncLoop(api: unauthenticatedAPI)
                syncLoop = loop
                await loop.start()
            }
            let configuredAPI = try configure()
            let generation = connectionGeneration.value
            try await validateConnection()
            guard generation == connectionGeneration.value else { return }
            authentication = .connected
            if let syncLoop { await syncLoop.cancel() }
            let syncLoop = ClientSyncLoop(api: configuredAPI)
            self.syncLoop = syncLoop
            await syncLoop.start()
            if !recordedAppStart {
                recordedAppStart = true
                if let policy = try? await configuredAPI.fetchClientPolicy() {
                    await configuredAPI.applyDiagnosticLevel(policy.logLevel)
                }
                await configuredAPI.recordDiagnosticAndWait(phase: "app_start", requestPath: "client", level: .info)
            }
            #if DEBUG
            if suppressConnectionFollowupsForTesting { return }
            #endif
            if let coordinator { _ = try? await coordinator.recoverPending(currentInstanceLineageID: instanceLineageID, onOutcome: { _ in }) }
            guard generation == connectionGeneration.value else { return }
            startEventLoop()
            if continuousVoiceEnabled, voiceSession == nil {
                Task { @MainActor [weak self] in await self?.startVoiceIfEnabled(trigger: "app_start") }
            }
        } catch {
            if Self.isAccessDenied(error) || (error as? ZoeError).map({ if case .missingCredential = $0 { true } else { false } }) == true {
                lockInstance()
                authentication = .disconnected
            } else if !authentication.isConnected { authentication = .unavailable }
            setOffline(error)
        }
    }

    public func reconnect() async { await start() }

    public func setContinuousVoiceEnabled(_ enabled: Bool) {
        guard continuousVoiceEnabled != enabled else { return }
        continuousVoiceEnabled = enabled
        defaults.set(enabled, forKey: continuousVoiceKey)
        if enabled, authentication.isConnected {
            Task {
                if voiceSessionMediaClosed { await endVoiceSession(reason: .userToggleOff) }
                await startVoiceIfEnabled(trigger: "menu_toggle")
            }
        } else if !enabled {
            Task { await endVoiceSession(reason: .userToggleOff) }
        }
    }

    private func startVoiceIfEnabled(trigger: String = "auto_resume") async {
        guard continuousVoiceEnabled, authentication.isConnected, voiceSession == nil else { return }
        await startVoice(voice: nil, voiceMode: nil, languageCode: nil, trigger: trigger)
    }

    public func setSilenceHangupEnabled(_ enabled: Bool) {
        let before = silenceSettings
        silenceHangupEnabled = enabled
        defaults.set(enabled, forKey: "zoe.voice.silenceHangupEnabled")
        silence.isEnabled = enabled
        recordSilenceSettingChange(from: before)
    }

    public func setSilenceTimeoutSeconds(_ seconds: Double) {
        guard seconds.isFinite else { return }
        let before = silenceSettings
        silenceTimeoutSeconds = min(3600, max(10, seconds))
        defaults.set(silenceTimeoutSeconds, forKey: "zoe.voice.silenceTimeoutSeconds")
        silence.timeoutSeconds = silenceTimeoutSeconds
        recordSilenceSettingChange(from: before)
    }

    public func setNewUISurfaceBehavior(_ behavior: NewUISurfaceBehavior) {
        newUISurfaceBehavior = behavior
        defaults.set(behavior.rawValue, forKey: "zoe.ui.newUISurfaceBehavior")
    }

    public func setPopoverCloseDelaySeconds(_ seconds: Double) {
        guard seconds.isFinite else { return }
        popoverCloseDelaySeconds = SurfaceClosePolicy.validatedDelay(seconds)
        defaults.set(popoverCloseDelaySeconds, forKey: "zoe.ui.popoverCloseDelaySeconds")
    }

    public func setWindowCloseDelaySeconds(_ seconds: Double) {
        guard seconds.isFinite else { return }
        windowCloseDelaySeconds = SurfaceClosePolicy.validatedDelay(seconds)
        defaults.set(windowCloseDelaySeconds, forKey: "zoe.ui.windowCloseDelaySeconds")
    }

    private var silenceSettings: String { DiagnosticSummaries.settings(enabled: silenceHangupEnabled, seconds: silenceTimeoutSeconds) }
    private var silenceSettingsSuffix: String { DiagnosticSummaries.settingsSuffix(enabled: silenceHangupEnabled, seconds: silenceTimeoutSeconds) }
    private var diagnosticElapsed: TimeInterval { diagnosticConnectedAt.map { silence.diagnosticNow.timeIntervalSince($0) } ?? 0 }

    private func recordSilenceSettingChange(from before: String) {
        guard before != silenceSettings else { return }
        (voiceAPI ?? api)?.recordDiagnostic(phase: "silence_setting_changed", requestPath: "client", errorSummary: "\(before)->\(silenceSettings)", level: .info, voiceSessionID: voiceSession?.voiceSessionId, voiceGeneration: voiceSession?.generation, voiceMediaEpoch: voiceSessionMediaEpoch)
    }

    private func recordVoiceTrace(phase: String, summary: String, upload: Bool = false) {
        #if DEBUG
        voiceDiagnosticObserverForTesting?(phase, summary)
        #endif
        guard let api = voiceAPI ?? api else { return }
        let session = voiceSession
        let epoch = voiceSessionMediaEpoch ?? voiceMediaEpoch
        Task {
            await api.recordDiagnosticAndWait(phase: phase, requestPath: "client", errorSummary: String(summary.prefix(200)), level: .info, voiceSessionID: session?.voiceSessionId, voiceGeneration: session?.generation, voiceMediaEpoch: epoch)
            if upload { api.triggerDiagnosticUpload() }
        }
    }

    private func recordSilenceReset(type: String) {
        if let summary = silenceResetDiagnostics.note(type: type, now: silence.diagnosticNow, elapsed: diagnosticElapsed) {
            recordVoiceTrace(phase: "silence_reset", summary: summary)
        }
    }

    private func checkVoiceSilence() async {
        guard let session = voiceSession, let api = voiceAPI,
              let epoch = voiceSessionMediaEpoch, epoch == voiceMediaEpoch, !voiceSessionMediaClosed,
              silence.tick() == .timedOut else { return }
        let summary = DiagnosticSummaries.timeout(age: silence.lastActivityAge, timeout: silence.timeoutSeconds, enabled: silence.isEnabled, interaction: silence.interactionInProgress, elapsed: diagnosticElapsed)
        recordVoiceTrace(phase: "silence_tick_timeout", summary: summary, upload: true)
        await finishVoiceSession(session, api: api, mediaEpoch: epoch, lossReason: nil, reason: .silenceTimeout)
    }

    private func reportVoiceCensusIfNeeded() {
        guard voiceSession != nil else { return }
        let elapsed = diagnosticElapsed
        let early = !censusEarlyRecorded && elapsed >= 20
        let periodic = elapsed >= censusNextReportElapsed
        guard early || periodic else { return }
        if early {
            censusEarlyRecorded = true
            recordVoiceTrace(phase: "voice_event_census_early", summary: voiceEventCensus.summary())
        }
        if periodic { censusNextReportElapsed = (floor(elapsed / 60) + 1) * 60 }
        for report in voiceInnerEventCensus.typeReports() {
            recordVoiceTrace(phase: "voice_event_types", summary: report)
        }
    }

    private func resetVoiceActivity() {
        silence.reset()
        silenceResetDiagnostics = SilenceResetDiagnostics()
        voiceActivityCoalescer = VoiceActivityCoalescer()
        voiceActivityClassifier = VoiceActivityClassifier()
        voiceEventCensus = EventCensusDiagnostics()
        voiceInnerEventCensus = EventCensusDiagnostics(completeTypes: true)
    }

    private func noteVoiceConnected() {
        // Start the window here unless the live data channel already delivered activity.
        voiceStartedAt = Date()
        diagnosticConnectedAt = silence.diagnosticNow
        censusEndRecorded = false
        censusEarlyRecorded = false
        censusNextReportElapsed = 60
        if silence.lastActivityAge == nil {
            silence.noteVoiceActivity()
            recordSilenceReset(type: "connect")
        }
    }

    public func receiveInstallationLink(_ url: URL) {
        do {
            pendingConnectionImport = try InstallationConnection(link: url)
            connectionImportError = nil
        } catch {
            pendingConnectionImport = nil
            connectionImportError = "This installation link is invalid."
        }
    }

    public func dismissConnectionImport() { pendingConnectionImport = nil }
    public func dismissConnectionImportError() { connectionImportError = nil }

    /// Only explicit camera/image import actions use this path. External links remain staged for review.
    @discardableResult
    public func connectImportedConnection(_ connection: InstallationConnection) async -> Bool {
        await connect(serverAddress: connection.serverURL.absoluteString, key: (try? connection.key.bearerToken()) ?? "")
    }

    /// Validates a candidate before replacing the active connection. Imported keys stay in memory until confirmed.
    @discardableResult
    public func connect(serverAddress address: String, key: String) async -> Bool {
        guard !isBusy, !isConnecting else { return false }
        isBusy = true
        defer { isBusy = false }
        defer { refreshSavedKeyText() }
        do {
            let candidate = try InstallationConnection(serverAddress: address, key: key)
            if !serverAddress.isEmpty, candidate.serverURL != validServerURL(serverAddress) {
                notice = "Clear the saved key before connecting to a different server."
                return false
            }
            let candidateAPI = try connectionAPI(url: candidate.serverURL, key: candidate.key)
            _ = try await candidateAPI.snapshot()
            let store = try keyStore(url: candidate.serverURL)
            let previousKey = try store.load()
            // Local credential replacement is atomic. A failed write leaves the old connection intact.
            try store.replace(candidate.key)
            do {
                guard try store.load() == candidate.key else {
                    throw ZoeError.persistence("The instance key could not be saved in local storage.")
                }
                await endVoiceSession(reason: .connectionCleared)
                // Keep instance-scoped pending commands across key rotation.
                try await removeLocalConnection(preservingCredential: true)
            } catch {
                do {
                    if let previousKey { try store.replace(previousKey) }
                    else { try store.reset() }
                } catch {
                    throw ZoeError.persistence("The previous instance key could not be restored in local storage. Reconnect with your server key.")
                }
                throw error
            }
            serverAddress = candidate.serverURL.absoluteString
            loadVoicePreference()
            defaults.set(serverAddress, forKey: "zoe.instance.serverAddress")
            pendingConnectionImport = nil
            isBusy = false
            await start()
            if let connectionCleanupWarning { notice = connectionCleanupWarning }
            return authentication.isConnected
        } catch {
            notice = userMessage(error)
            return false
        }
    }

    public func clearCredential() async {
        guard !isBusy, !isConnecting else { return }
        isBusy = true
        defer { isBusy = false }
        await endVoiceSession(reason: .connectionCleared)
        defer { refreshSavedKeyText() }
        do {
            try await removeLocalConnection()
            notice = connectionCleanupWarning ?? "Saved key and local connection data cleared."
        } catch { notice = userMessage(error) }
    }

    /// Publishes only the masked text. The decrypted key stays inside this function and is never stored or logged.
    func refreshSavedKeyText() {
        var key: String?
        if let url = try? ServerAddress.normalize(serverAddress), let store = try? keyStore(url: url) {
            key = try? store.load()?.bearerToken()
        }
        savedKeyText = SavedKeyMask.text(for: key)
    }

    private func removeLocalConnection(preservingCredential: Bool = false) async throws {
        connectionCleanupWarning = nil
        let oldJournal = journal
        lockInstance()
        authentication = .disconnected
        if !preservingCredential { try await oldJournal?.discard() }
        if let url = try? ServerAddress.normalize(serverAddress) {
            let store = try keyStore(url: url)
            if !preservingCredential { try store.reset() }
        }
        configuredServerURL = nil
        serverAddress = ""
        preferredVoice = nil
        defaults.removeObject(forKey: "zoe.instance.serverAddress")
    }

    private func lockInstance() {
        recoveryACKTask?.cancel()
        closeUISurfaces()
        connectionGeneration.advance()
        connectionEpoch = connectionGeneration.value
        eventTask?.cancel()
        eventTask = nil
        if let syncLoop { Task { await syncLoop.cancel() } }
        syncLoop = nil
        instanceLineageID = nil
        eventCursor = nil
        authoritativeControlSnapshot = nil
        recoveredVoicePool = nil
        eventRecoveryRequired = false
        voiceConfig = nil
        voicePoolStatus = nil
        api = nil
        identity = nil
        journal = nil
        coordinator = nil
        // Capture and detach old resources synchronously. Later cleanup cannot target a new session.
        let oldSession = voiceSession
        let oldAPI = voiceAPI
        let oldEpoch = voiceSessionMediaEpoch
        let oldDuration = voiceStartedAt.map { Date().timeIntervalSince($0) } ?? 0
        silence.reset()
        negotiatingVoiceEpoch = nil
        _ = voiceMediaFence.stop(voiceMediaEpoch)
        let oldTransport = voiceTransport
        let oldAudio = voiceAudioSession
        oldTransport?.onDataMessage = nil
        oldTransport?.onConnectionState = nil
        oldTransport?.onDataChannelState = nil
        oldAudio?.onLifecycleEvent = nil
        voiceTransport = nil
        voiceAudioSession = nil
        voiceSession = nil
        voiceSessionMediaEpoch = nil
        voiceSessionMediaClosed = false
        voiceAPI = nil
        voiceStatus = .stopped
        try? oldAudio?.deactivate()
        if let oldSession, let oldAPI {
            deliverVoiceEnd(oldSession, api: oldAPI, mediaEpoch: oldEpoch ?? 0, reason: .connectionCleared, duration: oldDuration)
        }
        oldTransport?.closeImmediately()
        Task { [weak self] in
            guard let self, let oldSession, let oldAPI else { return }
            _ = await self.requestVoiceHangup(oldSession, api: oldAPI, phase: "instance_lock")
        }
    }
    public func refreshVoiceReadiness() async {
        guard let api = try? configure() else { return }
        let generation = connectionGeneration.value
        let mediaEpoch = voiceMediaEpoch
        voiceReadinessRevision &+= 1
        let revision = voiceReadinessRevision
        do {
            let (nextConfig, nextPool) = try await fetchVoiceReadiness(api: api)
            guard !Task.isCancelled, generation == connectionGeneration.value,
                  api === self.api, mediaEpoch == voiceMediaEpoch,
                  revision == voiceReadinessRevision else { return }
            voiceConfig = nextConfig
            voicePoolStatus = nextPool
        } catch {
            // Settings readiness is advisory, not a media lifecycle observation.
            // Retain the last cache and actual call state on failure/cancellation.
        }
    }

    private func fetchVoiceReadiness(api: APIClient) async throws -> (VoiceConfig, VoicePoolStatus) {
        #if DEBUG
        if let override = voiceReadinessOverrideForTesting { return try await override() }
        #endif
        async let config = api.voiceConfig()
        async let pool = api.voicePoolStatus()
        return try await (config, pool)
    }

    public var effectiveVoice: String? { voiceConfig.map { $0.resolvedVoice(preferred: preferredVoice) } }

    public func setPreferredVoice(_ voice: String?) {
        guard voice == nil || voiceConfig?.voices.contains(where: { $0.id == voice }) == true,
              let key = voicePreferenceKey else { return }
        preferredVoice = voice
        if let voice { defaults.set(voice, forKey: key) }
        else { defaults.removeObject(forKey: key) }
    }

    private var voicePreferenceKey: String? {
        guard let url = try? ServerAddress.normalize(serverAddress) else { return nil }
        return "zoe.voice.preferred." + url.absoluteString
    }

    private func loadVoicePreference() {
        preferredVoice = voicePreferenceKey.flatMap { defaults.string(forKey: $0) }
    }

    public func startVoice(voice: String? = nil, voiceMode: String? = nil, languageCode: String? = nil) async {
        await startVoice(voice: voice, voiceMode: voiceMode, languageCode: languageCode, trigger: "menu_toggle")
    }

    private func startVoice(voice: String?, voiceMode: String?, languageCode: String?, trigger: String) async {
        guard voiceSession == nil, !voiceStartInProgress else { return }
        let api: APIClient
        do { api = try configure() }
        catch {
            voiceStatus = .unavailable(reason: uiText("Voice is unavailable right now."))
            return
        }
        voiceStartInProgress = true
        voiceAttemptEndReason = nil
        resetVoiceActivity()
        censusEndRecorded = false
        censusEarlyRecorded = false
        censusNextReportElapsed = 60
        diagnosticConnectedAt = nil
        voiceStartedAt = Date()
        defer { voiceStartInProgress = false }
        let mediaEpoch = voiceMediaFence.beginAttempt()
        earlyVoiceEndEpoch = nil
        voiceStartMediaLoss = nil
        let generation = connectionGeneration.value
        voiceAPI = api

        var allocatedSession: GatewayCall?
        var diagnosticPhase = "config_request"
        voiceStatus = .connecting
        do {
            let config = try await api.voiceConfig()
            try requireVoiceAttempt(generation: generation, mediaEpoch: mediaEpoch)
            guard let origin = config.gatewayOrigin else { throw ZoeError.invalidResponse }
            let pool = try? await api.voicePoolStatus()
            try requireVoiceAttempt(generation: generation, mediaEpoch: mediaEpoch)
            voiceConfig = config
            voicePoolStatus = pool
            if await retryPendingVoiceHangups(api: api, origin: origin, generation: generation, mediaEpoch: mediaEpoch) {
                try requireVoiceAttempt(generation: generation, mediaEpoch: mediaEpoch)
                let refreshedPool = try? await api.voicePoolStatus()
                try requireVoiceAttempt(generation: generation, mediaEpoch: mediaEpoch)
                voicePoolStatus = refreshedPool ?? pool
            }
            try requireVoiceAttempt(generation: generation, mediaEpoch: mediaEpoch)
            // Zoe's pool status is a mirror; the gateway decides admission.
            diagnosticPhase = "webrtc_offer"
            let offer = try await createVoiceOffer(config: config, api: api, generation: generation, mediaEpoch: mediaEpoch)
            try requireVoiceAttempt(generation: generation, mediaEpoch: mediaEpoch)
            diagnosticPhase = "gateway_dial"
            api.recordDiagnostic(phase: "voice_start", requestPath: Self.gatewayDialPath, errorSummary: trigger + silenceSettingsSuffix, level: .info, voiceMediaEpoch: mediaEpoch)
            let payload = VoiceDialRequest(
                requestID: UUID().uuidString.lowercased(),
                offerSDP: offer,
                voice: config.resolvedVoice(preferred: voice ?? preferredVoice),
                voiceMode: voiceMode.flatMap { $0.isEmpty ? nil : $0 } ?? config.defaults.voiceMode,
                languageCode: languageCode.flatMap { $0.isEmpty ? nil : $0 } ?? config.defaults.languageCode
            )
            let response = try await api.dialVoiceCall(origin: origin, payload: payload)
            let created = GatewayCall(response: response, origin: origin)
            allocatedSession = created
            try requireVoiceAttempt(generation: generation, mediaEpoch: mediaEpoch)
            diagnosticPhase = "webrtc_answer"
            negotiatingVoiceEpoch = mediaEpoch
            try await connectVoiceMedia(answer: response.answerSdp, generation: generation, mediaEpoch: mediaEpoch)
            try requireVoiceAttempt(generation: generation, mediaEpoch: mediaEpoch)

            negotiatingVoiceEpoch = nil
            voiceSession = created
            voiceSessionMediaEpoch = mediaEpoch
            voiceSessionMediaClosed = false
            voiceAPI = api
            voiceStatus = .active(sessionID: created.voiceSessionId, mediaGeneration: created.generation)
            noteVoiceConnected()
            api.recordDiagnostic(phase: "voice_connected", requestPath: "client", level: .info, voiceSessionID: created.voiceSessionId, voiceGeneration: created.generation, voiceMediaEpoch: mediaEpoch)
            #if os(macOS)
            await reportScreenSize()
            #endif
        } catch {
            if earlyVoiceEndEpoch == mediaEpoch, generation == connectionGeneration.value,
               voiceMediaEpoch == (mediaEpoch &+ 1), let allocatedSession {
                deliverVoiceEnd(allocatedSession, api: api, mediaEpoch: mediaEpoch, reason: .voiceEndEvent)
                completeLocalVoiceEnd(lossReason: nil)
                refreshPoolAfterVoiceEnd(api: api)
                return
            }
            let redacted = RedactedDiagnostic(error)
            api.recordDiagnostic(phase: diagnosticPhase, requestPath: diagnosticPhase == "gateway_dial" ? Self.gatewayDialPath : "client", httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: redacted.errorSummary)
            let attemptWasCurrent = mediaEpoch == voiceMediaEpoch
            closeVoiceLocalMedia(mediaEpoch: mediaEpoch)
            let closedEpoch = mediaEpoch &+ 1
            let canPublishEnd = generation == connectionGeneration.value && closedEpoch == voiceMediaEpoch &&
                (attemptWasCurrent || voiceStartMediaLoss?.epoch == mediaEpoch)
            var hangupUnconfirmed = false
            if let allocatedSession {
                if canPublishEnd {
                    deliverVoiceEnd(allocatedSession, api: api, mediaEpoch: mediaEpoch, reason: voiceAttemptEndReason ?? .setupFailed)
                }
                if earlyVoiceEndEpoch != mediaEpoch {
                    hangupUnconfirmed = !(await requestVoiceHangup(allocatedSession, api: api, phase: "setup_hangup", epoch: mediaEpoch))
                }
            } else if canPublishEnd {
                let reason = voiceAttemptEndReason ?? .setupFailed
                let summary = reason.summary(duration: voiceStartedAt.map { Date().timeIntervalSince($0) } ?? 0) + silenceSettingsSuffix
                let census = voiceEventCensus.summary()
                let typeReports = voiceInnerEventCensus.typeReports()
                censusEndRecorded = true
                Task {
                    await api.recordDiagnosticAndWait(phase: "voice_event_census", requestPath: "client", errorSummary: census, level: .info, voiceMediaEpoch: mediaEpoch)
                    for report in typeReports {
                        await api.recordDiagnosticAndWait(phase: "voice_event_types", requestPath: "client", errorSummary: report, level: .info, voiceMediaEpoch: mediaEpoch)
                    }
                    await api.recordDiagnosticAndWait(phase: "voice_end", requestPath: "client", errorType: reason.isError ? "voice_lifecycle" : "", errorSummary: summary, level: reason.isError ? .error : .info, voiceMediaEpoch: mediaEpoch)
                    api.triggerDiagnosticUpload()
                }
            }
            let mediaLossReason = voiceStartMediaLoss?.epoch == mediaEpoch ? voiceStartMediaLoss?.reason : nil
            if canPublishEnd, generation == connectionGeneration.value, closedEpoch == voiceMediaEpoch {
                if hangupUnconfirmed, let allocatedSession {
                    voiceSession = allocatedSession
                    voiceSessionMediaEpoch = mediaEpoch
                    voiceSessionMediaClosed = true
                    voiceAPI = api
                    voiceStatus = .interrupted(reason: uiText("Local media is closed, but gateway hangup is unconfirmed. Retry hangup before starting another call."))
                } else {
                    voiceSession = nil
                    voiceSessionMediaEpoch = nil
                    voiceSessionMediaClosed = false
                    voiceAPI = nil
                    if let mediaLossReason {
                        voiceStatus = .interrupted(reason: uiFormat("%@ Local media is closed. The setup attempt was stopped; this does not prove upstream termination or cancel a task.", mediaLossReason))
                    } else {
                        voiceStatus = .unavailable(reason: Self.voiceDialFailureMessage(error))
                    }
                }
                refreshPoolAfterVoiceEnd(api: api)
            }
        }
    }

    private func createVoiceOffer(config: VoiceConfig, api: APIClient, generation: UInt64, mediaEpoch: UInt64) async throws -> String {
        #if DEBUG
        if let voiceOfferOverrideForTesting { return try await voiceOfferOverrideForTesting() }
        #endif
        let audio = AppleDesktopAudioSessionAdapter()
        voiceAudioSession = audio
        audio.onLifecycleEvent = { [weak self] event in
            api.recordDiagnostic(phase: "audio_activate", requestPath: "/v1/voice/audio", errorType: "audio_lifecycle", errorSummary: String(String(describing: event).prefix(64)))
            Task { @MainActor in
                await self?.handleVoiceMediaLoss(.audio(event), mediaEpoch: mediaEpoch)
            }
        }
        try audio.activate()
        let transport = try LibWebRTCTransport(dataChannelLabel: config.webRTC.dataChannel.label)
        voiceTransport = transport
        wireVoiceTransport(transport, generation: generation, mediaEpoch: mediaEpoch)
        func recordGathering() {
            let report = transport.iceGatheringReport
            api.recordDiagnostic(phase: "ice_gathering", requestPath: "client", errorType: report.timedOut ? "ice_gathering_timeout" : "", errorSummary: report.summary, level: report.timedOut ? .error : .info, voiceMediaEpoch: mediaEpoch)
        }
        do {
            let offer = try await transport.createOffer()
            recordGathering()
            return offer
        } catch {
            recordGathering()
            throw error
        }
    }

    private func connectVoiceMedia(answer: String, generation: UInt64, mediaEpoch: UInt64) async throws {
        #if DEBUG
        if let voiceAnswerOverrideForTesting { return try await voiceAnswerOverrideForTesting(answer) }
        #endif
        guard let transport = voiceTransport else { throw ZoeError.invalidResponse }
        try await transport.applyAnswer(answer)
        try requireVoiceAttempt(generation: generation, mediaEpoch: mediaEpoch)
        try await transport.waitUntilConnected()
    }

    private func requireVoiceAttempt(generation: UInt64, mediaEpoch: UInt64) throws {
        try Task.checkCancellation()
        guard generation == connectionGeneration.value, voiceMediaFence.accepts(mediaEpoch) else { throw CancellationError() }
    }

    public func endVoiceSession() async {
        await endVoiceSession(reason: .other)
    }

    private func endVoiceSession(reason: VoiceEndReason) async {
        guard let session = voiceSession, let api = voiceAPI, let mediaEpoch = voiceSessionMediaEpoch else {
            if voiceStartInProgress {
                if voiceStartMediaLoss == nil {
                    voiceAttemptEndReason = reason
                    voiceStartMediaLoss = (voiceMediaEpoch, uiText("Voice setup was stopped locally."))
                }
            }
            let closingEpoch = voiceMediaEpoch
            closeVoiceLocalMedia(mediaEpoch: closingEpoch)
            voiceSession = nil
            voiceSessionMediaEpoch = nil
            voiceSessionMediaClosed = false
            voiceAPI = nil
            if case .unavailable = voiceStatus {} else { voiceStatus = .stopped }
            return
        }
        await finishVoiceSession(session, api: api, mediaEpoch: mediaEpoch, lossReason: nil, reason: reason)
    }

    private static let gatewayDialPath = "/client/v1/voice/calls"
    private static let gatewayHangupPath = "/client/v1/voice/calls/{gateway_session_id}/hangup"

    private func deliverVoiceEnd(_ session: GatewayCall, api: APIClient, mediaEpoch: UInt64, reason: VoiceEndReason, duration: TimeInterval? = nil) {
        let summary = reason.summary(duration: duration ?? voiceStartedAt.map { Date().timeIntervalSince($0) } ?? 0) + silenceSettingsSuffix
        let census = censusEndRecorded ? nil : voiceEventCensus.summary()
        let typeReports = censusEndRecorded ? [] : voiceInnerEventCensus.typeReports()
        censusEndRecorded = true
        // Independent best-effort delivery never gates hangup, even if URLSession ignores cancellation.
        Task {
            if let census {
                await api.recordDiagnosticAndWait(phase: "voice_event_census", requestPath: "client", errorSummary: census, level: .info, voiceSessionID: session.voiceSessionId, voiceGeneration: session.generation, voiceMediaEpoch: mediaEpoch)
            }
            for report in typeReports {
                await api.recordDiagnosticAndWait(phase: "voice_event_types", requestPath: "client", errorSummary: report, level: .info, voiceSessionID: session.voiceSessionId, voiceGeneration: session.generation, voiceMediaEpoch: mediaEpoch)
            }
            await api.recordDiagnosticAndWait(phase: "voice_end", requestPath: "client", errorType: reason.isError ? "voice_lifecycle" : "", errorSummary: summary, level: reason.isError ? .error : .info, voiceSessionID: session.voiceSessionId, voiceGeneration: session.generation, voiceMediaEpoch: mediaEpoch)
            api.triggerDiagnosticUpload()
        }
    }

    private func finishVoiceSession(_ session: GatewayCall, api: APIClient, mediaEpoch: UInt64, lossReason: String?, reason: VoiceEndReason, gatewayIsEnding: Bool = false) async {
        guard voiceSession?.handle == session.handle,
              voiceSessionMediaEpoch == mediaEpoch,
              voiceShutdownEpochs.insert(mediaEpoch).inserted else { return }
        defer { voiceShutdownEpochs.remove(mediaEpoch) }
        let duration = voiceStartedAt.map { Date().timeIntervalSince($0) } ?? 0
        if let lossReason {
            voiceStatus = .interrupted(reason: uiFormat("%@ Closing local media and requesting gateway hangup.", lossReason))
        }
        if !voiceSessionMediaClosed {
            guard voiceMediaEpoch == mediaEpoch else { return }
            closeVoiceLocalMedia(mediaEpoch: mediaEpoch)
            guard voiceSession?.handle == session.handle,
                  voiceSessionMediaEpoch == mediaEpoch else { return }
            voiceSessionMediaClosed = true
        }
        deliverVoiceEnd(session, api: api, mediaEpoch: mediaEpoch, reason: reason, duration: duration)
        if gatewayIsEnding {
            // The remote end signal needs no client hangup and is not proof of release.
            completeLocalVoiceEnd(lossReason: nil)
            refreshPoolAfterVoiceEnd(api: api)
            return
        }
        let confirmed = await requestVoiceHangup(session, api: api, phase: "hangup", epoch: mediaEpoch, reconnect: lossReason != nil)
        guard voiceSession?.handle == session.handle, voiceSessionMediaEpoch == mediaEpoch,
              voiceMediaEpoch == (mediaEpoch &+ 1) else { return }
        if confirmed {
            completeLocalVoiceEnd(lossReason: lossReason)
        } else {
            voiceStatus = .interrupted(reason: uiText("Local media is closed, but gateway hangup is unconfirmed. Retry hangup before starting another call."))
        }
        refreshPoolAfterVoiceEnd(api: api)
    }

    private func completeLocalVoiceEnd(lossReason: String?) {
        voiceSession = nil
        voiceSessionMediaEpoch = nil
        voiceSessionMediaClosed = false
        voiceAPI = nil
        voiceStatus = lossReason.map { .interrupted(reason: uiFormat("%@ Local media is closed. Ending a call does not cancel a task.", $0)) } ?? .idle
    }

    private struct PendingVoiceHangup: Codable, Hashable {
        let serverURL: String
        let originDigest: Data
        let handle: String
    }

    private static func originDigest(_ origin: URL) -> Data {
        Data(SHA256.hash(data: Data(origin.absoluteString.utf8)))
    }

    private func pendingVoiceHangups() -> Set<PendingVoiceHangup> {
        guard let data = defaults.data(forKey: pendingVoiceHangupKey) else { return [] }
        return (try? JSONDecoder().decode(Set<PendingVoiceHangup>.self, from: data)) ?? []
    }

    private func updatePendingHangup(_ pending: PendingVoiceHangup, adding: Bool) {
        var rows = pendingVoiceHangups()
        if adding { rows.insert(pending) } else { rows.remove(pending) }
        if rows.isEmpty { defaults.removeObject(forKey: pendingVoiceHangupKey) }
        else if let data = try? JSONEncoder().encode(rows) { defaults.set(data, forKey: pendingVoiceHangupKey) }
    }

    private func requestVoiceHangup(_ call: GatewayCall, api: APIClient, phase: String, epoch: UInt64? = nil, reconnect: Bool = false) async -> Bool {
        let pending = PendingVoiceHangup(serverURL: api.serverURL.absoluteString, originDigest: Self.originDigest(call.origin), handle: call.handle)
        updatePendingHangup(pending, adding: true)
        let generation = connectionGeneration.value
        let result = await attemptVoiceHangup(pending, origin: call.origin, api: api, phase: phase)
        if result == .retryable {
            scheduleVoiceHangupRetry(pending, origin: call.origin, api: api, phase: phase, generation: generation, epoch: epoch, reconnect: reconnect)
        }
        return result == .confirmed
    }

    private enum HangupResult { case confirmed, retryable, unconfirmed, inFlight }

    private func attemptVoiceHangup(_ pending: PendingVoiceHangup, origin: URL, api: APIClient, phase: String) async -> HangupResult {
        guard hangupInFlight.insert(pending).inserted else { return .inFlight }
        defer { hangupInFlight.remove(pending) }
        do {
            #if DEBUG
            if let voiceHangupOverrideForTesting { try await voiceHangupOverrideForTesting(pending.handle) }
            else { try await api.hangupVoiceCall(origin: origin, gatewaySessionID: pending.handle) }
            #else
            try await api.hangupVoiceCall(origin: origin, gatewaySessionID: pending.handle)
            #endif
            updatePendingHangup(pending, adding: false)
            return .confirmed
        } catch {
            let redacted = RedactedDiagnostic(error)
            api.recordDiagnostic(phase: "voice_\(phase)", requestPath: Self.gatewayHangupPath, httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: redacted.errorSummary)
            return Self.isRetryableHangup(error) ? .retryable : .unconfirmed
        }
    }

    private static func isRetryableHangup(_ error: Error) -> Bool {
        if let error = error as? ZoeError, case let .http(status, _, _, _) = error { return (500..<600).contains(status) }
        if let error = error as? URLError { return error.code == .cancelled || APIClient.isTransientVoiceTransportFailure(error) }
        return error is CancellationError
    }

    private func scheduleVoiceHangupRetry(_ pending: PendingVoiceHangup, origin: URL, api: APIClient, phase: String, generation: UInt64, epoch: UInt64?, reconnect: Bool = false) {
        guard voiceHangupRetryTasks[pending] == nil else { return }
        voiceHangupRetryTasks[pending] = Task { [weak self] in
            for delay in [1, 2, 4] {
                try? await Task.sleep(for: .seconds(delay))
                guard let self, !Task.isCancelled else { return }
                guard self.pendingVoiceHangups().contains(pending) else { break }
                let result = await self.attemptVoiceHangup(pending, origin: origin, api: api, phase: "\(phase)_retry")
                if result == .confirmed {
                    if generation == self.connectionGeneration.value,
                       self.voiceAPI === api, self.voiceSessionMediaClosed,
                       let epoch, self.voiceSessionMediaEpoch == epoch,
                       self.voiceMediaEpoch == (epoch &+ 1),
                       self.voiceSession?.handle == pending.handle, self.voiceSession?.origin == origin {
                        self.completeLocalVoiceEnd(lossReason: nil)
                        self.refreshPoolAfterVoiceEnd(api: api)
                        if reconnect { await self.startVoiceIfEnabled() }
                    }
                    break
                }
                if result == .unconfirmed { break }
            }
            if !Task.isCancelled { self?.voiceHangupRetryTasks[pending] = nil }
        }
    }

    @discardableResult
    private func retryPendingVoiceHangups(api: APIClient, origin: URL, generation: UInt64, mediaEpoch: UInt64) async -> Bool {
        // Persisted handles are not authority to send a key to an origin. Zoe must re-advertise it.
        let digest = Self.originDigest(origin)
        var attempted = false
        for pending in pendingVoiceHangups() where pending.serverURL == api.serverURL.absoluteString && pending.originDigest == digest {
            guard !Task.isCancelled, generation == connectionGeneration.value, mediaEpoch == voiceMediaEpoch else { break }
            attempted = true
            let result = await attemptVoiceHangup(pending, origin: origin, api: api, phase: "pending_hangup")
            if result == .retryable {
                scheduleVoiceHangupRetry(pending, origin: origin, api: api, phase: "pending_hangup", generation: generation, epoch: nil)
            }
        }
        return attempted
    }

    private func refreshPoolAfterVoiceEnd(api: APIClient) {
        guard self.api === api else { return }
        let generation = connectionGeneration.value
        let epoch = voiceMediaEpoch
        let revision = voiceReadinessRevision
        Task { [weak self] in
            guard let status = try? await api.voicePoolStatus(), let self,
                  generation == self.connectionGeneration.value, self.api === api,
                  epoch == self.voiceMediaEpoch, revision == self.voiceReadinessRevision else { return }
            self.voicePoolStatus = status
        }
    }

    #if os(macOS)
    private func terminateVoiceForAppExit() async {
        guard let session = voiceSession, let api = voiceAPI, let epoch = voiceSessionMediaEpoch else { return }
        deliverVoiceEnd(session, api: api, mediaEpoch: epoch, reason: .appTerminate)
        closeVoiceLocalMedia(mediaEpoch: epoch)
        await withTaskGroup(of: Void.self) { group in
            group.addTask { _ = await self.requestVoiceHangup(session, api: api, phase: "app_terminate") }
            group.addTask {
                try? await Task.sleep(for: .seconds(2))
            }
            await group.next()
            group.cancelAll()
        }
    }
    #endif

    private func closeVoiceLocalMedia(mediaEpoch: UInt64) {
        guard voiceMediaFence.stop(mediaEpoch) else { return }
        negotiatingVoiceEpoch = nil
        silence.reset()
        #if DEBUG
        voiceLocalMediaStopObserverForTesting?()
        #endif
        let transport = voiceTransport
        let audio = voiceAudioSession
        voiceTransport = nil
        voiceAudioSession = nil
        transport?.onDataMessage = nil
        transport?.onConnectionState = nil
        transport?.onDataChannelState = nil
        audio?.onLifecycleEvent = nil
        try? audio?.deactivate()
        transport?.closeImmediately()
    }

    private func handleVoiceMediaLoss(_ loss: VoiceMediaLoss, mediaEpoch: UInt64) async {
        guard mediaEpoch == voiceMediaEpoch else { return }
        guard let rawReason = loss.message else { return }
        let reason = uiText(rawReason)
        if continuousVoiceEnabled {
            voiceStatus = .reconnecting(reason: reason)
            #if DEBUG
            voiceStatusObserverForTesting?(voiceStatus)
            #endif
        }
        if let api = voiceAPI {
            let (errorType, errorSummary): (String, String) = switch loss {
            case let .connection(state): ("connection", Self.voiceConnectionStateName(state))
            case let .audio(event): ("audio", Self.voiceAudioEventName(event))
            case let .dataChannel(state): ("data_channel", Self.voiceDataChannelStateName(state))
            }
            let key = "\(mediaEpoch):\(errorType):\(errorSummary)"
            if recordedVoiceMediaLosses.insert(key).inserted {
                let session = voiceSession
                let peerState = errorType == "connection" ? errorSummary : nil
                let dataChannelState = errorType == "data_channel" ? errorSummary : nil
                await api.recordDiagnosticAndWait(
                    phase: "voice_media_loss",
                    requestPath: "/v1/voice/media",
                    errorType: errorType,
                    errorSummary: String(errorSummary.prefix(64)),
                    voiceSessionID: session?.voiceSessionId,
                    voiceGeneration: session?.generation,
                    voiceMediaEpoch: mediaEpoch,
                    voicePeerState: peerState,
                    voiceDataChannelState: dataChannelState
                )
                Task { await api.uploadDiagnosticsIfPossible() }
            }
        }
        guard mediaEpoch == voiceMediaEpoch else { return }
        if let session = voiceSession, let api = voiceAPI, voiceSessionMediaEpoch == mediaEpoch {
            if continuousVoiceEnabled {
                voiceStatus = .reconnecting(reason: reason)
                #if DEBUG
                voiceStatusObserverForTesting?(voiceStatus)
                #endif
            }
            let generation = connectionGeneration.value
            await finishVoiceSession(session, api: api, mediaEpoch: mediaEpoch, lossReason: reason, reason: loss.diagnosticReason)
            let closedEpoch = mediaEpoch &+ 1
            if continuousVoiceEnabled {
                Task { @MainActor [weak self] in
                    try? await Task.sleep(for: .seconds(2))
                    guard let self, generation == self.connectionGeneration.value, closedEpoch == self.voiceMediaEpoch else { return }
                    await self.startVoiceIfEnabled()
                }
            }
            return
        }
        voiceStartMediaLoss = (mediaEpoch, reason)
        voiceAttemptEndReason = loss.diagnosticReason
        voiceStatus = .interrupted(reason: uiFormat("%@ Stopping voice setup and releasing local media.", reason))
        closeVoiceLocalMedia(mediaEpoch: mediaEpoch)
    }

    private func wireVoiceTransport(_ transport: LibWebRTCTransport, generation: UInt64 = 0, mediaEpoch: UInt64) {
        transport.onDataMessage = { [weak self] data in
            Task { @MainActor in self?.receiveVoiceData(data, generation: generation, mediaEpoch: mediaEpoch) }
        }
        transport.onConnectionState = { [weak self] state in
            Task { @MainActor in
                await self?.receiveConnectionState(state, mediaEpoch: mediaEpoch)
            }
        }
        transport.onDataChannelState = { [weak self] state in
            Task { @MainActor in
                await self?.receiveDataChannelState(state, mediaEpoch: mediaEpoch)
            }
        }
    }

    private static func voiceConnectionStateName(_ state: RTCPeerConnectionState) -> String {
        switch state {
        case .new: "new"
        case .connecting: "connecting"
        case .connected: "connected"
        case .disconnected: "disconnected"
        case .failed: "failed"
        case .closed: "closed"
        @unknown default: "unknown"
        }
    }

    private static func voiceDataChannelStateName(_ state: RTCDataChannelState) -> String {
        switch state {
        case .connecting: "connecting"
        case .open: "open"
        case .closing: "closing"
        case .closed: "closed"
        @unknown default: "unknown"
        }
    }

    private static func voiceAudioEventName(_ event: VoiceAudioLifecycleEvent) -> String {
        switch event {
        case .interrupted: "interrupted"
        case .routeUnavailable: "route_unavailable"
        }
    }

    private func receiveConnectionState(_ state: RTCPeerConnectionState, mediaEpoch: UInt64) async {
        await handleVoiceMediaLoss(.connection(state), mediaEpoch: mediaEpoch)
    }

    private func receiveDataChannelState(_ state: RTCDataChannelState, mediaEpoch: UInt64) async {
        guard state == .closing || state == .closed else { return }
        await handleVoiceMediaLoss(.dataChannel(state), mediaEpoch: mediaEpoch)
    }

    private func validServerURL(_ address: String) -> URL? { try? ServerAddress.normalize(address) }

    private func keyStore(url: URL) throws -> any InstanceKeyStoring {
        #if DEBUG
        if let override = keyStoreOverrideForTesting { return try override(url) }
        #endif
        return try InstanceKeyStore(serverURL: url)
    }

    private func connectionAPI(url: URL, key: InstanceKey) throws -> APIClient {
        #if DEBUG
        if let override = connectionAPIOverrideForTesting { return try override(url, key, installationID) }
        #endif
        return try APIClient(serverURL: url, credentials: key, installationID: installationID)
    }

    private func configure() throws -> APIClient {
        let url = try ServerAddress.normalize(serverAddress)
        if let api, api.serverURL == url { return api }
        let store = try keyStore(url: url)
        guard let key = try store.load() else { throw ZoeError.missingCredential }
        if configuredServerURL != url {
            lockInstance()
            connectionGeneration.switchOrigin(to: url)
            connectionEpoch = connectionGeneration.value
            configuredServerURL = url
            loadVoicePreference()
            voiceConfig = nil
            voicePoolStatus = nil
        }
        let api = try connectionAPI(url: url, key: key)
        self.identity = store
        self.api = api
        // Journals are opened only after an authenticated snapshot establishes instance identity.
        return api
    }

    @discardableResult
    private func startEventLoop() {
        guard let api else { return }
        eventTask?.cancel()
        eventTask = Task { [weak self] in
            guard let self else { return }
            await self.listen(using: api)
        }
    }

    private func receiveVoiceData(_ data: Data, generation: UInt64, mediaEpoch: UInt64) {
        guard generation == connectionGeneration.value,
              mediaEpoch == voiceMediaEpoch,
              negotiatingVoiceEpoch == mediaEpoch || (voiceSessionMediaEpoch == mediaEpoch && voiceSession != nil),
              data.count <= 1_048_576 else { return }
        guard let outer = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { return }
        var event = outer
        if outer["type"] as? String == "data_message", let encoded = outer["data"] as? String,
           let innerData = encoded.data(using: .utf8), let inner = (try? JSONSerialization.jsonObject(with: innerData)) as? [String: Any] {
            event = inner
        }
        let type = (event["type"] as? String) ?? ""
        let activity = voiceActivityClassifier.accept(event)
        let outerType = (outer["type"] as? String) ?? "missing"
        voiceEventCensus.note(type: outerType, activity: activity)
        if outerType == "data_message", event["type"] as? String != outerType {
            voiceEventCensus.note(type: "data_message/" + String(type.prefix(48)), activity: activity)
        }
        if type != "data_message" {
            voiceInnerEventCensus.note(type: type, activity: activity)
        }
        if activity, voiceActivityCoalescer.shouldReset(now: silence.diagnosticNow) {
            silence.noteVoiceActivity()
            recordSilenceReset(type: type)
        }
        if type == "voice.end" || type == "voice_end" ||
            ((type == "function_call" || type == "tool_call") && ((event["name"] as? String) == "voice.end" || (event["name"] as? String) == "voice_end")) {
            if negotiatingVoiceEpoch == mediaEpoch {
                // The gateway is already ending this call. Fence the handshake before it can publish active media.
                earlyVoiceEndEpoch = mediaEpoch
                closeVoiceLocalMedia(mediaEpoch: mediaEpoch)
                return
            }
            Task { @MainActor [weak self] in
                await self?.receiveRemoteVoiceEnd(generation: generation, mediaEpoch: mediaEpoch)
            }
        }
    }

    private func receiveRemoteVoiceEnd(generation: UInt64, mediaEpoch: UInt64) async {
        guard generation == connectionGeneration.value,
              mediaEpoch == voiceMediaEpoch,
              voiceSessionMediaEpoch == mediaEpoch,
              let session = voiceSession,
              let api = voiceAPI else { return }
        await finishVoiceSession(session, api: api, mediaEpoch: mediaEpoch, lossReason: nil, reason: .voiceEndEvent, gatewayIsEnding: true)
    }

    private var instanceLineageID: String?
    private var eventCursor: String?
    public let silence: SilenceController
    private var voiceMonitorTask: Task<Void, Never>?

    private func adoptInstanceIdentity(_ identity: String, api: APIClient) throws {
        guard !identity.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { throw ZoeError.invalidResponse }
        guard instanceLineageID != identity || coordinator == nil else { return }
        let journal = try CommandJournal(instanceLineageID: identity)
        if instanceLineageID != identity {
            closeUISurfaces()
            eventCursor = nil
            authoritativeControlSnapshot = nil
            recoveryACKTask?.cancel()
        }
        instanceLineageID = identity
        self.journal = journal
        coordinator = CommandCoordinator(api: api, journal: journal)
    }

    private func validateConnection() async throws {
        guard let api else { throw ZoeError.missingCredential }
        let generation = connectionGeneration.value
        let snapshot = try await api.snapshot()
        guard isCurrentConnection(generation, sourceAPI: api) else { return }
        try adoptInstanceIdentity(snapshot.authority.instanceLineageId, api: api)
        // Neither metadata nor UI snapshots prove consumption of global events.
    }

    private func listen(using api: APIClient) async {
        let generation = connectionGeneration.value
        while !Task.isCancelled && isCurrentConnection(generation, sourceAPI: api) {
            let started = Date()
            let diagnostics = StreamDiagnostics(now: started)
            api.recordDiagnostic(phase: "events_stream", requestPath: "/v1/events", errorSummary: "connect", level: .info)
            let streamReportTask = Task { @MainActor in
                while !Task.isCancelled {
                    try? await Task.sleep(for: .seconds(1))
                    guard !Task.isCancelled else { return }
                    diagnostics.report(api: api)
                }
            }
            do {
                try await receiveEventConnection(using: api, generation: generation) { [weak self] push in
                    await self?.receiveStreamPush(push, diagnostics: diagnostics, generation: generation, api: api)
                }
                streamReportTask.cancel()
                diagnostics.report(api: api, force: true)
                api.recordDiagnostic(phase: "events_stream", requestPath: "/v1/events", errorSummary: "ended;dur=\(DiagnosticSummaries.seconds(Date().timeIntervalSince(started)))", level: .info)
                if Task.isCancelled || !isCurrentConnection(generation, sourceAPI: api) { return }
                streamBackoff = 1
            } catch {
                streamReportTask.cancel()
                diagnostics.report(api: api, force: true)
                let redacted = RedactedDiagnostic(error)
                api.recordDiagnostic(phase: "events_stream", requestPath: "/v1/events", httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: "error;\(redacted.errorType);dur=\(DiagnosticSummaries.seconds(Date().timeIntervalSince(started)))", level: .info)
                if Task.isCancelled || !isCurrentConnection(generation, sourceAPI: api) { return }
                if Self.isAccessDenied(error) {
                    lockInstance()
                    authentication = .disconnected
                    setOffline(error)
                    return
                }
                notice = userMessage(error)
            }
            let delay = streamBackoff
            streamBackoff = min(streamBackoff * 2, 30)
            api.recordDiagnostic(phase: "events_stream", requestPath: "/v1/events", errorSummary: "reconnect_in=\(delay)", level: .info)
            try? await Task.sleep(nanoseconds: delay * 1_000_000_000)
        }
    }

    /// UI repair never advances global delivery. Replaying from the last consumed global
    /// cursor preserves non-UI events; surface revisions suppress obsolete UI presentations.
    private func receiveEventConnection(using api: APIClient, generation: UInt64,
                                         onPush: @escaping @Sendable (ServerPush) async -> Void) async throws {
        let needsRecovery = eventCursor == nil || eventCursor == "0" || eventRecoveryRequired
        if needsRecovery {
            do {
                try await recoverGlobalState(using: api, generation: generation)
            } catch {
                guard generation == connectionGeneration.value else { return }
                eventRecoveryRequired = true
                authentication = .unavailable
                notice = "Event history recovery is unavailable. Retrying without skipping events."
                throw error
            }
        }
        guard !Task.isCancelled, generation == connectionGeneration.value else { return }
        if !needsRecovery {
            let snapshot = try await api.uiSnapshot()
            guard !Task.isCancelled, generation == connectionGeneration.value else { return }
            let removed = uiSurfaceRequests.reconcile(snapshot)
            onUISurfacesClosed?(removed)
            for surface in snapshot.surfaces {
                onUISurfaceInputState?(surface.event)
                if shouldReplaySurface(surface.event) {
                    await applyUISurface(surface.event)
                } else {
                    // Reserve the recovered revision without claiming a native render.
                    uiSurfaceRequests.apply(surface.event)
                }
                guard !Task.isCancelled, generation == connectionGeneration.value else { return }
            }
            if let coordinator {
                let instance = instanceLineageID
                _ = try? await coordinator.recoverPending(currentInstanceLineageID: instance, onOutcome: { _ in }, isValid: { [weak self] in
                    guard let self else { return false }
                    return generation == self.connectionGeneration.value && self.instanceLineageID == instance
                })
            }
            guard !Task.isCancelled, generation == connectionGeneration.value else { return }
        }
        // Only actual events or the complete recovery baseline advance this cursor.
        _ = try await EventStreamClient(api: api).run(after: eventCursor ?? "0", onConnected: {
            await api.recordDiagnosticAndWait(phase: "events_stream", requestPath: "/v1/events", errorSummary: "connected", level: .info)
        }, onPush: onPush)
    }

    private func recoverGlobalState(using api: APIClient, generation: UInt64) async throws {
        let recovery = try await api.recovery()
        guard !Task.isCancelled, generation == connectionGeneration.value else { throw CancellationError() }
        guard let identity = recovery.snapshot.instanceLineageID else { throw ZoeError.invalidResponse }
        try adoptInstanceIdentity(identity, api: api)
        #if DEBUG
        await recoveryApplicationGateForTesting?()
        #endif
        guard !Task.isCancelled, generation == connectionGeneration.value else { throw CancellationError() }
        var staged = uiSurfaceRequests
        let removed = staged.reconcile(recovery.ui)
        for surface in recovery.ui.surfaces {
            let event = surface.event
            onUISurfaceInputState?(event)
            if staged.apply(event) {
                guard shouldReplaySurface(event) else { continue }
                if recoveryRendered[event.binding] == nil || isCurrentUIPresentation?(event) == false {
                    // An attempted replacement can change native content even if it
                    // fails. Previous physical evidence is no longer usable for ACKs.
                    recoveryRendered = recoveryRendered.filter { $0.key.surfaceID != event.surfaceID }
                    let displayed = onUISurface?(event) ?? false
                    guard generation == connectionGeneration.value, !Task.isCancelled else { throw CancellationError() }
                    guard displayed else { throw ZoeError.invalidResponse }
                    recordUISurfaceHistory(event)
                    recoveryRendered = recoveryRendered.filter { $0.key.surfaceID != event.surfaceID }
                    recoveryRendered[event.binding] = event
                    sessionPresentedSurfaces[event.surfaceID] = event
                }
                staged.recordRender(event, success: true)
            }
        }
        if let local = voiceSession {
            let current = recovery.voice.currentSession
            if current?.id != local.voiceSessionId || current?.generation != local.generation || current?.state != "active" {
                // The snapshot already proves this local call is no longer current.
                // Close only local media; never hang up the replacement server call.
                // Audio cannot be rolled back: publish stopped truthfully before awaiting.
                let epoch = voiceSessionMediaEpoch
                voiceSession = nil
                voiceSessionMediaEpoch = nil
                voiceSessionMediaClosed = false
                voiceAPI = nil
                voiceStatus = .stopped
                if let epoch { closeVoiceLocalMedia(mediaEpoch: epoch) }
                guard !Task.isCancelled, generation == connectionGeneration.value else { throw CancellationError() }
            }
        }
        guard !Task.isCancelled, generation == connectionGeneration.value else { throw CancellationError() }
        #if DEBUG
        await recoveryCommitGateForTesting?()
        guard !Task.isCancelled, generation == connectionGeneration.value else { throw CancellationError() }
        #endif
        // Commit boundary: no throwing or suspension between these assignments and
        // checkpoint adoption. Native displays are not physically transactional.
        instanceLineageID = identity
        authoritativeControlSnapshot = recovery.snapshot
        recoveredVoicePool = recovery.voice.pool
        uiSurfaceRequests = staged
        if var status = voicePoolStatus {
            status.blocked = recovery.voice.pool.blocked
            status.reasonCode = recovery.voice.pool.reasonCode
            voicePoolStatus = status
        }
        eventCursor = recovery.cursor
        eventRecoveryRequired = false
        authentication = .connected
        onUISurfacesClosed?(removed)
        let events = recovery.ui.surfaces.map(\.event).filter { staged.outcomes[$0.binding] == true }
        let instance = identity
        let coordinator = coordinator
        // ACK transport/receipt lookup is independent of authoritative recovery.
        // Coordinator persists uncertain submissions; later connections retry them.
        recoveryACKTask?.cancel()
        recoveryACKTask = Task { @MainActor [weak self] in
            guard let self, generation == self.connectionGeneration.value else { return }
            if let coordinator {
                for event in events {
                    guard generation == self.connectionGeneration.value, !Task.isCancelled else { return }
                    guard self.isCurrentUIPresentation?(event) != false else { continue }
                    _ = try? await coordinator.submit(kind: "ui.rendered", payload: event.renderedPayload(success: true), instanceLineageID: instance, idempotencyKey: event.renderedIdempotencyKey, isValid: { [weak self] in
                        guard let self else { return false }
                        return generation == self.connectionGeneration.value && self.instanceLineageID == instance && self.isCurrentUIPresentation?(event) != false
                    })
                    guard generation == self.connectionGeneration.value, !Task.isCancelled else { return }
                }
                _ = try? await coordinator.recoverPending(currentInstanceLineageID: instance, onOutcome: { _ in }, isValid: { [weak self] in
                    guard let self else { return false }
                    return generation == self.connectionGeneration.value && self.instanceLineageID == instance &&
                        events.allSatisfy { self.isCurrentUIPresentation?($0) != false }
                })
                guard generation == self.connectionGeneration.value, !Task.isCancelled else { return }
            }
        }
    }

    private func receiveStreamPush(_ push: ServerPush, diagnostics: StreamDiagnostics, generation: UInt64, api: APIClient) async {
        switch push {
        case let .event(envelope): diagnostics.received.note(type: envelope.type)
        case .snapshotRequired: diagnostics.received.note(type: "snapshot_required")
        }
        diagnostics.report(api: api)
        await handle(push, expectedGeneration: generation, sourceAPI: api)
    }

    private func recordUITrace(phase: String, summary: String, level: DiagnosticLevel, upload: Bool = false) {
        guard let api else { return }
        Task {
            await api.recordDiagnosticAndWait(phase: phase, requestPath: "/v1/events", errorSummary: String(summary.prefix(200)), level: level)
            if upload { api.triggerDiagnosticUpload() }
        }
    }

    private func handle(_ push: ServerPush, expectedGeneration: UInt64, sourceAPI: APIClient?) async {
        guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI), !Task.isCancelled else { return }
        switch push {
        case let .event(envelope):
            if let cursor = Int64(envelope.cursor), let previous = eventCursor.flatMap(Int64.init), cursor <= previous {
                // A same-version replay can still reconcile a pending ACK below.
                if envelope.type != "ui.surface" { return }
            }
            // Advance only after this event has been handled, never from a UI snapshot.
            defer {
                if isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI),
                   let cursor = Int64(envelope.cursor), cursor > (eventCursor.flatMap(Int64.init) ?? -1) {
                    eventCursor = envelope.cursor
                }
                #if DEBUG
                consumedEventObserverForTesting?(envelope.type)
                #endif
            }
            if envelope.type == "ui.surface" {
                recordUITrace(phase: "ui_surface_received", summary: UISurfaceEvent.receivedSummary(payload: envelope.payload, cursor: envelope.cursor), level: .info)
                guard authentication.isConnected else {
                    recordUITrace(phase: "ui_surface_not_connected", summary: "not_connected", level: .warn)
                    return
                }
                let decoded = UISurfaceEvent.decodeWithDiagnostic(envelope.payload)
                let failure = decoded.failure ?? ((envelope.aggregateType != "ui_surface" || envelope.aggregateId != decoded.event?.surfaceID) ? "aggregate_mismatch" : nil)
                guard let event = decoded.event, failure == nil else {
                    recordUITrace(phase: "ui_surface_decode_failed", summary: failure ?? "payload_decode", level: .warn, upload: true)
                    return
                }
                guard envelope.aggregateVersion == event.surfaceVersion else { return }
                await applyUISurface(event)
            }
            if envelope.type == "voice.end", envelope.aggregateId == voiceSession?.voiceSessionId,
               let epoch = voiceSessionMediaEpoch {
                await receiveRemoteVoiceEnd(generation: expectedGeneration, mediaEpoch: epoch)
            }
        case .snapshotRequired:
            eventRecoveryRequired = true
            // The stream ends at this marker. The next connection first obtains a
            // complete checkpoint, retaining the old cursor on every failure.
        }
    }

    private func shouldReplaySurface(_ event: UISurfaceEvent) -> Bool {
        let wasPresented = sessionPresentedSurfaces[event.surfaceID].map { isCurrentUIPresentation?($0) != false } ?? false
        return SurfaceClosePolicy.shouldReplay(taskQuestion: event.taskQuestion, wasPresented: wasPresented)
    }

    private func applyUISurface(_ event: UISurfaceEvent) async {
        guard uiSurfaceRequests.apply(event) else {
            if uiSurfaceRequests.outcomes[event.binding] != nil,
               let journal, let coordinator,
               let pending = try? await journal.all(),
               pending.contains(where: { $0.idempotencyKey == event.renderedIdempotencyKey }) {
                _ = try? await coordinator.retryPending(idempotencyKey: event.renderedIdempotencyKey, currentInstanceLineageID: instanceLineageID)
            }
            return
        }
        recoveryRendered = recoveryRendered.filter { $0.key.surfaceID != event.surfaceID }
        let displayed = onUISurface?(event) ?? false
        if displayed {
            sessionPresentedSurfaces[event.surfaceID] = event
            recordUISurfaceHistory(event)
        }
        uiSurfaceRequests.recordRender(event, success: displayed)
        recordUITrace(phase: displayed ? "ui_surface_displayed" : "ui_surface_display_failed", summary: event.diagnosticSummary, level: displayed ? .info : .warn, upload: !displayed)
        await submitUICommand(kind: "ui.rendered", payload: event.renderedPayload(success: displayed), key: event.renderedIdempotencyKey)
    }

    private func isCurrentConnection(_ generation: UInt64, sourceAPI: APIClient?) -> Bool {
        generation == connectionGeneration.value && (sourceAPI == nil || sourceAPI === api)
    }

    /// History is local and never fed back to the server; only the authenticated instance is visible.
    private func recordUISurfaceHistory(_ event: UISurfaceEvent) {
        guard let instance = instanceLineageID, !serverAddress.isEmpty else {
            var missing: [String] = []
            if instanceLineageID == nil { missing.append("instance") }
            if serverAddress.isEmpty { missing.append("server") }
            recordUITrace(phase: "history_scope_unavailable", summary: "missing=" + missing.joined(separator: ","), level: .warn, upload: true)
            return
        }
        let scope = UISurfaceHistoryScope(instanceLineageID: instance)
        guard let write = onUISurfaceDisplayed else { return }
        Task { [weak self] in
            do {
                try await write(event, scope)
                self?.recordUITrace(phase: "history_entry_written", summary: "written", level: .info)
            } catch {
                self?.recordUITrace(phase: "history_write_failed", summary: Self.historyFailureCategory(error), level: .warn, upload: true)
            }
        }
    }

    private static func historyFailureCategory(_ error: Error) -> String {
        guard let historyError = error as? UISurfaceHistoryError else { return "io_error" }
        switch historyError {
        case .invalidID: return "invalidID"
        case .notFound: return "notFound"
        }
    }

    public var historyScope: UISurfaceHistoryScope? {
        guard let instanceLineageID else { return nil }
        return UISurfaceHistoryScope(instanceLineageID: instanceLineageID)
    }

    private func closeUISurfaces() {
        sessionPresentedSurfaces.removeAll()
        recoveryRendered.removeAll()
        let ids = Array(uiSurfaceRequests.requestIDs.keys)
        for _ in ids { recordUITrace(phase: "ui_surface_closed", summary: "disconnect", level: .info) }
        uiSurfaceRequests.removeAll()
        onUISurfacesClosed?(ids)
        onUISurfacesReset?()
    }

    public func noteUISurfaceUserClosed() {
        recordUITrace(phase: "ui_surface_closed", summary: "user_closed", level: .info)
    }

    public func noteUISurfaceRenderErrors(_ errors: [Error]) {
        guard let first = errors.first else { return }
        // Error descriptions can contain component IDs and user content; report only the type.
        let errorType = String(String(reflecting: type(of: first)).prefix(160))
        recordUITrace(phase: "ui_surface_render_error", summary: "count=\(errors.count) first_type=\(errorType)", level: .warn, upload: true)
    }

    public var canSubmitConversationText: Bool { authentication.isConnected && coordinator != nil && instanceLineageID != nil }

    public func submitConversationText(_ text: String, idempotencyKey: String) async throws {
        guard canSubmitConversationText, let coordinator, let instanceLineageID else {
            throw ZoeError.persistence("Connect to the server before sending.")
        }
        _ = try await coordinator.submit(kind: "voice.text_input", payload: .object(["text": .string(text)]), instanceLineageID: instanceLineageID, idempotencyKey: idempotencyKey)
    }

    public func sendUISurfaceAction(binding: UISurfaceBinding, action: ResolvedAction) {
        let component = action.sourceComponentId
        let surfaceID = binding.surfaceID
        guard authentication.isConnected else {
            SurfaceActionOutcomes.shared.report(surfaceID: surfaceID, component: component, success: false)
            return
        }
        do {
            let payload = try uiActionPayload(binding: binding, action: action)
            let generation = connectionGeneration.value
            let sourceAPI = api
            Task { @MainActor [weak self] in
                guard let self, self.isCurrentConnection(generation, sourceAPI: sourceAPI) else {
                    SurfaceActionOutcomes.shared.report(surfaceID: surfaceID, component: component, success: false)
                    return
                }
                let accepted = await self.submitUICommand(kind: "ui.action", payload: payload, key: UUID().uuidString.lowercased())
                SurfaceActionOutcomes.shared.report(surfaceID: surfaceID, component: component, success: accepted)
            }
        } catch {
            api?.recordDiagnostic(phase: "ui_action", requestPath: "/v1/commands", errorType: "encoding_error", errorSummary: "invalid_action_context", level: .warn)
            SurfaceActionOutcomes.shared.report(surfaceID: surfaceID, component: component, success: false)
        }
    }

    #if os(macOS)
    public func reportScreenSize() async {
        guard authentication.isConnected, let visible = screenVisibleSize() else { return }
        let size = SurfaceSize(width: Int(visible.width.rounded()), height: Int(visible.height.rounded()))
        guard size != lastReportedScreenSize else { return }
        // Reserve before suspension so simultaneous screen notifications do not duplicate reports.
        let previous = lastReportedScreenSize
        lastReportedScreenSize = size
        let sent = await submitUICommand(kind: "ui.screen", payload: size.screenPayload, key: size.screenIdempotencyKey)
        if sent {
            api?.recordDiagnostic(phase: "ui_screen_report", requestPath: "/v1/commands", errorSummary: "w=\(size.width);h=\(size.height)", level: .info)
        } else if lastReportedScreenSize == size {
            lastReportedScreenSize = previous
        }
    }
    #endif

    @discardableResult private func submitUICommand(kind: String, payload: JSONValue, key: String) async -> Bool {
        guard authentication.isConnected, let api else { return false }
        do {
            if kind == "ui.screen" {
                _ = try await api.submit(kind: kind, payload: payload, idempotencyKey: key)
            } else {
                guard let coordinator else { return false }
                guard let instanceLineageID else { throw ZoeError.invalidResponse }
                _ = try await coordinator.submit(kind: kind, payload: payload, instanceLineageID: instanceLineageID, idempotencyKey: key)
            }
            return true
        } catch {
            let diagnostic = RedactedDiagnostic(error)
            api.recordDiagnostic(phase: kind == "ui.screen" ? "ui_screen_report" : "ui_command", requestPath: "/v1/commands", httpStatus: diagnostic.httpStatus,
                                 errorType: diagnostic.errorType, errorSummary: diagnostic.errorSummary, level: .warn)
            if kind == "ui.rendered" {
                api.recordDiagnostic(phase: "ui_rendered_ack", requestPath: "/v1/commands", httpStatus: diagnostic.httpStatus, errorType: diagnostic.errorType, errorSummary: diagnostic.errorSummary, level: .error)
            }
            return false
        }
    }

    private static func isAccessDenied(_ error: Error) -> Bool {
        guard let failure = error as? ZoeError,
              case let .http(status, code, _, _) = failure else { return false }
        return status == 401 || (status == 403 && code == "authentication_required")
    }

    private func setOffline(_ error: Error) {
        if Self.isAccessDenied(error), authentication.isConnected {
            lockInstance()
            authentication = .disconnected
        }
        let message = userMessage(error)
        notice = message
    }

    private func userMessage(_ error: Error) -> String {
        reportClientError(error, code: "client_error")
        if Self.isAccessDenied(error) { return "The instance key was rejected. Replace it with the current key from your server." }
        if error is ConnectionInputError { return error.localizedDescription }
        if let failure = error as? ZoeError, case .missingCredential = failure {
            return "Enter your server address and instance key to connect."
        }
        return "Something went wrong. Try again."
    }

    private func reportClientError(_ error: Error, code: String) {
        let redacted = RedactedDiagnostic(error)
        api?.recordDiagnostic(phase: "client_error", requestPath: "client", httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: "\(code):\(redacted.errorSummary)")
    }

    static func voiceDialFailureMessage(_ error: Error) -> String {
        if let error = error as? ZoeError, case let .http(status, code, _, _) = error {
            switch (status, code) {
            case (400, "invalid_request"): return uiText("The voice settings or call request were not accepted.")
            case (401, "unauthenticated"): return uiText("The instance key was not accepted for voice calls.")
            case (409, "voice_lease_busy"): return uiText("Another voice call is active or still closing.")
            case (409, "idempotency_conflict"): return uiText("This voice call could not be retried. Start a new call.")
            case (503, "voice_pool_quarantined"): return uiText("The voice account pool is quarantined.")
            case (503, "voice_unavailable"): return uiText("Voice is unavailable right now.")
            default: break
            }
        }
        return uiText("Voice is unavailable right now.")
    }

}
