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
    public var onUISurfacesClosed: (([String]) -> Void)?
    private var uiSurfaceRequests = UISurfaceRequests()
    @Published public internal(set) var serverAddress: String
    @Published public private(set) var isConnecting = false
    @Published public private(set) var notice: String?
    @Published public private(set) var isBusy = false
    @Published public private(set) var authentication: InstanceConnectionState = .checking
    @Published public private(set) var pendingConnectionImport: InstallationConnection?
    @Published public private(set) var connectionImportError: String?
    private let installationID: String
    private var connectionCleanupWarning: String?
    @Published public private(set) var connectionEpoch: UInt64 = 0
    @Published public private(set) var voiceConfig: VoiceConfig?
    @Published public private(set) var voicePoolStatus: VoicePoolStatus?
    @Published public private(set) var voiceStatus: VoiceIntegrationStatus = .idle
    @Published public private(set) var continuousVoiceEnabled: Bool
    @Published public private(set) var silenceHangupEnabled: Bool
    @Published public private(set) var silenceTimeoutSeconds: Double
    @Published public var voiceDraft = ""
    @Published public private(set) var voiceDraftRevision: Int64 = 0
    private let defaults: UserDefaults
    private let continuousVoiceKey = "zoe.voice.continuous"
    private let pendingVoiceHangupKey = "zoe.voice.pendingHangup"
    private var api: APIClient?
    private var identity: (any InstanceKeyStoring)?
    private var journal: CommandJournal?
    private var coordinator: CommandCoordinator?
    private var eventTask: Task<Void, Never>?
    private var syncLoop: ClientSyncLoop?
    private var streamBackoff: UInt64 = 1
    private var connectionGeneration = ConnectionGeneration()
#if DEBUG
    var connectionAPIOverrideForTesting: ((URL, InstanceKey, String) throws -> APIClient)?
    var keyStoreOverrideForTesting: ((URL) throws -> any InstanceKeyStoring)?
    var suppressConnectionFollowupsForTesting = false
    var voiceDiagnosticObserverForTesting: ((String, String) -> Void)?
    var voiceCensusSummaryForTesting: String { voiceEventCensus.summary() }
    func reportVoiceCensusForTesting() { reportVoiceCensusIfNeeded() }
    var voiceLocalMediaStopObserverForTesting: (() -> Void)?
    var voiceStatusObserverForTesting: ((VoiceIntegrationStatus) -> Void)?
    var voiceHangupOverrideForTesting: ((String) async throws -> VoiceOperationResponse)?
    func handleEventForTesting(_ envelope: EventEnvelope, expectedConnectionEpoch: UInt64? = nil) async {
        await handle(.event(envelope), expectedGeneration: expectedConnectionEpoch ?? connectionGeneration.value, sourceAPI: nil)
    }
    func setAuthenticationForTesting(_ state: InstanceConnectionState) {
        authentication = state
    }

    func setSnapshotForTesting(_ snapshot: Snapshot?) {
        workspaceID = snapshot?.workspaceId
        eventCursor = snapshot?.eventCursor
    }

    @discardableResult
    func installVoiceSessionForTesting(_ session: VoiceSessionResponse, api: APIClient) -> UInt64 {
        let epoch = voiceMediaFence.beginAttempt()
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

    var voiceHeartbeatActiveForTesting: Bool { voiceHeartbeat.isActive }

    func installVoiceHeartbeatForTesting(_ task: Task<Void, Never>) {
        voiceHeartbeat.install(task)
    }

    func handleVoiceHeartbeatFailureForTesting(sessionID: String, api: APIClient, mediaEpoch: UInt64, generation: UInt64, message: String) async {
        await handleVoiceHeartbeatFailure(sessionID: sessionID, api: api, mediaEpoch: mediaEpoch, generation: generation, message: message)
    }

    func receiveVoiceDataForTesting(_ data: Data, generation: UInt64? = nil, mediaEpoch: UInt64? = nil) async {
        receiveVoiceData(data, generation: generation ?? connectionGeneration.value, mediaEpoch: mediaEpoch ?? voiceMediaEpoch)
        await Task.yield()
    }

    func configureVoiceCoordinatorForTesting(api: APIClient, journal: CommandJournal) {
        self.api = api
        coordinator = CommandCoordinator(api: api, journal: journal)
    }

    var hasPendingVoiceHangupForTesting: Bool { pendingVoiceHangup() != nil }

    func retryPendingVoiceHangupForTesting(api: APIClient) async {
        await retryPendingVoiceHangup(api: api)
    }
#endif
    private var configuredServerURL: URL?
    private var voiceTransport: LibWebRTCTransport?
    private var voiceAudioSession: VoiceAudioSessionAdapter?
    private var voiceSession: VoiceSessionResponse?
    private var voiceMediaFence = VoiceMediaEpochFence()
    private var voiceMediaEpoch: UInt64 { voiceMediaFence.current }
    private var voiceSessionMediaEpoch: UInt64?
    private var voiceSessionMediaClosed = false
    private var voiceStartMediaLoss: (epoch: UInt64, reason: String)?
    private var voiceStartInProgress = false
    private var voiceStartedAt: Date?
    private var diagnosticConnectedAt: Date?
    private var silenceResetDiagnostics = SilenceResetDiagnostics()
    private var voiceEventCensus = EventCensusDiagnostics()
    private var voiceInnerEventCensus = EventCensusDiagnostics(completeTypes: true)
    private var voiceActivityCoalescer = VoiceActivityCoalescer()
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
    private var voiceHangupInFlightSessionID: String?
    private var voiceAPI: APIClient?
    private var recordedVoiceMediaLosses = Set<String>()
    private var voiceHangupRetryTask: Task<Void, Never>?
    #if os(macOS)
    private var terminationObserver: NSObjectProtocol?
    #endif
    private var voiceTurnID: String?
    private var voiceProviderMessageID: String?
    private var automaticVoiceTurnInFlight = Set<String>()
    private var automaticVoiceTurnCompleted = Set<String>()
    private let automaticVoiceTurnSetLimit = 128
    private let voiceHeartbeat = VoiceHeartbeatLease()
    public init(defaults: UserDefaults = .standard, silenceClock: SilenceClock = SystemSilenceClock()) {
        self.defaults = defaults
        let savedEnabled = defaults.object(forKey: "zoe.voice.silenceHangupEnabled") == nil ? true : defaults.bool(forKey: "zoe.voice.silenceHangupEnabled")
        silenceHangupEnabled = savedEnabled
        let savedTimeout = (defaults.object(forKey: "zoe.voice.silenceTimeoutSeconds") as? NSNumber)?.doubleValue ?? 60
        let timeout = savedTimeout.isFinite ? min(3600, max(10, savedTimeout)) : 60
        silenceTimeoutSeconds = timeout
        silence = SilenceController(timeoutSeconds: timeout, isEnabled: savedEnabled, clock: silenceClock)
        serverAddress = defaults.string(forKey: "zoe.instance.serverAddress") ?? ""
        let savedInstallation = defaults.string(forKey: "zoe.installationID")
        let identifier = savedInstallation.flatMap(UUID.init(uuidString:))?.uuidString.lowercased() ?? UUID().uuidString.lowercased()
        installationID = identifier
        defaults.set(identifier, forKey: "zoe.installationID")
        continuousVoiceEnabled = defaults.bool(forKey: continuousVoiceKey)
        silenceTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(1))
                guard let self, !Task.isCancelled else { return }
                self.reportVoiceCensusIfNeeded()
                if self.silence.tick() == .timedOut {
                    self.recordVoiceTrace(phase: "silence_tick_timeout", summary: DiagnosticSummaries.timeout(age: self.silence.lastActivityAge, timeout: self.silence.timeoutSeconds, enabled: self.silence.isEnabled, interaction: self.silence.interactionInProgress, elapsed: self.diagnosticElapsed), upload: true)
                    await self.endVoiceSession(reason: .silenceTimeout)
                }
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
        silenceTask?.cancel()
        voiceHangupRetryTask?.cancel()
        #if os(macOS)
        if let terminationObserver { NotificationCenter.default.removeObserver(terminationObserver) }
        #endif
        if let syncLoop { Task { await syncLoop.cancel() } }
    }
    public var canEndVoiceSession: Bool { voiceSession != nil }

    public func start() async {
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
            if let coordinator { _ = try? await coordinator.recoverPending(currentWorkspaceID: workspaceID, onOutcome: { _ in }) }
            guard generation == connectionGeneration.value else { return }
            startEventLoop()
            if continuousVoiceEnabled, voiceSession == nil {
                Task { @MainActor [weak self] in await self?.startVoiceIfEnabled(trigger: "app_start") }
            }
        } catch {
            if Self.isAccessDenied(error) || (error as? ZoeError).map({ if case .missingCredential = $0 { true } else { false } }) == true {
                lockWorkspace(clearDrafts: false)
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
            Task { await startVoiceIfEnabled(trigger: "menu_toggle") }
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

    private func noteVoiceConnected() {
        // Connected calls exclude setup time from both silence and end-duration accounting.
        voiceStartedAt = Date()
        diagnosticConnectedAt = silence.diagnosticNow
        silenceResetDiagnostics = SilenceResetDiagnostics()
        voiceEventCensus = EventCensusDiagnostics()
        voiceInnerEventCensus = EventCensusDiagnostics(completeTypes: true)
        voiceActivityCoalescer = VoiceActivityCoalescer()
        censusEndRecorded = false
        censusEarlyRecorded = false
        censusNextReportElapsed = 60
        silence.reset()
        silence.noteVoiceActivity()
        recordSilenceReset(type: "connect")
    }

    public func receiveInstallationLink(_ url: URL) {
        do {
            pendingConnectionImport = try InstallationConnection(link: url)
            connectionImportError = nil
        } catch {
            pendingConnectionImport = nil
            connectionImportError = uiText("This installation link is invalid.")
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
        do {
            let candidate = try InstallationConnection(serverAddress: address, key: key)
            if !serverAddress.isEmpty, candidate.serverURL != validServerURL(serverAddress) {
                notice = uiText("Clear the saved key before connecting to a different server.")
                return false
            }
            let candidateAPI = try connectionAPI(url: candidate.serverURL, key: candidate.key)
            _ = try await candidateAPI.snapshot()
            let store = try keyStore(url: candidate.serverURL)
            let previousKey = try store.load()
            // Local credential replacement is atomic. A failed write leaves the old workspace intact.
            try store.replace(candidate.key)
            do {
                guard try store.load() == candidate.key else {
                    throw ZoeError.persistence("The instance key could not be saved in local storage.")
                }
                await endVoiceSession(reason: .connectionCleared)
                // Cleanup must use the old credential scope without deleting the newly saved key.
                try await removeLocalConnection(preservingCredential: true, previousKey: previousKey)
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
        do {
            try await removeLocalConnection()
            notice = connectionCleanupWarning ?? uiText("Saved key and local connection data cleared.")
        } catch { notice = userMessage(error) }
    }

    private func removeLocalConnection(preservingCredential: Bool = false, previousKey: InstanceKey? = nil) async throws {
        connectionCleanupWarning = nil
        let oldJournal = journal
        lockWorkspace(clearDrafts: true)
        authentication = .disconnected
        try await oldJournal?.discard()
        if let url = try? ServerAddress.normalize(serverAddress) {
            let store = try keyStore(url: url)
            let cleanupKey = preservingCredential ? previousKey : try store.load()
            if let key = cleanupKey, oldJournal == nil {
                try await CommandJournal(serverURL: url, credentialScope: key.bearerToken()).discard()
            }
            if !preservingCredential { try store.reset() }
        }
        configuredServerURL = nil
        serverAddress = ""
        defaults.removeObject(forKey: "zoe.instance.serverAddress")
    }

    private func lockWorkspace(clearDrafts: Bool) {
        closeUISurfaces()
        connectionGeneration.advance()
        connectionEpoch = connectionGeneration.value
        eventTask?.cancel()
        eventTask = nil
        if let syncLoop { Task { await syncLoop.cancel() } }
        syncLoop = nil
        workspaceID = nil
        eventCursor = nil
        voiceConfig = nil
        voicePoolStatus = nil
        if clearDrafts { voiceDraft = "" }
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
        _ = voiceMediaFence.stop(voiceMediaEpoch)
        voiceHeartbeat.cancel()
        let oldTransport = voiceTransport
        let oldAudio = voiceAudioSession
        oldTransport?.onDataMessage = nil
        oldTransport?.onConnectionState = nil
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
        Task { [weak self] in
            await oldTransport?.close()
            guard let self, let oldSession, let oldAPI else { return }
            await self.releaseVoiceSession(oldSession, api: oldAPI, phase: "workspace_lock")
        }
    }
    public func refreshVoiceReadiness() async {
        guard let api = try? configure() else { return }
        let generation = connectionGeneration.value
        do {
            async let config = api.voiceConfig()
            async let pool = api.voicePoolStatus()
            let (nextConfig, nextPool) = try await (config, pool)
            guard generation == connectionGeneration.value, api === self.api else { return }
            voiceConfig = nextConfig
            voicePoolStatus = nextPool
            if voiceSession == nil { voiceStatus = .idle }
        } catch {
            guard generation == connectionGeneration.value else { return }
            voiceStatus = .unavailable(reason: voiceUnavailableMessage())
        }
    }

    public func startVoice(voice: String? = nil, voiceMode: String? = nil, languageCode: String? = nil) async {
        await startVoice(voice: voice, voiceMode: voiceMode, languageCode: languageCode, trigger: "menu_toggle")
    }

    private func startVoice(voice: String?, voiceMode: String?, languageCode: String?, trigger: String) async {
        guard voiceSession == nil, !voiceStartInProgress else { return }
        voiceStartInProgress = true
        voiceAttemptEndReason = nil
        voiceEventCensus = EventCensusDiagnostics()
        voiceInnerEventCensus = EventCensusDiagnostics(completeTypes: true)
        voiceActivityCoalescer = VoiceActivityCoalescer()
        censusEndRecorded = false
        censusEarlyRecorded = false
        censusNextReportElapsed = 60
        diagnosticConnectedAt = nil
        silence.reset()
        voiceStartedAt = Date()
        defer { voiceStartInProgress = false }
        let mediaEpoch = voiceMediaFence.beginAttempt()
        voiceStartMediaLoss = nil
        let generation: UInt64
        let api: APIClient
        do {
            api = try configure()
            generation = connectionGeneration.value
            voiceAPI = api
        } catch {
            voiceStatus = .unavailable(reason: voiceUnavailableMessage())
            return
        }

        var audio: VoiceAudioSessionAdapter?
        var transport: LibWebRTCTransport?
        var allocatedSession: VoiceSessionResponse?
        var diagnosticPhase = "config_request"
        voiceStatus = .connecting
        do {
            let config = try await api.voiceConfig()
            guard generation == connectionGeneration.value, mediaEpoch == voiceMediaEpoch else { throw CancellationError() }
            let pool = try await api.voicePoolStatus()
            guard generation == connectionGeneration.value, mediaEpoch == voiceMediaEpoch else { throw CancellationError() }
            voiceConfig = config
            voicePoolStatus = pool
            await retryPendingVoiceHangup(api: api)
            let refreshedPool = try await api.voicePoolStatus()
            voicePoolStatus = refreshedPool
            guard !refreshedPool.blocked else { throw ZoeError.http(status: 409, code: refreshedPool.reasonCode, message: "voice_pool_quarantined", details: nil) }

            audio = AppleDesktopAudioSessionAdapter()
            voiceAudioSession = audio
            audio?.onLifecycleEvent = { [weak self] event in
                api.recordDiagnostic(phase: "audio_activate", requestPath: "/v1/voice/audio", errorType: "audio_lifecycle", errorSummary: String(String(describing: event).prefix(64)))
                Task { @MainActor in
                    await self?.handleVoiceMediaLoss(.audio(event), mediaEpoch: mediaEpoch)
                }
            }
            diagnosticPhase = "audio_activate"
            try audio?.activate()
            let iceServers = config.webRTC.iceServers.map { RTCIceServer(urlStrings: $0.urls) }
            diagnosticPhase = "webrtc_offer"
            transport = try LibWebRTCTransport(iceServers: iceServers, dataChannelLabel: config.webRTC.dataChannel.label)
            voiceTransport = transport
            wireVoiceTransport(transport!, generation: generation, mediaEpoch: mediaEpoch)
            let offer = try await transport!.createOffer()
            guard generation == connectionGeneration.value, mediaEpoch == voiceMediaEpoch else { throw CancellationError() }
            diagnosticPhase = "session_create"
            await api.recordDiagnosticAndWait(phase: "voice_start", requestPath: "/v1/voice/sessions", errorSummary: trigger + silenceSettingsSuffix, level: .info, voiceMediaEpoch: mediaEpoch)
            let created = try await api.createVoiceSession(
                requestID: UUID().uuidString.lowercased(),
                offerSDP: offer,
                voice: voice ?? config.defaults.voice,
                voiceMode: voiceMode ?? config.defaults.voiceMode,
                languageCode: languageCode ?? config.defaults.languageCode
            )
            allocatedSession = created
            guard generation == connectionGeneration.value, mediaEpoch == voiceMediaEpoch else { throw CancellationError() }
            guard created.state == "active", !created.answerSdp.isEmpty else { throw ZoeError.invalidResponse }
            try await transport!.applyAnswer(created.answerSdp)
            guard generation == connectionGeneration.value, mediaEpoch == voiceMediaEpoch else { throw CancellationError() }
            try await transport!.waitUntilConnected()
            guard generation == connectionGeneration.value, mediaEpoch == voiceMediaEpoch else { throw CancellationError() }
            diagnosticPhase = "connected"
            let confirmed = try await api.confirmVoiceConnected(sessionID: created.voiceSessionId, generation: created.generation)
            guard generation == connectionGeneration.value, mediaEpoch == voiceMediaEpoch else { throw CancellationError() }
            guard confirmed.ok else { throw ZoeError.invalidResponse }

            voiceSession = created
            voiceSessionMediaEpoch = mediaEpoch
            voiceSessionMediaClosed = false
            voiceAPI = api
            voiceTurnID = nil
            voiceProviderMessageID = nil
            voiceDraft = ""
            voiceDraftRevision = 0
            voiceStatus = .active(sessionID: created.voiceSessionId, mediaGeneration: created.generation)
            noteVoiceConnected()
            await api.recordDiagnosticAndWait(phase: "voice_connected", requestPath: "/v1/voice/sessions/connected", level: .info, voiceSessionID: created.voiceSessionId, voiceGeneration: created.generation, voiceMediaEpoch: mediaEpoch)
            startVoiceHeartbeat(sessionID: created.voiceSessionId, api: api, generation: generation)
        } catch {
            let redacted = RedactedDiagnostic(error)
            api.recordDiagnostic(phase: diagnosticPhase, requestPath: "/v1/voice/\(diagnosticPhase)", httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: redacted.errorSummary)
            let attemptWasCurrent = mediaEpoch == voiceMediaEpoch
            await closeVoiceLocalMedia(mediaEpoch: mediaEpoch)
            var remainsQuarantined = false
            if let allocatedSession {
                deliverVoiceEnd(allocatedSession, api: api, mediaEpoch: mediaEpoch, reason: voiceAttemptEndReason ?? .setupFailed)
                let release = await releaseVoiceSession(allocatedSession, api: api, phase: "setup_release")
                remainsQuarantined = release == nil || release?.poolBlocked == true
                if remainsQuarantined {
                    let latestPool = try? await api.voicePoolStatus()
                    if generation == connectionGeneration.value, api === self.api { voicePoolStatus = latestPool }
                }
            } else {
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
                    await api.recordDiagnosticAndWait(phase: "voice_end", requestPath: "/v1/voice/sessions", errorType: reason.isError ? "voice_lifecycle" : "", errorSummary: summary, level: reason.isError ? .error : .info, voiceMediaEpoch: mediaEpoch)
                    api.triggerDiagnosticUpload()
                }
            }
            let mediaLossReason = voiceStartMediaLoss?.epoch == mediaEpoch ? voiceStartMediaLoss?.reason : nil
            if generation == connectionGeneration.value, (attemptWasCurrent || voiceStartMediaLoss?.epoch == mediaEpoch) {
                if remainsQuarantined, let allocatedSession {
                    voiceSession = allocatedSession
                    voiceSessionMediaEpoch = mediaEpoch
                    voiceSessionMediaClosed = true
                    voiceAPI = api
                    voiceStatus = .interrupted(reason: uiFormat("Local voice media is closed. Server hangup is unconfirmed or the account pool remains quarantined; no new session can start until the pool is cleared. %@", mediaLossReason ?? ""))
                } else {
                    voiceSession = nil
                    voiceSessionMediaEpoch = nil
                    voiceSessionMediaClosed = false
                    voiceAPI = nil
                    if let mediaLossReason {
                        voiceStatus = .interrupted(reason: uiFormat("%@ Local media is closed. The setup attempt was stopped; this does not prove upstream termination or cancel a task.", mediaLossReason))
                    } else {
                        voiceStatus = .unavailable(reason: voiceUnavailableMessage())
                    }
                }
            }
        }
    }

    public func endVoiceSession() async {
        await endVoiceSession(reason: .other)
    }

    private func endVoiceSession(reason: VoiceEndReason) async {
        guard let session = voiceSession, let api = voiceAPI, let mediaEpoch = voiceSessionMediaEpoch else {
            if voiceStartInProgress {
                voiceAttemptEndReason = reason
                voiceStartMediaLoss = (voiceMediaEpoch, uiText("Voice setup was stopped locally."))
            }
            let closingEpoch = voiceMediaEpoch
            await closeVoiceLocalMedia(mediaEpoch: closingEpoch)
            guard closingEpoch == voiceMediaEpoch else { return }
            voiceSession = nil
            voiceSessionMediaEpoch = nil
            voiceSessionMediaClosed = false
            voiceAPI = nil
            if case .unavailable = voiceStatus {} else { voiceStatus = .stopped }
            return
        }
        await finishVoiceSession(session, api: api, mediaEpoch: mediaEpoch, lossReason: nil, reason: reason)
    }

    private func deliverVoiceEnd(_ session: VoiceSessionResponse, api: APIClient, mediaEpoch: UInt64, reason: VoiceEndReason, duration: TimeInterval? = nil) {
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
            await api.recordDiagnosticAndWait(phase: "voice_end", requestPath: "/v1/voice/sessions/hangup", errorType: reason.isError ? "voice_lifecycle" : "", errorSummary: summary, level: reason.isError ? .error : .info, voiceSessionID: session.voiceSessionId, voiceGeneration: session.generation, voiceMediaEpoch: mediaEpoch)
            api.triggerDiagnosticUpload()
        }
    }

    private func finishVoiceSession(_ session: VoiceSessionResponse, api: APIClient, mediaEpoch: UInt64, lossReason: String?, reason: VoiceEndReason) async {
        guard voiceSession?.voiceSessionId == session.voiceSessionId,
              voiceSessionMediaEpoch == mediaEpoch,
              voiceHangupInFlightSessionID != session.voiceSessionId else { return }
        let duration = voiceStartedAt.map { Date().timeIntervalSince($0) } ?? 0
        if let lossReason {
            voiceStatus = .interrupted(reason: uiFormat("%@ Voice controls are stopped while local media closes and server release is checked.", lossReason))
        }
        if !voiceSessionMediaClosed {
            guard voiceMediaEpoch == mediaEpoch else { return }
            await closeVoiceLocalMedia(mediaEpoch: mediaEpoch)
            guard voiceSession?.voiceSessionId == session.voiceSessionId,
                  voiceSessionMediaEpoch == mediaEpoch else { return }
            voiceSessionMediaClosed = true
        }
        voiceHangupInFlightSessionID = session.voiceSessionId
        deliverVoiceEnd(session, api: api, mediaEpoch: mediaEpoch, reason: reason, duration: duration)
        savePendingVoiceHangup(session, api: api)
        defer {
            if voiceHangupInFlightSessionID == session.voiceSessionId {
                voiceHangupInFlightSessionID = nil
            }
        }
        do {
            let result: VoiceOperationResponse
            #if DEBUG
            if let voiceHangupOverrideForTesting {
                result = try await voiceHangupOverrideForTesting(session.voiceSessionId)
            } else {
                result = try await api.hangupVoiceSession(session)
            }
            #else
            result = try await api.hangupVoiceSession(session)
            #endif
            if result.poolBlocked != true { defaults.removeObject(forKey: pendingVoiceHangupKey) }
            else {
                api.recordDiagnostic(phase: "voice_hangup", requestPath: "/v1/voice/sessions/hangup", errorType: "pool_blocked", errorSummary: "release_unconfirmed")
                scheduleVoiceHangupRetry(session, api: api, phase: "hangup")
            }
            guard voiceSession?.voiceSessionId == session.voiceSessionId, voiceSessionMediaEpoch == mediaEpoch else { return }
            let latestPool = result.poolBlocked == true ? try? await api.voicePoolStatus() : nil
            guard voiceSession?.voiceSessionId == session.voiceSessionId, voiceSessionMediaEpoch == mediaEpoch else { return }
            if result.poolBlocked == true {
                voicePoolStatus = latestPool
                voiceSessionMediaClosed = true
                voiceStatus = .interrupted(reason: lossReason.map { uiFormat("%@ Local media is closed. Upstream termination is unverified; the account pool remains quarantined. Retry server hangup to repeat the idempotent lease release.", $0) } ?? uiText("Local media is closed. Upstream termination is unverified; the account pool remains quarantined. Retry server hangup to repeat the idempotent lease release."))
            } else if let lossReason {
                voiceStatus = .interrupted(reason: uiFormat("%@ Local media is closed and the server confirmed release of the voice lease. This did not cancel a task.", lossReason))
                voiceSession = nil
                voiceSessionMediaEpoch = nil
                voiceSessionMediaClosed = false
                voiceAPI = nil
            } else {
                voiceStatus = .idle
                voiceSession = nil
                voiceSessionMediaEpoch = nil
                voiceSessionMediaClosed = false
                voiceAPI = nil
            }
        } catch {
            guard voiceSession?.voiceSessionId == session.voiceSessionId, voiceSessionMediaEpoch == mediaEpoch else { return }
            savePendingVoiceHangup(session, api: api)
            let redacted = RedactedDiagnostic(error)
            api.recordDiagnostic(phase: "voice_hangup", requestPath: "/v1/voice/sessions/hangup", httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: redacted.errorSummary)
            scheduleVoiceHangupRetry(session, api: api, phase: "hangup")
            voiceStatus = .interrupted(reason: uiFormat("Local media is closed, but server hangup is unconfirmed; the session remains fenced. %@%@", lossReason.map { $0 + " " } ?? "", ""))
        }
    }

    private struct PendingVoiceHangup: Codable {
        let serverURL: String
        let session: VoiceSessionResponse
    }

    private func savePendingVoiceHangup(_ session: VoiceSessionResponse, api: APIClient) {
        let pending = PendingVoiceHangup(serverURL: api.serverURL.absoluteString, session: session)
        if let data = try? JSONEncoder().encode(pending) { defaults.set(data, forKey: pendingVoiceHangupKey) }
    }

    private func pendingVoiceHangup() -> PendingVoiceHangup? {
        guard let data = defaults.data(forKey: pendingVoiceHangupKey) else { return nil }
        return try? JSONDecoder().decode(PendingVoiceHangup.self, from: data)
    }

    private func releaseVoiceSession(_ session: VoiceSessionResponse, api: APIClient, phase: String) async -> VoiceOperationResponse? {
        savePendingVoiceHangup(session, api: api)
        do {
            let result = try await api.hangupVoiceSession(session)
            if result.poolBlocked != true { defaults.removeObject(forKey: pendingVoiceHangupKey) }
            else {
                api.recordDiagnostic(phase: "voice_\(phase)", requestPath: "/v1/voice/sessions/hangup", errorType: "pool_blocked", errorSummary: "release_unconfirmed")
                scheduleVoiceHangupRetry(session, api: api, phase: phase)
            }
            return result
        } catch {
            let redacted = RedactedDiagnostic(error)
            api.recordDiagnostic(phase: "voice_\(phase)", requestPath: "/v1/voice/sessions/hangup", httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: redacted.errorSummary)
            scheduleVoiceHangupRetry(session, api: api, phase: phase)
            return nil
        }
    }

    private func scheduleVoiceHangupRetry(_ session: VoiceSessionResponse, api: APIClient, phase: String) {
        guard voiceHangupRetryTask == nil else { return }
        voiceHangupRetryTask = Task { [weak self] in
            for delay in [1, 2, 4] {
                try? await Task.sleep(for: .seconds(delay))
                guard !Task.isCancelled else { return }
                do {
                    let result = try await api.hangupVoiceSession(session)
                    if result.poolBlocked != true {
                        self?.defaults.removeObject(forKey: self?.pendingVoiceHangupKey ?? "")
                        self?.voiceHangupRetryTask = nil
                        return
                    }
                } catch {
                    let redacted = RedactedDiagnostic(error)
                    api.recordDiagnostic(phase: "voice_\(phase)_retry", requestPath: "/v1/voice/sessions/hangup", httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: redacted.errorSummary)
                }
            }
            await MainActor.run { self?.voiceHangupRetryTask = nil }
        }
    }

    private func retryPendingVoiceHangup(api: APIClient) async {
        guard let pending = pendingVoiceHangup(), pending.serverURL == api.serverURL.absoluteString else { return }
        _ = await releaseVoiceSession(pending.session, api: api, phase: "pending_release")
    }

    #if os(macOS)
    private func terminateVoiceForAppExit() async {
        guard let session = voiceSession, let api = voiceAPI, let epoch = voiceSessionMediaEpoch else { return }
        deliverVoiceEnd(session, api: api, mediaEpoch: epoch, reason: .appTerminate)
        await closeVoiceLocalMedia(mediaEpoch: epoch)
        await withTaskGroup(of: Void.self) { group in
            group.addTask { _ = await self.releaseVoiceSession(session, api: api, phase: "app_terminate") }
            group.addTask {
                try? await Task.sleep(for: .seconds(2))
            }
            await group.next()
            group.cancelAll()
        }
    }
    #endif

    private func closeVoiceLocalMedia(mediaEpoch: UInt64) async {
        guard voiceMediaFence.stop(mediaEpoch) else { return }
        silence.reset()
        voiceHeartbeat.cancel()
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
        await transport?.close()
        try? audio?.deactivate()
    }

    private func handleVoiceMediaLoss(_ loss: VoiceMediaLoss, mediaEpoch: UInt64, endReason: VoiceEndReason? = nil) async {
        guard mediaEpoch == voiceMediaEpoch else { return }
        guard let rawReason = loss.message else { return }
        let reason: String
        if case let .leaseRenewalFailed(message) = loss {
            reason = uiFormat("The voice lease heartbeat failed: %@", message)
        } else {
            reason = uiText(rawReason)
        }
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
            case .leaseRenewalFailed: ("lease", "lease_renewal_failed")
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
        if let session = voiceSession, let api = voiceAPI, voiceSessionMediaEpoch == mediaEpoch {
            if continuousVoiceEnabled {
                voiceStatus = .reconnecting(reason: reason)
                #if DEBUG
                voiceStatusObserverForTesting?(voiceStatus)
                #endif
            }
            await finishVoiceSession(session, api: api, mediaEpoch: mediaEpoch, lossReason: reason, reason: endReason ?? loss.diagnosticReason)
            if continuousVoiceEnabled {
                Task { @MainActor [weak self] in
                    try? await Task.sleep(for: .seconds(2))
                    await self?.startVoiceIfEnabled()
                }
            }
            return
        }
        voiceStartMediaLoss = (mediaEpoch, reason)
        voiceAttemptEndReason = endReason ?? loss.diagnosticReason
        voiceStatus = .interrupted(reason: uiFormat("%@ Stopping voice setup and releasing local media.", reason))
        await closeVoiceLocalMedia(mediaEpoch: mediaEpoch)
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
        if let api, api.serverURL == url, coordinator != nil { return api }
        let store = try keyStore(url: url)
        guard let key = try store.load() else { throw ZoeError.missingCredential }
        if configuredServerURL != url {
            lockWorkspace(clearDrafts: true)
            connectionGeneration.switchOrigin(to: url)
            connectionEpoch = connectionGeneration.value
            configuredServerURL = url
            voiceConfig = nil
            voicePoolStatus = nil
            voiceDraft = ""
            voiceDraftRevision = 0
            voiceTurnID = nil
            voiceProviderMessageID = nil
        }
        let api = try connectionAPI(url: url, key: key)
        self.identity = store
        self.api = api
        // Physical installation identity must never be derived from the shared instance key.
        let scope = try key.bearerToken()
        let journal = try CommandJournal(serverURL: url, credentialScope: scope)
        self.journal = journal
        coordinator = CommandCoordinator(api: api, journal: journal)
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

    private func startVoiceHeartbeat(sessionID: String, api: APIClient, generation: UInt64) {
        guard let mediaEpoch = voiceSessionMediaEpoch, voiceMediaEpoch == mediaEpoch else { return }
        let task = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(15))
                if Task.isCancelled { return }
                do {
                    _ = try await api.heartbeatVoiceSession(sessionID: sessionID)
                } catch {
                    guard let self,
                          !Task.isCancelled,
                          generation == self.connectionGeneration.value,
                          self.voiceAPI === api,
                          self.voiceSession?.voiceSessionId == sessionID,
                          self.voiceSessionMediaEpoch == mediaEpoch,
                          self.voiceMediaEpoch == mediaEpoch else { return }
                    await self.handleVoiceHeartbeatFailure(
                        sessionID: sessionID,
                        api: api,
                        mediaEpoch: mediaEpoch,
                        generation: generation,
                        message: self.userMessage(error)
                    )
                    return
                }
            }
        }
        voiceHeartbeat.install(task)
    }

    private func handleVoiceHeartbeatFailure(sessionID: String, api: APIClient, mediaEpoch: UInt64, generation: UInt64, message: String) async {
        guard !Task.isCancelled,
              generation == connectionGeneration.value,
              voiceAPI === api,
              voiceSession?.voiceSessionId == sessionID,
              voiceSessionMediaEpoch == mediaEpoch,
              voiceMediaEpoch == mediaEpoch else { return }
        api.recordDiagnostic(phase: "voice_heartbeat", requestPath: "/v1/voice/sessions/heartbeat", errorType: "heartbeat_failure", errorSummary: "heartbeat_failure", level: .error, voiceSessionID: sessionID, voiceGeneration: voiceSession?.generation, voiceMediaEpoch: mediaEpoch)
        await handleVoiceMediaLoss(.leaseRenewalFailed(message), mediaEpoch: mediaEpoch, endReason: .heartbeatFailure)
    }

    private func receiveVoiceData(_ data: Data, generation: UInt64, mediaEpoch: UInt64) {
        guard generation == connectionGeneration.value,
              mediaEpoch == voiceMediaEpoch,
              voiceSessionMediaEpoch == mediaEpoch,
              voiceSession != nil else { return }
        guard let outer = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { return }
        var event = outer
        if outer["type"] as? String == "data_message", let encoded = outer["data"] as? String,
           let innerData = encoded.data(using: .utf8), let inner = (try? JSONSerialization.jsonObject(with: innerData)) as? [String: Any] {
            event = inner
        }
        let type = (event["type"] as? String) ?? ""
        let activity = Self.isVoiceActivity(event)
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
        if type == "user_audio_turn.final" {
            guard let final = Self.parseAuthoritativeVoiceTurn(event) else {
                recordAutomaticVoiceTurnDiagnostic(errorType: "contract_rejected", errorSummary: "invalid_final_event", mediaEpoch: mediaEpoch)
                return
            }
            Task { @MainActor [weak self] in
                await self?.processAuthoritativeVoiceTurn(final, generation: generation, mediaEpoch: mediaEpoch)
            }
            return
        }
        if type == "voice.end" || type == "voice_end" ||
            ((type == "function_call" || type == "tool_call") && ((event["name"] as? String) == "voice.end" || (event["name"] as? String) == "voice_end")) {
            Task { @MainActor [weak self] in await self?.endVoiceSession(reason: .voiceEndEvent) }
            return
        }
        var messageID: String?
        var transcript: String?
        if type == "chat_message_delta" {
            let payload = (event["payload"] as? [String: Any]) ?? event
            let delta = (payload["delta"] as? [String: Any]) ?? [:]
            let version = delta["v"] as? [String: Any]
            let message = version?["message"] as? [String: Any]
            let author = message?["author"] as? [String: Any]
            guard author?["role"] as? String == "user" else { return }
            messageID = message?["id"] as? String
            if let content = message?["content"] as? [String: Any], let parts = content["parts"] as? [[String: Any]] {
                transcript = parts.compactMap { part -> String? in
                    if part["content_type"] as? String == "audio_transcription", part["direction"] as? String == "out" { return nil }
                    return (part["transcript"] as? String) ?? (part["text"] as? String) ?? (part["content"] as? String)
                }.joined(separator: " ")
            }
        } else if type.contains("input_audio") && type.contains("transcription") {
            let payload = (event["payload"] as? [String: Any]) ?? event
            transcript = (event["transcript"] as? String) ?? (event["text"] as? String) ?? (payload["transcript"] as? String) ?? (payload["text"] as? String)
            messageID = (event["item_id"] as? String) ?? (payload["item_id"] as? String)
        }
        guard let transcript, !transcript.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { return }
        let clean = transcript.trimmingCharacters(in: .whitespacesAndNewlines)
        if let messageID, messageID != voiceProviderMessageID {
            voiceProviderMessageID = messageID
            voiceTurnID = UUID().uuidString.lowercased()
            voiceDraftRevision = 0
        } else if voiceTurnID == nil {
            voiceTurnID = UUID().uuidString.lowercased()
        }
        guard voiceDraft != clean else { return }
        voiceDraft = clean
        voiceDraftRevision += 1
    }

    static func isVoiceActivity(_ event: [String: Any]) -> Bool {
        let type = event["type"] as? String ?? ""
        if type == "user_audio_turn.final" { return parseAuthoritativeVoiceTurn(event) != nil }
        if VoiceActivityClassifier.isActivity(type: type) { return true }
        let payload = (event["payload"] as? [String: Any]) ?? event
        if type.contains("input_audio") && type.contains("transcription") {
            let text = (event["transcript"] as? String) ?? (event["text"] as? String) ?? (payload["transcript"] as? String) ?? (payload["text"] as? String)
            return text?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false
        }
        guard type == "chat_message_delta",
              let delta = payload["delta"] as? [String: Any],
              let version = delta["v"] as? [String: Any],
              let message = version["message"] as? [String: Any],
              let author = message["author"] as? [String: Any],
              let role = author["role"] as? String, role == "user" || role == "assistant",
              let content = message["content"] as? [String: Any],
              let parts = content["parts"] as? [[String: Any]] else { return false }
        return parts.contains { part in
            if role == "user", part["content_type"] as? String == "audio_transcription", part["direction"] as? String == "out" { return false }
            let text = (part["transcript"] as? String) ?? (part["text"] as? String) ?? (part["content"] as? String)
            return text?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false
        }
    }

    private struct AuthoritativeVoiceTurn: Sendable {
        let turnID: String
        let revision: Int64
        let transcript: String
    }

    private static func parseAuthoritativeVoiceTurn(_ event: [String: Any]) -> AuthoritativeVoiceTurn? {
        guard event.keys.contains("type"), event.keys.contains("turn_id"), event.keys.contains("revision"), event.keys.contains("transcript"),
              event["type"] as? String == "user_audio_turn.final",
              let turnID = event["turn_id"] as? String,
              let revisionNumber = event["revision"] as? NSNumber,
              CFGetTypeID(revisionNumber) != CFBooleanGetTypeID(),
              !CFNumberIsFloatType(revisionNumber),
              let transcript = event["transcript"] as? String else { return nil }
        let cleanTurnID = turnID.trimmingCharacters(in: .whitespacesAndNewlines)
        let cleanTranscript = transcript.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !cleanTurnID.isEmpty, cleanTurnID.count <= 256,
              revisionNumber.int64Value >= 1,
              cleanTranscript.count <= 20_000, !cleanTranscript.isEmpty else { return nil }
        return AuthoritativeVoiceTurn(turnID: cleanTurnID, revision: revisionNumber.int64Value, transcript: cleanTranscript)
    }

    private func processAuthoritativeVoiceTurn(_ final: AuthoritativeVoiceTurn, generation: UInt64, mediaEpoch: UInt64) async {
        guard generation == connectionGeneration.value,
              mediaEpoch == voiceMediaEpoch,
              voiceSessionMediaEpoch == mediaEpoch,
              case .active = voiceStatus,
              let session = voiceSession,
              let sourceAPI = voiceAPI,
              let coordinator else {
            recordAutomaticVoiceTurnDiagnostic(errorType: "contract_rejected", errorSummary: "stale_voice_epoch", mediaEpoch: mediaEpoch)
            return
        }
        let contentHash = SHA256.hash(data: Data(final.transcript.utf8)).map { String(format: "%02x", $0) }.joined()
        let identity = "\(session.voiceSessionId)|\(session.generation)|\(final.turnID)|\(final.revision)|\(contentHash)"
        guard !automaticVoiceTurnCompleted.contains(identity), automaticVoiceTurnInFlight.insert(identity).inserted else { return }
        defer { automaticVoiceTurnInFlight.remove(identity) }

        let workspaceID = self.workspaceID
        let prefix = SHA256.hash(data: Data(identity.utf8)).map { String(format: "%02x", $0) }.joined()
        let commitKey = "voice-final-commit-\(prefix)"
        let handoffKey = "voice-final-handoff-\(prefix)"
        let providerMessageID = voiceProviderMessageID
        voiceDraft = final.transcript
        voiceDraftRevision = final.revision
        voiceTurnID = final.turnID
        do {
            var fields: [String: JSONValue] = [
                "session_id": .string(session.voiceSessionId),
                "generation": .integer(session.generation),
                "turn_id": .string(final.turnID),
                "revision": .integer(final.revision),
                "content": .string(final.transcript),
                "user_confirmed": .bool(true)
            ]
            if let providerMessageID { fields["provider_message_id"] = .string(providerMessageID) }
            _ = try await coordinator.submit(kind: "voice.turn.commit", payload: .object(fields), workspaceID: workspaceID, idempotencyKey: commitKey)
            guard isCurrentConnection(generation, sourceAPI: sourceAPI), mediaEpoch == voiceMediaEpoch,
                  voiceSession?.voiceSessionId == session.voiceSessionId, voiceSessionMediaEpoch == mediaEpoch else { return }
            let handoff: JSONValue = .object([
                "session_id": .string(session.voiceSessionId),
                "generation": .integer(session.generation),
                "turn_id": .string(final.turnID),
                "revision": .integer(final.revision),
                "content_hash": .string(contentHash)
            ])
            _ = try await coordinator.submit(kind: "voice.turn.handoff", payload: handoff, workspaceID: workspaceID, idempotencyKey: handoffKey)
            guard isCurrentConnection(generation, sourceAPI: sourceAPI), mediaEpoch == voiceMediaEpoch else { return }
            automaticVoiceTurnCompleted.insert(identity)
            if automaticVoiceTurnCompleted.count > automaticVoiceTurnSetLimit {
                automaticVoiceTurnCompleted.remove(automaticVoiceTurnCompleted.first!)
            }
            voiceDraft = ""
            voiceTurnID = nil
            voiceProviderMessageID = nil
            voiceDraftRevision = 0
            notice = uiText("Voice turn handed off to Zoe.")
        } catch {
            recordAutomaticVoiceTurnDiagnostic(errorType: "processing_failed", errorSummary: RedactedDiagnostic(error).errorSummary, mediaEpoch: mediaEpoch)
            notice = uiText("The voice turn could not be handed off; the draft was preserved.")
        }
    }

    private func recordAutomaticVoiceTurnDiagnostic(errorType: String, errorSummary: String, mediaEpoch: UInt64) {
        guard let api = voiceAPI else { return }
        let session = voiceSession
        Task { await api.recordDiagnosticAndWait(
            phase: "voice_audio_turn_final",
            requestPath: "/v1/voice/media",
            errorType: String(errorType.prefix(64)),
            errorSummary: String(errorSummary.prefix(128)),
            voiceSessionID: session?.voiceSessionId,
            voiceGeneration: session?.generation,
            voiceMediaEpoch: mediaEpoch
        ) }
    }

    private var workspaceID: String?
    private var eventCursor: String?
    public let silence: SilenceController
    private var silenceTask: Task<Void, Never>?

    private func validateConnection() async throws {
        guard let api else { throw ZoeError.missingCredential }
        let generation = connectionGeneration.value
        let snapshot = try await api.snapshot()
        guard isCurrentConnection(generation, sourceAPI: api) else { return }
        workspaceID = snapshot.workspaceId
        eventCursor = snapshot.eventCursor
    }

    private func listen(using api: APIClient) async {
        let generation = connectionGeneration.value
        let stream = EventStreamClient(api: api)
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
                _ = try await stream.run(after: eventCursor, onConnected: {
                    await api.recordDiagnosticAndWait(phase: "events_stream", requestPath: "/v1/events", errorSummary: "connected", level: .info)
                }) { [weak self] push in
                    await self?.receiveStreamPush(push, diagnostics: diagnostics, generation: generation, api: api)
                }
                streamReportTask.cancel()
                diagnostics.report(api: api, force: true)
                api.recordDiagnostic(phase: "events_stream", requestPath: "/v1/events", errorSummary: "ended;dur=\(DiagnosticSummaries.seconds(Date().timeIntervalSince(started)))", level: .info)
                if Task.isCancelled || !isCurrentConnection(generation, sourceAPI: api) { return }
                closeUISurfaces()
                streamBackoff = 1
            } catch {
                streamReportTask.cancel()
                diagnostics.report(api: api, force: true)
                let redacted = RedactedDiagnostic(error)
                api.recordDiagnostic(phase: "events_stream", requestPath: "/v1/events", httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: "error;\(redacted.errorType);dur=\(DiagnosticSummaries.seconds(Date().timeIntervalSince(started)))", level: .info)
                if Task.isCancelled || !isCurrentConnection(generation, sourceAPI: api) { return }
                closeUISurfaces()
                if Self.isAccessDenied(error) {
                    lockWorkspace(clearDrafts: false)
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
            eventCursor = envelope.cursor
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
                uiSurfaceRequests.apply(event)
                let displayed = onUISurface?(event) ?? false
                if event.isDeletion { recordUITrace(phase: "ui_surface_closed", summary: "server_delete", level: .info) }
                if !event.isDeletion {
                    recordUITrace(phase: displayed ? "ui_surface_displayed" : "ui_surface_display_failed", summary: event.diagnosticSummary, level: displayed ? .info : .warn, upload: !displayed)
                }
                if displayed, !event.isDeletion {
                    await submitUICommand(kind: "ui.rendered", payload: event.renderedPayload,
                                          key: event.renderedIdempotencyKey(cursor: envelope.cursor, version: envelope.aggregateVersion))
                }
            }
            if envelope.type == "voice.end" { await endVoiceSession(reason: .voiceEndEvent) }
        case .snapshotRequired:
            do { try await validateConnection() }
            catch { setOffline(error) }
        }
    }

    private func isCurrentConnection(_ generation: UInt64, sourceAPI: APIClient?) -> Bool {
        generation == connectionGeneration.value && (sourceAPI == nil || sourceAPI === api)
    }

    private func closeUISurfaces() {
        let ids = Array(uiSurfaceRequests.requestIDs.keys)
        for _ in ids { recordUITrace(phase: "ui_surface_closed", summary: "disconnect", level: .info) }
        uiSurfaceRequests.removeAll()
        onUISurfacesClosed?(ids)
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

    public func sendUISurfaceAction(surfaceID: String, action: ResolvedAction) {
        guard authentication.isConnected, let requestID = uiSurfaceRequests.requestIDs[surfaceID] else { return }
        do {
            let payload = try uiActionPayload(requestID: requestID, surfaceID: surfaceID, action: action)
            let generation = connectionGeneration.value
            let sourceAPI = api
            Task { @MainActor [weak self] in
                guard let self, self.isCurrentConnection(generation, sourceAPI: sourceAPI),
                      self.uiSurfaceRequests.requestIDs[surfaceID] == requestID else { return }
                await self.submitUICommand(kind: "ui.action", payload: payload, key: UUID().uuidString.lowercased())
            }
        } catch {
            api?.recordDiagnostic(phase: "ui_action", requestPath: "/v1/commands", errorType: "encoding_error", errorSummary: "invalid_action_context", level: .warn)
        }
    }

    private func submitUICommand(kind: String, payload: JSONValue, key: String) async {
        guard authentication.isConnected, let api else { return }
        do {
            // UI commands are transient: submit once, never journal/retry failed actions.
            _ = try await api.submit(kind: kind, payload: payload, idempotencyKey: key)
        } catch {
            let diagnostic = RedactedDiagnostic(error)
            api.recordDiagnostic(phase: "ui_command", requestPath: "/v1/commands", httpStatus: diagnostic.httpStatus,
                                 errorType: diagnostic.errorType, errorSummary: diagnostic.errorSummary, level: .warn)
            if kind == "ui.rendered" {
                api.recordDiagnostic(phase: "ui_rendered_ack", requestPath: "/v1/commands", httpStatus: diagnostic.httpStatus, errorType: diagnostic.errorType, errorSummary: diagnostic.errorSummary, level: .error)
            }
        }
    }

    private static func isAccessDenied(_ error: Error) -> Bool {
        guard let failure = error as? ZoeError,
              case let .http(status, code, _, _) = failure else { return false }
        return status == 401 || (status == 403 && code == "authentication_required")
    }

    private func setOffline(_ error: Error) {
        if Self.isAccessDenied(error), authentication.isConnected {
            lockWorkspace(clearDrafts: false)
            authentication = .disconnected
        }
        let message = userMessage(error)
        notice = message
    }

    private func userMessage(_ error: Error) -> String {
        reportClientError(error, code: "client_error")
        if Self.isAccessDenied(error) { return uiText("The instance key was rejected. Replace it with the current key from your server.") }
        if error is ConnectionInputError { return uiText(error.localizedDescription) }
        if let failure = error as? ZoeError, case .missingCredential = failure {
            return uiText("Enter your server address and instance key to connect.")
        }
        return uiText("Something went wrong. Try again.")
    }

    private func reportClientError(_ error: Error, code: String) {
        let redacted = RedactedDiagnostic(error)
        api?.recordDiagnostic(phase: "client_error", requestPath: "client", httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: "\(code):\(redacted.errorSummary)")
    }

    private func voiceUnavailableMessage() -> String {
        uiText("Voice is unavailable right now.")
    }

}
