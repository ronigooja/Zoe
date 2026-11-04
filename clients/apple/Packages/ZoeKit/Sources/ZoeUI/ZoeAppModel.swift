import Combine
import CryptoKit
import ZoeKit
import Foundation
import WebRTC
#if os(macOS)
import AppKit
#endif

public enum TaskSubmissionResult: Equatable, Sendable {
    case accepted(CommandReceipt)
    case failed(String)
    case unknown(idempotencyKey: String, message: String)
}

@MainActor
public final class ZoeAppModel: ObservableObject {
    @Published public internal(set) var serverAddress: String
    @Published public private(set) var isConnecting = false
    @Published public private(set) var snapshot: Snapshot?
    @Published public private(set) var freshness: ClientFreshness = .loading
    @Published public private(set) var notice: String?
    @Published public private(set) var isBusy = false
    @Published public private(set) var authentication: InstanceConnectionState = .checking
    @Published public private(set) var pendingConnectionImport: InstallationConnection?
    @Published public private(set) var connectionImportError: String?
    private let installationID: String
    private var connectionCleanupWarning: String?

    @Published public private(set) var memoryClaims: [MemoryClaim] = []
    @Published public private(set) var memoryMissing: [String] = []
    @Published public private(set) var memoryConflicts: [String] = []
    @Published public private(set) var notifications: [NotificationRecord] = []
    @Published public private(set) var pushNotificationsEnabled: Bool
    @Published public private(set) var capabilityReleases: [CapabilityRelease] = []
    @Published public private(set) var capabilityDeployments: [CapabilityDeployment] = []
    @Published public private(set) var connectionEpoch: UInt64 = 0
    @Published public private(set) var voiceConfig: VoiceConfig?
    @Published public private(set) var voicePoolStatus: VoicePoolStatus?
    @Published public private(set) var voiceStatus: VoiceIntegrationStatus = .idle
    @Published public private(set) var continuousVoiceEnabled: Bool
    @Published public var voiceDraft = ""
    @Published public private(set) var voiceDraftRevision: Int64 = 0
    @Published public private(set) var exportArtifact: ScopedExportArtifact?
    @Published public private(set) var exportStatus: String?
    private var exportArtifactConnectionEpoch: UInt64?
    @Published public private(set) var pendingCommandSummaries: [PendingCommandSummary] = []
    @Published public private(set) var selectedTaskID: String?
    @Published public private(set) var taskEntries: [TaskEntry] = []
    @Published public private(set) var isLoadingTaskEntries = false
    @Published public private(set) var taskEntriesError: String?
    @Published public private(set) var taskDrafts: [String: String] = [:]
    @Published public private(set) var newTaskDraft = ""
    @Published public private(set) var hasMoreTaskEntries = false
    private var taskEntriesCursor: String?
    private var taskEntriesRequestID: UUID?
    private struct TaskIntent {
        let key: String
    }
    private var taskIntents: [String: TaskIntent] = [:]

    private let defaults: UserDefaults
    private let continuousVoiceKey = "zoe.voice.continuous"
    private let pendingVoiceHangupKey = "zoe.voice.pendingHangup"
    private var api: APIClient?
    private var identity: (any InstanceKeyStoring)?
    private var journal: CommandJournal?
    private var coordinator: CommandCoordinator?
    private var eventTask: Task<Void, Never>?
    private var syncLoop: ClientSyncLoop?
    private var snapshotCache: SnapshotCache?
    private var streamBackoff: UInt64 = 1
    private var connectionGeneration = ConnectionGeneration()
    private var exportDownloadTask: Task<ScopedExportArtifact, Error>?
    private var exportRequestID: UUID?
    private var memoryRefreshSequence: UInt64 = 0
    private var memoryRefreshInFlight = false
    private var pendingMemoryRefreshQuery: String?
    private var commandTitleByKey: [String: String] = [:]
    private var commandStatusByKey: [String: PendingCommandSummary.State] = [:]
#if DEBUG
    var connectionAPIOverrideForTesting: ((URL, InstanceKey, String) throws -> APIClient)?
    var keyStoreOverrideForTesting: ((URL) throws -> any InstanceKeyStoring)?
    var suppressConnectionFollowupsForTesting = false
    var capabilityRevocationSubmitOverrideForTesting: ((Int64, JSONValue) async -> Bool)?
    var capabilityRevocationRefreshOverrideForTesting: (() async -> Void)?
    var exportDownloadOverrideForTesting: ((String) async throws -> ScopedExportArtifact)?
    var memorySearchOverrideForTesting: ((String) async throws -> MemorySearchResult)?
    var taskEntriesFetchOverrideForTesting: ((String, String?) async throws -> TaskEntriesPage)?
    var snapshotFetchOverrideForTesting: (() async throws -> Snapshot)?
    var pendingCommandReadOverrideForTesting: (() async throws -> [PendingCommand])?
    var voiceLocalMediaStopObserverForTesting: (() -> Void)?
    var voiceStatusObserverForTesting: ((VoiceIntegrationStatus) -> Void)?
    var voiceHangupOverrideForTesting: ((String) async throws -> VoiceOperationResponse)?

    func setCapabilityDeploymentsForTesting(_ deployments: [CapabilityDeployment]) {
        capabilityDeployments = deployments
    }

    func setExportArtifactForTesting(_ artifact: ScopedExportArtifact?) {
        exportArtifact = artifact
        exportArtifactConnectionEpoch = artifact == nil ? nil : connectionGeneration.value
    }

    func setMemoryClaimsForTesting(_ claims: [MemoryClaim]) {
        memoryClaims = claims
    }

    func handleEventForTesting(_ envelope: EventEnvelope, expectedConnectionEpoch: UInt64? = nil) async {
        await handle(.event(envelope), expectedGeneration: expectedConnectionEpoch ?? connectionGeneration.value, sourceAPI: nil)
    }

    func refreshSnapshotForTesting() async throws {
        try await refreshSnapshot()
    }

    func recoverPendingCommandsForTesting(api: APIClient, journal: CommandJournal, expectedGeneration: UInt64? = nil, workspaceID: String? = "workspace-1") async {
        self.api = api
        self.journal = journal
        let testCoordinator = CommandCoordinator(api: api, journal: journal)
        coordinator = testCoordinator
        await recoverPendingCommands(expectedGeneration: expectedGeneration ?? connectionGeneration.value, sourceAPI: api, coordinator: testCoordinator, workspaceID: workspaceID)
    }

    func preparePendingCheckForTesting(api: APIClient, journal: CommandJournal, snapshot: Snapshot) async {
        self.api = api
        self.journal = journal
        coordinator = CommandCoordinator(api: api, journal: journal)
        self.snapshot = snapshot
        snapshotFetchOverrideForTesting = { snapshot }
        await reloadPendingCommandSummaries()
    }

    func replacePendingCheckConnectionForTesting(api: APIClient, journal: CommandJournal) {
        connectionGeneration.advance()
        connectionEpoch = connectionGeneration.value
        pendingCommandSummaries = []
        self.api = api
        self.journal = journal
        coordinator = CommandCoordinator(api: api, journal: journal)
    }

    func reloadPendingCommandSummariesForTesting() async {
        await reloadPendingCommandSummaries()
    }

    func setAuthenticationForTesting(_ state: InstanceConnectionState) {
        authentication = state
    }

    func setSnapshotForTesting(_ snapshot: Snapshot?) {
        self.snapshot = snapshot
    }

    @discardableResult
    func installVoiceSessionForTesting(_ session: VoiceSessionResponse, api: APIClient) -> UInt64 {
        let epoch = voiceMediaFence.beginAttempt()
        voiceSession = session
        voiceSessionMediaEpoch = epoch
        voiceSessionMediaClosed = false
        voiceAPI = api
        voiceStatus = .active(sessionID: session.voiceSessionId, mediaGeneration: session.generation)
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
    private var voiceHangupInFlightSessionID: String?
    private var voiceAPI: APIClient?
    private var recordedVoiceMediaLosses = Set<String>()
    private var voiceHangupRetryTask: Task<Void, Never>?
    #if os(macOS)
    private var terminationObserver: NSObjectProtocol?
    #endif
    private var voiceTurnID: String?
    private var voiceProviderMessageID: String?
    private let voiceHeartbeat = VoiceHeartbeatLease()
    private var hasLoadedNotificationInbox = false
    private var pendingAPNSTokenHex: String?
    private var lastRegisteredAPNSTokenHex: String?
    private var tokenRegistrationInFlight: String?
    private var registeredPushTokenID: String?
    private var pushIdentityFingerprint: String?
    private var pushPreferenceEpoch: UInt64 = 0
    private var canRegisterPushToken = false

    public init(defaults: UserDefaults = .standard) {
        Self.cleanupExpiredExportTemporaries()
        self.defaults = defaults
        serverAddress = defaults.string(forKey: "zoe.instance.serverAddress") ?? ""
        let savedInstallation = defaults.string(forKey: "zoe.installationID")
        let identifier = savedInstallation.flatMap(UUID.init(uuidString:))?.uuidString.lowercased() ?? UUID().uuidString.lowercased()
        installationID = identifier
        defaults.set(identifier, forKey: "zoe.installationID")
        pushNotificationsEnabled = defaults.bool(forKey: "zoe.apns.optIn")
        continuousVoiceEnabled = defaults.bool(forKey: continuousVoiceKey)
        snapshotCache = nil
        RemotePushBridge.shared.onToken = { [weak self] token in
            Task { @MainActor [weak self] in self?.receiveAPNSToken(token) }
        }
        RemotePushBridge.shared.onWake = { [weak self] in
            await self?.refreshAfterRemoteWake()
        }
        RemotePushBridge.shared.onFailure = { [weak self] in
            Task { @MainActor [weak self] in self?.notice = uiText("Remote notifications are unavailable on this installation. The Zoe inbox remains available.") }
        }
#if os(macOS)
        terminationObserver = NotificationCenter.default.addObserver(forName: NSApplication.willTerminateNotification, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in await self?.terminateVoiceForAppExit() }
        }
#endif
    }

    deinit {
        eventTask?.cancel()
        voiceHangupRetryTask?.cancel()
        #if os(macOS)
        if let terminationObserver { NotificationCenter.default.removeObserver(terminationObserver) }
        #endif
        if let syncLoop { Task { await syncLoop.cancel() } }
    }

    public var projects: [Project] { snapshot?.projects ?? [] }
    public var tasks: [TaskRecord] {
        (snapshot?.tasks ?? []).sorted {
            let left = Self.taskTimestamp($0.updatedAt)
            let right = Self.taskTimestamp($1.updatedAt)
            if left != right { return left > right }
            let leftCreated = Self.taskTimestamp($0.createdAt)
            let rightCreated = Self.taskTimestamp($1.createdAt)
            if leftCreated != rightCreated { return leftCreated > rightCreated }
            return $0.id < $1.id
        }
    }
    private static func taskTimestamp(_ value: String) -> Date {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = formatter.date(from: value) { return date }
        formatter.formatOptions = [.withInternetDateTime]
        return formatter.date(from: value) ?? .distantPast
    }

    public var selectedTaskDraft: String { selectedTaskID.map { taskDrafts[$0] ?? "" } ?? newTaskDraft }
    public var approvals: [Approval] { snapshot?.approvals ?? [] }
    public var runs: [Run] { snapshot?.runs ?? [] }
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
            try await refreshSnapshot()
            guard generation == connectionGeneration.value else { return }
            authentication = .connected
            if let syncLoop { await syncLoop.cancel() }
            let syncLoop = ClientSyncLoop(api: configuredAPI)
            self.syncLoop = syncLoop
            await syncLoop.start()
            #if DEBUG
            if suppressConnectionFollowupsForTesting { return }
            #endif
            await reloadPendingCommandSummaries()
            if pushNotificationsEnabled { NativeNotificationDelivery.shared.registerForRemoteNotifications() }
            if let selectedTaskID { await loadTaskEntries(taskID: selectedTaskID) }
            guard generation == connectionGeneration.value else { return }
            await refreshMemory()
            guard generation == connectionGeneration.value else { return }
            await refreshNotifications()
            guard generation == connectionGeneration.value else { return }
            if let coordinator { await recoverPendingCommands(expectedGeneration: generation, sourceAPI: configuredAPI, coordinator: coordinator, workspaceID: snapshot?.workspaceId) }
            guard generation == connectionGeneration.value else { return }
            startEventLoop()
            if continuousVoiceEnabled, voiceSession == nil {
                Task { @MainActor [weak self] in await self?.startVoiceIfEnabled() }
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
            Task { await startVoiceIfEnabled() }
        } else if !enabled {
            Task { await endVoiceSession() }
        }
    }

    private func startVoiceIfEnabled() async {
        guard continuousVoiceEnabled, authentication.isConnected, voiceSession == nil else { return }
        await startVoice()
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
            // Keychain replacement is atomic. A denied write leaves the old workspace intact.
            try store.replace(candidate.key)
            do {
                guard try store.load() == candidate.key else {
                    throw ZoeError.persistence("The instance key could not be saved in Keychain.")
                }
                await endVoiceSession()
                // Cleanup must use the old credential scope without deleting the newly saved key.
                try await removeLocalConnection(preservingCredential: true, previousKey: previousKey)
            } catch {
                do {
                    if let previousKey { try store.replace(previousKey) }
                    else { try store.reset() }
                } catch {
                    throw ZoeError.persistence("The previous instance key could not be restored in Keychain. Reconnect with your server key.")
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
        await endVoiceSession()
        do {
            try await removeLocalConnection()
            notice = connectionCleanupWarning ?? uiText("Saved key and local connection data cleared.")
        } catch { notice = userMessage(error) }
    }

    private func removeLocalConnection(preservingCredential: Bool = false, previousKey: InstanceKey? = nil) async throws {
        connectionCleanupWarning = nil
        // Unregister only this physical installation; the shared key and other devices stay valid.
        pushPreferenceEpoch &+= 1
        canRegisterPushToken = false
        NativeNotificationDelivery.shared.unregisterFromRemoteNotifications()
        if let api, let tokenID = registeredPushTokenID {
            do { _ = try await api.revokePushToken(tokenID: tokenID) }
            catch { connectionCleanupWarning = uiText("Local data was cleared, but the server notification registration could not be removed. Other devices are unaffected.") }
        }
        pendingAPNSTokenHex = nil
        let oldJournal = journal
        let oldCache = snapshotCache
        lockWorkspace(clearDrafts: true)
        authentication = .disconnected
        // Capture before locking: lockWorkspace detaches resources to fence late callbacks.
        try await oldJournal?.discard()
        try oldCache?.discard()
        if let url = try? ServerAddress.normalize(serverAddress) {
            let store = try keyStore(url: url)
            let cleanupKey: InstanceKey?
            if preservingCredential { cleanupKey = previousKey }
            else { cleanupKey = try store.load() }
            if let key = cleanupKey {
                let scope = try key.bearerToken()
                if oldJournal == nil { try await CommandJournal(serverURL: url, credentialScope: scope).discard() }
                if oldCache == nil { try SnapshotCache(serverURL: url, credentialScope: scope).discard() }
            }
            if !preservingCredential { try store.reset() }
            for key in defaults.dictionaryRepresentation().keys where key.hasPrefix("zoe.apns.") && key.contains(url.absoluteString) {
                defaults.removeObject(forKey: key)
            }
        }
        api = nil
        identity = nil
        journal = nil
        coordinator = nil
        snapshotCache = nil
        configuredServerURL = nil
        pendingCommandSummaries = []
        commandStatusByKey = [:]
        commandTitleByKey = [:]
        registeredPushTokenID = nil
        pushIdentityFingerprint = nil
        lastRegisteredAPNSTokenHex = nil
        tokenRegistrationInFlight = nil
        canRegisterPushToken = false
        hasLoadedNotificationInbox = false
        serverAddress = ""
        defaults.removeObject(forKey: "zoe.instance.serverAddress")
    }

    private func lockWorkspace(clearDrafts: Bool) {
        connectionGeneration.advance()
        connectionEpoch = connectionGeneration.value
        eventTask?.cancel()
        eventTask = nil
        if let syncLoop { Task { await syncLoop.cancel() } }
        syncLoop = nil
        cancelExport(status: nil)
        clearVisibleMemory()
        snapshot = nil
        clearTaskContext(clearDrafts: clearDrafts)
        notifications = []
        capabilityReleases = []
        capabilityDeployments = []
        pendingCommandSummaries = []
        commandStatusByKey = [:]
        commandTitleByKey = [:]
        canRegisterPushToken = false
        hasLoadedNotificationInbox = false
        voiceConfig = nil
        voicePoolStatus = nil
        if clearDrafts { voiceDraft = "" }
        api = nil
        identity = nil
        journal = nil
        coordinator = nil
        snapshotCache = nil
        // Capture and detach old resources synchronously. Later cleanup cannot target a new session.
        let oldSession = voiceSession
        let oldAPI = voiceAPI
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
        Task { [weak self] in
            await oldTransport?.close()
            guard let self, let oldSession, let oldAPI else { return }
            await self.releaseVoiceSession(oldSession, api: oldAPI, phase: "workspace_lock")
        }
    }

    @discardableResult
    public func createProject(name: String, description: String?) async -> Bool {
        var payload: [String: JSONValue] = ["name": .string(name)]
        if let description, !description.isEmpty { payload["description"] = .string(description) }
        return await send(kind: "project.create", payload: .object(payload))
    }

    @discardableResult
    public func remember(scope: String, scopeID: String?, subject: String, key: String, content: String) async -> Bool {
        var scopePayload: [String: JSONValue] = ["kind": .string(scope)]
        if let scopeID { scopePayload["id"] = .string(scopeID) }
        let payload: JSONValue = .object([
            "scope": .object(scopePayload),
            "subject": .string(subject),
            "key": .string(key),
            "content": .string(content),
            "source_kind": .string("user_note")
        ])
        let accepted = await send(kind: "memory.remember", payload: payload)
        if accepted { await refreshMemory() }
        return accepted
    }

    public func deleteMemory(_ memory: MemoryClaim) async {
        let payload: JSONValue = .object([
            "memory_id": .string(memory.id),
            "expected_revision": .integer(memory.revision),
            "reason": .string("user_requested")
        ])
        await send(kind: "memory.delete", expectedVersion: memory.revision, payload: payload)
        await refreshMemory()
    }

    public func markNotificationRead(_ notification: NotificationRecord) async {
        guard notification.readAt == nil else { return }
        await send(kind: "notification.mark_read", payload: .object(["notification_id": .string(notification.id)]))
        await refreshNotifications()
    }

    public func enablePushNotifications() async {
        pushPreferenceEpoch &+= 1
        let preferenceEpoch = pushPreferenceEpoch
        do {
            guard try await NativeNotificationDelivery.shared.requestPermission() else {
                notice = uiText("System notification permission is disabled. The Zoe inbox remains available.")
                return
            }
            guard preferenceEpoch == pushPreferenceEpoch else { return }
            pushNotificationsEnabled = true
            defaults.set(true, forKey: "zoe.apns.optIn")
            NativeNotificationDelivery.shared.registerForRemoteNotifications()
            await registerPendingAPNSToken()
            if pendingAPNSTokenHex == nil {
                notice = uiText("Push wakeups are enabled. Waiting for APNs registration.")
            } else if lastRegisteredAPNSTokenHex == pendingAPNSTokenHex {
                notice = uiText("Push wakeups are enabled. Notification content remains in the Zoe inbox.")
            }
        } catch {
            reportClientError(error, code: "notification_permission")
            notice = uiText("System notification permission could not be requested. Try again.")
        }
    }

    public func disablePushNotifications() async {
        pushPreferenceEpoch &+= 1
        pushNotificationsEnabled = false
        defaults.set(false, forKey: "zoe.apns.optIn")
        NativeNotificationDelivery.shared.unregisterFromRemoteNotifications()
        pendingAPNSTokenHex = nil
        lastRegisteredAPNSTokenHex = nil
        if registeredPushTokenID != nil || tokenRegistrationInFlight != nil {
            defaults.set(true, forKey: pushRevocationPendingKey)
        }
        guard registeredPushTokenID != nil else {
            notice = uiText("Push wakeups are disabled on this device.")
            return
        }
        defaults.set(true, forKey: pushRevocationPendingKey)
        await revokePendingPushToken()
    }

    public func refreshAfterRemoteWake() async {
        guard canRegisterPushToken, api != nil else { return }
        do {
            try await refreshSnapshot()
            await refreshNotifications()
            await refreshMemory()
        } catch {
            if snapshot == nil { setOffline(error) }
            else { setStale(uiText("A push wakeup arrived, but the latest server state could not be loaded.")) }
        }
    }

    public func refreshNotifications(unreadOnly: Bool = false) async {
        guard let api else { return }
        let generation = connectionGeneration.value
        do {
            let result = try await api.notifications(unreadOnly: unreadOnly)
            guard generation == connectionGeneration.value, api === self.api else { return }
            let oldIDs = Set(notifications.map(\.id))
            notifications = result.notifications
            if hasLoadedNotificationInbox {
                for item in result.notifications where item.readAt == nil && !oldIDs.contains(item.id) {
                    await NativeNotificationDelivery.shared.deliver(item)
                }
            }
            hasLoadedNotificationInbox = true
        } catch {
            guard generation == connectionGeneration.value else { return }
            notice = uiText("The notification inbox could not be refreshed.")
        }
    }

    public func refreshMemory(query: String = "") async {
        #if DEBUG
        let testSearch = memorySearchOverrideForTesting
        #else
        let testSearch: ((String) async throws -> MemorySearchResult)? = nil
        #endif
        guard api != nil || testSearch != nil else { return }
        if memoryRefreshInFlight {
            memoryRefreshSequence &+= 1
            pendingMemoryRefreshQuery = query
            return
        }
        memoryRefreshInFlight = true
        defer { memoryRefreshInFlight = false }

        var nextQuery: String? = query
        while let currentQuery = nextQuery {
            nextQuery = nil
            memoryRefreshSequence &+= 1
            let requestSequence = memoryRefreshSequence
            let generation = connectionGeneration.value
            let capturedAPI = api
            do {
                let result: MemorySearchResult
                if let testSearch { result = try await testSearch(currentQuery) }
                else if let capturedAPI { result = try await capturedAPI.searchMemory(query: currentQuery) }
                else { throw ZoeError.missingCredential }
                guard requestSequence == memoryRefreshSequence,
                      generation == connectionGeneration.value,
                      testSearch != nil || capturedAPI === self.api else {
                    nextQuery = pendingMemoryRefreshQuery
                    pendingMemoryRefreshQuery = nil
                    continue
                }
                memoryClaims = result.items
                memoryMissing = result.missing
                memoryConflicts = result.conflicts
            } catch {
                guard requestSequence == memoryRefreshSequence,
                      generation == connectionGeneration.value,
                      testSearch != nil || capturedAPI === self.api else {
                    nextQuery = pendingMemoryRefreshQuery
                    pendingMemoryRefreshQuery = nil
                    continue
                }
                if let failure = error as? ZoeError,
                   case let .http(status, _, _, _) = failure, status == 401 || status == 403 {
                    clearMemoryAndPreparedExport(status: uiText("Memory access was denied; cached memory and the prepared export were cleared."))
                } else {
                    notice = uiText("Memory could not be refreshed.")
                }
            }
            nextQuery = pendingMemoryRefreshQuery
            pendingMemoryRefreshQuery = nil
        }
    }

    public func refreshCapabilities() async {
        guard let api else { return }
        let generation = connectionGeneration.value
        do {
            async let releases = api.capabilityReleases()
            async let deployments = api.capabilityDeployments()
            let (nextReleases, nextDeployments) = try await (releases, deployments)
            guard generation == connectionGeneration.value, api === self.api else { return }
            capabilityReleases = nextReleases.items
            capabilityDeployments = nextDeployments.items
        } catch {
            guard generation == connectionGeneration.value else { return }
            notice = uiText("Capability inventory could not be refreshed.")
        }
    }

    public func submitCapabilityRelease(artifactRef: String, artifactDigest: String, entrypoint: String, capabilities: String, cpuMillis: Int64, memoryBytes: Int64, diskBytes: Int64, timeoutSeconds: Int64, concurrency: Int, expectedConnectionEpoch: UInt64) async -> Bool {
        guard expectedConnectionEpoch == connectionEpoch else {
            notice = uiText("The server connection changed. Reopen this form and review its values before submitting.")
            return false
        }
        let manifest: JSONValue = .object([
            "schema_version": .integer(1),
            "artifact_ref": .string(artifactRef),
            "artifact_digest": .string(artifactDigest),
            "entrypoint": .string(entrypoint),
            "capabilities": .array(capabilities.split(separator: ",").map { .string($0.trimmingCharacters(in: .whitespacesAndNewlines)) }.filter { if case .string(let value) = $0 { return !value.isEmpty }; return false }),
            "budget": .object([
                "cpu_millis": .integer(cpuMillis),
                "memory_bytes": .integer(memoryBytes),
                "disk_bytes": .integer(diskBytes),
                "timeout_seconds": .integer(timeoutSeconds),
                "concurrency": .integer(Int64(concurrency))
            ]),
            "network": .object(["mode": .string("none")])
        ])
        guard await send(kind: "capability.release.submit", payload: .object(["manifest": manifest])) else { return false }
        await refreshCapabilities()
        return true
    }

    public func requestCapabilityDeployment(serviceID: String, releaseDigest: String, operation: String, expectedConnectionEpoch: UInt64) async -> Bool {
        guard expectedConnectionEpoch == connectionEpoch,
              capabilityReleases.contains(where: { $0.digest == releaseDigest }) else {
            notice = uiText("The server connection or release selection changed. Reopen this form and review the current inventory.")
            return false
        }
        let payload: JSONValue = .object([
            "service_id": .string(serviceID),
            "release_digest": .string(releaseDigest),
            "operation": .string(operation)
        ])
        let expectedVersion = capabilityDeployments.version(forServiceID: serviceID)
        guard await send(kind: "capability.deployment.request", expectedVersion: expectedVersion, payload: payload) else { return false }
        await refreshCapabilities()
        return true
    }

    public func revokeCapabilityDeployment(_ deployment: CapabilityDeployment, expectedConnectionEpoch: UInt64) async -> Bool {
        guard expectedConnectionEpoch == connectionEpoch else { return false }
        guard let current = capabilityDeployments.first(where: { $0.deploymentId == deployment.deploymentId }),
              current.version == deployment.version,
              current.revokedAt == nil,
              !current.grantId.isEmpty else {
            await refreshRevocationInventory()
            guard expectedConnectionEpoch == connectionEpoch else { return false }
            notice = uiText("This deployment changed or its authorization is already revoked. The latest capability inventory is being loaded.")
            return false
        }

        let payload: JSONValue = .object(["deployment_id": .string(deployment.deploymentId)])
        let accepted: Bool
#if DEBUG
        if let submit = capabilityRevocationSubmitOverrideForTesting {
            accepted = await submit(deployment.version, payload)
        } else {
            accepted = await send(kind: "capability.deployment.revoke", expectedVersion: deployment.version, payload: payload)
        }
#else
        accepted = await send(kind: "capability.deployment.revoke", expectedVersion: deployment.version, payload: payload)
#endif
        guard expectedConnectionEpoch == connectionEpoch else { return false }
        guard accepted else {
            await refreshRevocationInventory()
            guard expectedConnectionEpoch == connectionEpoch else { return false }
            if let refreshed = capabilityDeployments.first(where: { $0.deploymentId == deployment.deploymentId }),
               refreshed.version != deployment.version {
                notice = uiText("The deployment version changed. The latest capability inventory has been loaded; review it before trying again.")
            }
            return false
        }

        await refreshRevocationInventory()
        guard expectedConnectionEpoch == connectionEpoch else { return false }
        notice = uiText("Authorization revoked. Stop cleanup status is shown in the capability inventory.")
        return true
    }

    private func refreshRevocationInventory() async {
#if DEBUG
        if let refresh = capabilityRevocationRefreshOverrideForTesting {
            await refresh()
            return
        }
#endif
        await refreshCapabilities()
    }

    public func authorizeCapabilityDeployment(_ deployment: CapabilityDeployment, release: CapabilityRelease, scopeKind: String = "workspace", scopeID: String? = nil, expiresAt: Date, expectedConnectionEpoch: UInt64) async -> Bool {
        guard expectedConnectionEpoch == connectionEpoch,
              capabilityDeployments.contains(where: { $0.deploymentId == deployment.deploymentId && $0.version == deployment.version }),
              capabilityReleases.contains(where: { $0.digest == release.digest && $0.digest == deployment.releaseDigest }) else {
            notice = uiText("The server connection or deployment version changed. Reopen this request and review the current inventory.")
            return false
        }
        var payload: [String: JSONValue] = [
            "deployment_id": .string(deployment.deploymentId),
            "scope_kind": .string(scopeKind),
            "actions": .array([.string(deployment.requestedOperation)]),
            "budget": .object([
                "cpu_millis": .integer(release.manifest.budget.cpuMillis),
                "memory_bytes": .integer(release.manifest.budget.memoryBytes),
                "disk_bytes": .integer(release.manifest.budget.diskBytes),
                "timeout_seconds": .integer(release.manifest.budget.timeoutSeconds),
                "concurrency": .integer(Int64(release.manifest.budget.concurrency))
            ]),
            "expires_at": .string(ISO8601DateFormatter().string(from: expiresAt))
        ]
        if let scopeID, !scopeID.isEmpty { payload["scope_id"] = .string(scopeID) }
        guard await send(kind: "capability.deployment.authorize", expectedVersion: deployment.version, payload: .object(payload)) else { return false }
        await refreshCapabilities()
        return true
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
        guard voiceSession == nil, !voiceStartInProgress else { return }
        voiceStartInProgress = true
        defer { voiceStartInProgress = false }
        let mediaEpoch = voiceMediaFence.beginAttempt()
        voiceStartMediaLoss = nil
        let generation: UInt64
        let api: APIClient
        do {
            api = try configure()
            generation = connectionGeneration.value
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

            #if os(iOS)
            audio = AppleMobileAudioSessionAdapter()
            #elseif os(macOS)
            audio = AppleDesktopAudioSessionAdapter()
            #endif
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
            startVoiceHeartbeat(sessionID: created.voiceSessionId, api: api, generation: generation)
        } catch {
            let redacted = RedactedDiagnostic(error)
            api.recordDiagnostic(phase: diagnosticPhase, requestPath: "/v1/voice/\(diagnosticPhase)", httpStatus: redacted.httpStatus, errorType: redacted.errorType, errorSummary: redacted.errorSummary)
            let attemptWasCurrent = mediaEpoch == voiceMediaEpoch
            await closeVoiceLocalMedia(mediaEpoch: mediaEpoch)
            var remainsQuarantined = false
            if let allocatedSession {
                let release = await releaseVoiceSession(allocatedSession, api: api, phase: "setup_release")
                remainsQuarantined = release == nil || release?.poolBlocked == true
                if remainsQuarantined {
                    let latestPool = try? await api.voicePoolStatus()
                    if generation == connectionGeneration.value, api === self.api { voicePoolStatus = latestPool }
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
        guard let session = voiceSession, let api = voiceAPI, let mediaEpoch = voiceSessionMediaEpoch else {
            if voiceStartInProgress {
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
        await finishVoiceSession(session, api: api, mediaEpoch: mediaEpoch, lossReason: nil)
    }

    private func finishVoiceSession(_ session: VoiceSessionResponse, api: APIClient, mediaEpoch: UInt64, lossReason: String?) async {
        guard voiceSession?.voiceSessionId == session.voiceSessionId,
              voiceSessionMediaEpoch == mediaEpoch,
              voiceHangupInFlightSessionID != session.voiceSessionId else { return }
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
            await api.uploadDiagnosticsIfPossible()
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
            await api.uploadDiagnosticsIfPossible()
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

    private func handleVoiceMediaLoss(_ loss: VoiceMediaLoss, mediaEpoch: UInt64) async {
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
            }
        }
        if let session = voiceSession, let api = voiceAPI, voiceSessionMediaEpoch == mediaEpoch {
            if continuousVoiceEnabled {
                voiceStatus = .reconnecting(reason: reason)
                #if DEBUG
                voiceStatusObserverForTesting?(voiceStatus)
                #endif
            }
            await finishVoiceSession(session, api: api, mediaEpoch: mediaEpoch, lossReason: reason)
            if continuousVoiceEnabled {
                Task { @MainActor [weak self] in
                    try? await Task.sleep(for: .seconds(2))
                    await self?.startVoiceIfEnabled()
                }
            }
            return
        }
        voiceStartMediaLoss = (mediaEpoch, reason)
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

    public func confirmVoiceDraftAsTask(title: String? = nil) async {
        guard !isBusy, let session = voiceSession, let coordinator, let sourceAPI = api, !voiceDraft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
              let turnID = voiceTurnID, voiceDraftRevision > 0,
              voiceSessionMediaEpoch == voiceMediaEpoch,
              case .active = voiceStatus else { return }
        let generation = connectionGeneration.value
        let revision = voiceDraftRevision
        let workspaceID = snapshot?.workspaceId
        let text = voiceDraft.trimmingCharacters(in: .whitespacesAndNewlines)
        var commitFields: [String: JSONValue] = [
            "session_id": .string(session.voiceSessionId),
            "generation": .integer(session.generation),
            "turn_id": .string(turnID),
            "revision": .integer(revision),
            "content": .string(text),
            "user_confirmed": .bool(true)
        ]
        if let voiceProviderMessageID { commitFields["provider_message_id"] = .string(voiceProviderMessageID) }
        let commit = JSONValue.object(commitFields)
        let taskTitle = title?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false ? title! : String(text.prefix(72))
        let commitKey = UUID().uuidString.lowercased()
        let taskKey = UUID().uuidString.lowercased()
        var taskCreateAttempted = false
        isBusy = true
        defer { isBusy = false }
        do {
            _ = try await coordinator.submit(kind: "voice.turn.commit", payload: commit, workspaceID: workspaceID, idempotencyKey: commitKey)
            guard isCurrentConnection(generation, sourceAPI: sourceAPI) else { return }
            commandTitleByKey[commitKey] = uiText("Save confirmed voice turn")
            commandStatusByKey[commitKey] = .acceptedAwaitingSnapshot
            await reloadPendingCommandSummaries()
            let sourceReference: JSONValue = .object([
                "kind": .string("voice_turn"),
                "voice_session_id": .string(session.voiceSessionId),
                "generation": .integer(session.generation),
                "turn_id": .string(turnID),
                "revision": .integer(revision)
            ])
            let createTask: JSONValue = .object([
                "title": .string(taskTitle),
                "goal": .string(text),
                "source": .string("voice"),
                "source_reference": sourceReference,
                "initial_message": .string(text)
            ])
            taskCreateAttempted = true
            let taskOutcome = try await coordinator.submit(kind: "task.create", payload: createTask, workspaceID: workspaceID, idempotencyKey: taskKey)
            guard isCurrentConnection(generation, sourceAPI: sourceAPI) else { return }
            commandTitleByKey[taskKey] = uiText("Create task")
            commandStatusByKey[taskKey] = .acceptedAwaitingSnapshot
            await reloadPendingCommandSummaries()
            let taskReceipt = taskOutcome.receipt
            guard let taskID = taskReceipt.resourceId, let version = taskReceipt.resourceVersion else { throw ZoeError.invalidResponse }
            guard version > 0 else { throw ZoeError.invalidResponse }
            notice = uiFormat("Voice turn confirmed and linked to task %@.", taskID)
            voiceDraft = ""
            voiceTurnID = nil
            voiceProviderMessageID = nil
            voiceDraftRevision = 0
            try await refreshSnapshot()
        } catch {
            if isCurrentConnection(generation, sourceAPI: sourceAPI) {
                if isNeedsReview(error) {
                    let key = taskCreateAttempted ? taskKey : commitKey
                    commandStatusByKey[key] = .needsReview
                    try? await refreshSnapshot()
                }
                notice = uiText("The voice turn may be committed, but task creation needs attention.")
                await reloadPendingCommandSummaries()
            }
        }
    }

    public func updateVoiceDraft(_ text: String) {
        guard text != voiceDraft else { return }
        voiceDraft = text
        voiceDraftRevision += 1
    }

    public func setNotice(_ message: String) {
        notice = message
    }

    public func createTask(goal: String, title: String, projectID: String?) async -> TaskSubmissionResult {
        let cleanGoal = goal.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !cleanGoal.isEmpty else { return .failed(uiText("Enter a task before sending.")) }
        var payload: [String: JSONValue] = [
            "title": .string(title),
            "goal": .string(cleanGoal),
            "source": .string("user"),
            "initial_message": .string(cleanGoal)
        ]
        if let projectID { payload["project_id"] = .string(projectID) }
        return await submitTaskCommand(kind: "task.create", payload: .object(payload), clearsDraftID: nil)
    }

    public func append(to task: TaskRecord, content: String) async -> TaskSubmissionResult {
        let cleanContent = content.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !cleanContent.isEmpty else { return .failed(uiText("Enter a message before sending.")) }
        guard let currentTask = self.task(id: task.id) else {
            let message = uiText("This task is unavailable in the latest snapshot. Refresh it or choose New task before sending.")
            notice = message
            return .failed(message)
        }
        guard currentTask.status.canAcceptEntries else {
            return .failed(uiText("This task is finished and cannot accept more entries. Choose New task to start separate work."))
        }
        return await submitTaskCommand(kind: "task.append", expectedVersion: currentTask.version, payload: .object(["task_id": .string(currentTask.id), "content": .string(cleanContent)]), clearsDraftID: currentTask.id)
    }

    public func selectTask(_ taskID: String?) async {
        guard selectedTaskID != taskID else {
            if let taskID { await loadTaskEntries(taskID: taskID) }
            return
        }
        selectedTaskID = taskID
        clearTaskHistory()
        guard let taskID, task(id: taskID) != nil else {
            selectedTaskID = nil
            return
        }
        await loadTaskEntries(taskID: taskID)
    }

    private func clearTaskHistory() {
        taskEntriesRequestID = nil
        taskEntries = []
        taskEntriesCursor = nil
        hasMoreTaskEntries = false
        isLoadingTaskEntries = false
        taskEntriesError = nil
    }

    private func clearTaskContext(clearDrafts: Bool) {
        selectedTaskID = nil
        clearTaskHistory()
        if clearDrafts {
            taskDrafts = [:]
            newTaskDraft = ""
            taskIntents = [:]
        }
    }

    public func loadMoreTaskEntries() async {
        guard hasMoreTaskEntries, !isLoadingTaskEntries, let taskID = selectedTaskID else { return }
        await loadTaskEntries(taskID: taskID, cursor: taskEntriesCursor)
    }

    private func loadTaskEntries(taskID: String, cursor: String? = nil) async {
        guard selectedTaskID == taskID, let sourceAPI = api else { return }
        let generation = connectionGeneration.value
        let requestID = UUID()
        taskEntriesRequestID = requestID
        isLoadingTaskEntries = true
        taskEntriesError = nil
        if cursor == nil { taskEntries = []; taskEntriesCursor = nil; hasMoreTaskEntries = false }
        defer {
            if taskEntriesRequestID == requestID { isLoadingTaskEntries = false }
        }
        do {
            let page: TaskEntriesPage
            #if DEBUG
            if let fetch = taskEntriesFetchOverrideForTesting { page = try await fetch(taskID, cursor) }
            else { page = try await sourceAPI.taskEntries(taskID: taskID, cursor: cursor) }
            #else
            page = try await sourceAPI.taskEntries(taskID: taskID, cursor: cursor)
            #endif
            guard isCurrentConnection(generation, sourceAPI: sourceAPI), selectedTaskID == taskID,
                  taskEntriesRequestID == requestID else { return }
            guard page.messages.allSatisfy({ $0.taskId == taskID }),
                  page.nextCursor == nil || (page.nextCursor != "" && page.nextCursor != cursor) else { throw ZoeError.invalidResponse }
            // The server defines chronological ordering, including timestamp precision.
            // Keep that page order and replace overlapping IDs without adding rows.
            for entry in page.messages {
                if let index = taskEntries.firstIndex(where: { $0.id == entry.id }) { taskEntries[index] = entry }
                else { taskEntries.append(entry) }
            }
            taskEntriesCursor = page.nextCursor
            hasMoreTaskEntries = page.nextCursor != nil
        } catch {
            guard isCurrentConnection(generation, sourceAPI: sourceAPI), selectedTaskID == taskID,
                  taskEntriesRequestID == requestID else { return }
            // Never present a previously loaded transcript as a current successful read.
            taskEntries = []
            taskEntriesCursor = nil
            hasMoreTaskEntries = false
            taskEntriesError = userMessage(error)
        }
    }

    private func submitTaskCommand(kind: String, expectedVersion: Int64? = nil, payload: JSONValue, clearsDraftID: String?, retryKey: String? = nil) async -> TaskSubmissionResult {
        guard !isBusy else { return .failed(uiText("Another action is being confirmed. Try again when it finishes.")) }
        guard let coordinator, let sourceAPI = api, let journal else { return .failed(uiText("Connect to a Zoe server first.")) }
        let generation = connectionGeneration.value
        let context = clearsDraftID.map { "task:\($0)" } ?? "new-task"
        guard let workspaceID = snapshot?.workspaceId else { return .failed(uiText("Connect to a Zoe server first.")) }
        isBusy = true
        defer { isBusy = false }
        var key = retryKey ?? taskIntents[context]?.key ?? UUID().uuidString.lowercased()
        do {
            let pending = try await journal.all()
            guard isCurrentConnection(generation, sourceAPI: sourceAPI) else { return .unknown(idempotencyKey: key, message: uiText("Connection changed before the action could be confirmed.")) }
            // Reuse a durable unresolved action, including after application restart.
            let saved = pending.first { command in
                guard command.workspaceID == workspaceID, command.kind == kind else { return false }
                if let retryKey { return command.idempotencyKey == retryKey }
                if kind == "task.create" { return true }
                guard case let .object(fields) = command.payload,
                      case let .string(taskID)? = fields["task_id"] else { return false }
                return taskID == clearsDraftID
            }
            if let retryKey, saved == nil {
                return .unknown(idempotencyKey: retryKey, message: uiText("Connection changed before confirmation."))
            }
            if let saved { key = saved.idempotencyKey }
            taskIntents[context] = TaskIntent(key: key)
            let outcome: CommandOutcome
            if pending.contains(where: { $0.idempotencyKey == key }) {
                outcome = try await coordinator.retryPending(idempotencyKey: key, currentWorkspaceID: workspaceID)
            } else {
                outcome = try await coordinator.submit(kind: kind, expectedVersion: expectedVersion, payload: payload, workspaceID: workspaceID, idempotencyKey: key)
            }
            guard isCurrentConnection(generation, sourceAPI: sourceAPI) else { return .unknown(idempotencyKey: key, message: uiText("The server accepted the action on the previous connection.")) }
            reconcileTaskOutcome(outcome)
            do { try await refreshSnapshot() }
            catch {
                guard isCurrentConnection(generation, sourceAPI: sourceAPI) else { return .unknown(idempotencyKey: key, message: uiText("Connection changed after acceptance.")) }
                setStale(uiText("The action was accepted, but the latest task state could not be loaded."))
            }
            if let taskID = selectedTaskID { await loadTaskEntries(taskID: taskID) }
            guard isCurrentConnection(generation, sourceAPI: sourceAPI) else { return .unknown(idempotencyKey: key, message: uiText("Connection changed after acceptance.")) }
            await reloadPendingCommandSummaries()
            return .accepted(outcome.receipt)
        } catch {
            guard isCurrentConnection(generation, sourceAPI: sourceAPI) else { return .unknown(idempotencyKey: key, message: uiText("Connection changed before confirmation.")) }
            await reloadPendingCommandSummaries()
            if let error = error as? ZoeError, case let .http(status, _, _, _) = error,
               [400, 401, 403, 404, 409, 410, 413].contains(status) {
                taskIntents[context] = nil
                if isNeedsReview(error) { try? await refreshSnapshot() }
                return .failed(userMessage(error))
            }
            return .unknown(idempotencyKey: key, message: uiText("Could not confirm this send. Your draft is preserved; sending again checks the same saved action."))
        }
    }

    private func reconcileTaskOutcome(_ outcome: CommandOutcome) {
        guard outcome.receipt.kind == "task.create" || outcome.receipt.kind == "task.append" else { return }
        guard case let .object(payload) = outcome.command.payload else { return }
        let field = outcome.receipt.kind == "task.create" ? "goal" : "content"
        guard case let .string(content)? = payload[field] else { return }
        if outcome.receipt.kind == "task.create" {
            if newTaskDraft.trimmingCharacters(in: .whitespacesAndNewlines) == content { newTaskDraft = "" }
        } else if let taskID = outcome.receipt.resourceId,
                  taskDrafts[taskID]?.trimmingCharacters(in: .whitespacesAndNewlines) == content {
            taskDrafts[taskID] = ""
        }
        taskIntents = taskIntents.filter { $0.value.key != outcome.receipt.idempotencyKey }
    }

    public func updateTaskDraft(_ text: String, for taskID: String? = nil) {
        guard let taskID else { newTaskDraft = text; return }
        taskDrafts[taskID] = text
    }

    public func pendingTaskSubmissionKey(for taskID: String?) -> String? {
        taskIntents[taskID.map { "task:\($0)" } ?? "new-task"]?.key
    }

    public func retryTaskSubmission(idempotencyKey: String) async -> TaskSubmissionResult {
        guard let journal else { return .failed(uiText("Connect to a Zoe server first.")) }
        let generation = connectionGeneration.value
        do {
            let pending = try await journal.all()
            guard generation == connectionGeneration.value,
                  let command = pending.first(where: { $0.idempotencyKey == idempotencyKey }),
                  command.kind == "task.create" || command.kind == "task.append" else {
                return .unknown(idempotencyKey: idempotencyKey, message: uiText("Connection changed before confirmation."))
            }
            var taskID: String?
            if command.kind == "task.append", case let .object(fields) = command.payload,
               case let .string(value)? = fields["task_id"] { taskID = value }
            return await submitTaskCommand(kind: command.kind, expectedVersion: command.expectedVersion, payload: command.payload, clearsDraftID: taskID, retryKey: idempotencyKey)
        } catch {
            return .unknown(idempotencyKey: idempotencyKey, message: uiText("Could not confirm this send. Your draft is preserved; sending again checks the same saved action."))
        }
    }

    public func sendTaskDraft(projectID: String? = nil) async -> TaskSubmissionResult {
        await sendTaskDraft(for: selectedTaskID, projectID: projectID)
    }

    public func sendTaskDraft(for taskID: String?, projectID: String? = nil) async -> TaskSubmissionResult {
        let content = (taskID.map { taskDrafts[$0] ?? "" } ?? newTaskDraft).trimmingCharacters(in: .whitespacesAndNewlines)
        guard !content.isEmpty else { return .failed(uiText("Enter a task before sending.")) }
        if let taskID {
            guard let currentTask = task(id: taskID) else { return .failed(uiText("Refresh this task before sending.")) }
            let result = await append(to: currentTask, content: content)
            return result
        }
        let result = await createTask(goal: content, title: String(content.prefix(72)), projectID: projectID)
        if case let .accepted(receipt) = result {
            if selectedTaskID == nil, let taskID = receipt.resourceId { await selectTask(taskID) }
        }
        return result
    }

    @discardableResult
    public func stop(_ task: TaskRecord, reason: String? = nil) async -> Bool {
        var payload: [String: JSONValue] = ["task_id": .string(task.id)]
        if let reason, !reason.isEmpty { payload["reason"] = .string(reason) }
        return await send(kind: "task.stop", expectedVersion: task.version, payload: .object(payload))
    }

    @discardableResult
    public func decide(_ approval: Approval, decision: String) async -> Bool {
        guard decision == "approve" || decision == "reject" else { return false }
        let payload: [String: JSONValue] = [
            "task_id": .string(approval.taskId),
            "approval_id": .string(approval.id),
            "decision": .string(decision)
        ]
        return await send(kind: "task.approve", expectedVersion: approval.version, payload: .object(payload))
    }

    public func resultRun(for task: TaskRecord) -> Run? {
        guard task.canAcceptResult else { return nil }
        let candidates = runs.filter {
            $0.taskId == task.id && $0.status == "verification_pending" && $0.terminationConfirmed
        }
        guard candidates.count == 1 else { return nil }
        return candidates[0]
    }

    @discardableResult
    public func decideResult(for task: TaskRecord, run: Run, decision: String) async -> Bool {
        guard decision == "accept" || decision == "reject",
              task.canAcceptResult,
              run.taskId == task.id,
              run.status == "verification_pending",
              run.terminationConfirmed,
              resultRun(for: task)?.id == run.id else { return false }
        let payload: [String: JSONValue] = [
            "task_id": .string(task.id),
            "run_id": .string(run.id),
            "decision": .string(decision)
        ]
        return await send(kind: "task.accept_result", expectedVersion: task.version, payload: .object(payload))
    }

    public func task(id: String?) -> TaskRecord? {
        guard let id else { return nil }
        return tasks.first { $0.id == id }
    }

    public func prepareExport(scope: String, expectedConnectionEpoch: UInt64) async {
#if DEBUG
        let testDownload = exportDownloadOverrideForTesting
#else
        let testDownload: ((String) async throws -> ScopedExportArtifact)? = nil
#endif
        guard scope == "personal" || scope == "workspace",
              expectedConnectionEpoch == connectionGeneration.value,
              api != nil || testDownload != nil else {
            if expectedConnectionEpoch == connectionGeneration.value { exportStatus = uiText("Connect to your server before preparing an export.") }
            return
        }
        discardExportArtifact()
        exportStatus = uiText("Preparing the authorized export…")
        let generation = connectionGeneration.value
        let capturedAPI = api
        let requestID = UUID()
        exportRequestID = requestID
        let task = Task {
            if let testDownload { return try await testDownload(scope) }
            guard let capturedAPI else { throw ZoeError.missingCredential }
            return try await capturedAPI.downloadExport(scope: scope)
        }
        exportDownloadTask = task
        defer {
            if requestID == exportRequestID {
                exportDownloadTask = nil
                exportRequestID = nil
            }
        }
        var unownedArtifactURL: URL?
        do {
            let artifact = try await task.value
            unownedArtifactURL = artifact.fileURL
            if testDownload == nil { try await refreshSnapshot() }
            guard !Task.isCancelled, requestID == exportRequestID,
                  generation == connectionGeneration.value,
                  testDownload != nil || capturedAPI === self.api else {
                try? FileManager.default.removeItem(at: artifact.fileURL)
                unownedArtifactURL = nil
                if requestID == exportRequestID {
                    exportStatus = uiText("Export canceled. The prepared temporary file was removed.")
                }
                return
            }
            guard testDownload != nil || Self.exportAuthorityMatches(artifact, snapshot: snapshot) else {
                try? FileManager.default.removeItem(at: artifact.fileURL)
                unownedArtifactURL = nil
                exportStatus = uiText("Server authority changed while preparing the export. Prepare a fresh copy.")
                return
            }
            exportArtifact = artifact
            exportArtifactConnectionEpoch = generation
            unownedArtifactURL = nil
            exportStatus = uiText("Export prepared by the server. Choose Save to select where to keep the file.")
        } catch {
            if let unownedArtifactURL { try? FileManager.default.removeItem(at: unownedArtifactURL) }
            guard requestID == exportRequestID, generation == connectionGeneration.value else { return }
            exportStatus = userMessage(error)
        }
    }

    public func exportSaveFinished(saved: Bool, error: Error? = nil) {
        guard let artifact = exportArtifact else { return }
        try? FileManager.default.removeItem(at: artifact.fileURL)
        exportArtifact = nil
        exportArtifactConnectionEpoch = nil
        if saved { exportStatus = uiText("Export saved on this device.") }
        else if let error, (error as NSError).code != NSUserCancelledError {
            reportClientError(error, code: "export_save")
            exportStatus = uiText("The export could not be saved.")
        }
        else { exportStatus = uiText("Saving was canceled. The prepared temporary file was removed.") }
    }

    public func cancelExport(status: String? = "Export canceled. The prepared temporary file was removed.") {
        exportRequestID = nil
        exportDownloadTask?.cancel()
        exportDownloadTask = nil
        discardExportArtifact()
        exportStatus = status.map { uiText($0) }
    }

    public func validateExportForSave(expectedConnectionEpoch: UInt64) async -> Bool {
        guard let artifact = exportArtifact,
              expectedConnectionEpoch == connectionGeneration.value,
              exportArtifactConnectionEpoch == expectedConnectionEpoch,
              api != nil else {
            cancelExport(status: uiText("The export is no longer authorized on this connection. Prepare a fresh copy."))
            return false
        }
        do {
            try await refreshSnapshot()
        } catch {
            if exportArtifact?.fileURL == artifact.fileURL {
                cancelExport(status: uiText("Could not recheck server authority. The prepared export was removed."))
            }
            return false
        }
        guard expectedConnectionEpoch == connectionGeneration.value,
              exportArtifactConnectionEpoch == expectedConnectionEpoch,
              exportArtifact?.fileURL == artifact.fileURL,
              Self.exportAuthorityMatches(artifact, snapshot: snapshot) else {
            if exportArtifact?.fileURL == artifact.fileURL {
                cancelExport(status: uiText("Server authority changed. The prepared export was removed."))
            }
            return false
        }
        return true
    }

    private func discardExportArtifact() {
        if let artifact = exportArtifact { try? FileManager.default.removeItem(at: artifact.fileURL) }
        exportArtifact = nil
        exportArtifactConnectionEpoch = nil
    }

    public func projectName(_ id: String?) -> String {
        guard let id else { return uiText("Unassigned") }
        return projects.first(where: { $0.id == id })?.name ?? uiText("Project unavailable")
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
            freshness = .loading
            hasLoadedNotificationInbox = false
            canRegisterPushToken = false
            lastRegisteredAPNSTokenHex = nil
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
        pushIdentityFingerprint = installationID
        registeredPushTokenID = defaults.string(forKey: pushRegistrationKey(for: url, identityFingerprint: installationID))
        let scope = try key.bearerToken()
        let journal = try CommandJournal(serverURL: url, credentialScope: scope)
        snapshotCache = try SnapshotCache(serverURL: url, credentialScope: scope)
        self.journal = journal
        coordinator = CommandCoordinator(api: api, journal: journal)
        return api
    }

    @discardableResult
    private func send(kind: String, expectedVersion: Int64? = nil, payload: JSONValue) async -> Bool {
        guard !isBusy else { return false }
        guard let coordinator, let sourceAPI = api else { notice = uiText("Connect to a Zoe server first."); return false }
        let generation = connectionGeneration.value
        let idempotencyKey = UUID().uuidString.lowercased()
        let title = pendingCommandTitle(kind: kind, payload: payload)
        isBusy = true
        defer { isBusy = false }
        do {
            let outcome = try await coordinator.submit(kind: kind, expectedVersion: expectedVersion, payload: payload, workspaceID: snapshot?.workspaceId, idempotencyKey: idempotencyKey)
            guard isCurrentConnection(generation, sourceAPI: sourceAPI) else { return false }
            commandTitleByKey[outcome.receipt.idempotencyKey] = title
            commandStatusByKey[outcome.receipt.idempotencyKey] = .acceptedAwaitingSnapshot
            await reloadPendingCommandSummaries()
            do { try await refreshSnapshot() }
            catch {
                guard isCurrentConnection(generation, sourceAPI: sourceAPI) else { return false }
                setStale(uiText("The command was accepted, but the latest snapshot could not be loaded."))
            }
            guard isCurrentConnection(generation, sourceAPI: sourceAPI) else { return false }
            if kind == "task.stop" {
                notice = uiFormat("Stop intent recorded (receipt %@). Awaiting server confirmation that the runtime terminated.", outcome.receipt.commandId)
            } else {
                notice = uiFormat("%@ accepted by the server (receipt %@).", title, outcome.receipt.commandId)
            }
            return true
        } catch {
            if isCurrentConnection(generation, sourceAPI: sourceAPI) {
                commandTitleByKey[idempotencyKey] = title
                if isNeedsReview(error) { commandStatusByKey[idempotencyKey] = .needsReview }
                notice = userMessage(error)
                await reloadPendingCommandSummaries()
                if isNeedsReview(error) { try? await refreshSnapshot() }
            }
            return false
        }
    }

    public func checkPendingCommand(_ summary: PendingCommandSummary) async {
        guard !isBusy else { return }
        guard summary.state == .waitingForConfirmation || summary.state == .differentWorkspace else { return }
        guard summary.state != .differentWorkspace,
              let coordinator,
              let api,
              let workspaceID = snapshot?.workspaceId else {
            notice = uiText("Refresh the server and choose the workspace that owns this saved action before checking it.")
            return
        }
        let generation = connectionGeneration.value
        isBusy = true
        defer { isBusy = false }
        commandStatusByKey[summary.id] = .checkingStatus
        commandTitleByKey[summary.id] = summary.title
        await reloadPendingCommandSummaries()
        do {
            let outcome = try await coordinator.retryPending(idempotencyKey: summary.id, currentWorkspaceID: workspaceID)
            guard isCurrentConnection(generation, sourceAPI: api) else { return }
            reconcileTaskOutcome(outcome)
            commandStatusByKey[summary.id] = .acceptedAwaitingSnapshot
            notice = uiFormat("%@ was accepted. Refreshing the latest task state.", summary.title)
            await reloadPendingCommandSummaries()
            do { try await refreshSnapshot() }
            catch {
                guard isCurrentConnection(generation, sourceAPI: api) else { return }
                setStale(uiText("The action was accepted, but the latest snapshot could not be loaded."))
            }
            if let taskID = selectedTaskID, outcome.receipt.kind.hasPrefix("task.") { await loadTaskEntries(taskID: taskID) }
            if outcome.receipt.kind.hasPrefix("memory.") {
                await reconcileRecoveredMemoryOutcome(outcome, expectedGeneration: generation, sourceAPI: api)
            }
        } catch {
            guard isCurrentConnection(generation, sourceAPI: api) else { return }
            if isNeedsReview(error) {
                commandStatusByKey[summary.id] = .needsReview
                notice = uiFormat("%@ was not applied because the server state changed. Review the refreshed task and make a new choice.", summary.title)
                try? await refreshSnapshot()
            } else {
                commandStatusByKey[summary.id] = .waitingForConfirmation
                notice = uiFormat("Zoe could not confirm whether %@ was applied. The saved action is unchanged; check status again when connected.", summary.title)
            }
            await reloadPendingCommandSummaries()
        }
    }

    private func refreshSnapshot() async throws {
        #if DEBUG
        let testFetch = snapshotFetchOverrideForTesting
        #else
        let testFetch: (() async throws -> Snapshot)? = nil
        #endif
        let capturedAPI = api
        guard capturedAPI != nil || testFetch != nil else { throw ZoeError.invalidServerURL }
        let generation = connectionGeneration.value
        let cache = snapshotCache
        let latest: Snapshot
        do {
            if let testFetch { latest = try await testFetch() }
            else if let capturedAPI { latest = try await capturedAPI.snapshot() }
            else { throw ZoeError.invalidServerURL }
        }
        catch {
            if generation == connectionGeneration.value {
                if exportArtifact != nil {
                    cancelExport(status: uiText("Could not confirm current server authority. The prepared export was removed."))
                }
                if let failure = error as? ZoeError,
                   case let .http(status, _, _, _) = failure,
                   status == 401 || status == 403 {
                    clearMemoryAndPreparedExport(status: uiText("Server access changed; cached memory and the prepared export were cleared."))
                }
                if Self.isAccessDenied(error) {
                    lockWorkspace(clearDrafts: false)
                    authentication = .disconnected
                }
            }
            throw error
        }
        guard generation == connectionGeneration.value,
              testFetch != nil || capturedAPI === self.api else { return }
        if let current = snapshot, !cursor(latest.eventCursor, isAtLeast: current.eventCursor) {
            guard Self.authorityIdentityChanged(from: current.authority, to: latest.authority) else {
                throw ZoeError.invalidResponse
            }
        }
        if let current = snapshot, current.authority != latest.authority {
            if Self.authorityIdentityChanged(from: current.authority, to: latest.authority) { clearTaskContext(clearDrafts: true) }
            clearMemoryAndPreparedExport(status: uiText("Server authority changed; cached memory and the prepared export were cleared."))
        }
        snapshot = latest
        if let selectedTaskID, !latest.tasks.contains(where: { $0.id == selectedTaskID }) {
            clearTaskHistory()
            taskEntriesError = uiText("This task is no longer available. Refresh access or choose New task.")
        }
        if let artifact = exportArtifact, !Self.exportAuthorityMatches(artifact, snapshot: latest) {
            cancelExport(status: uiText("Server authority changed; the prepared export was removed. Prepare a fresh copy."))
        }
        try cache?.save(latest)
        freshness = .current(serverVersion: latest.serverVersion, cursor: latest.eventCursor)
        commandStatusByKey = commandStatusByKey.filter { $0.value == .needsReview }
        commandTitleByKey = commandTitleByKey.filter { commandStatusByKey[$0.key] == .needsReview }
        await reloadPendingCommandSummaries()
        canRegisterPushToken = true
        if pushNotificationsEnabled { await registerPendingAPNSToken() }
        else { await revokePendingPushToken() }
    }

    private var pushRegistrationKey: String {
        pushRegistrationKey(for: configuredServerURL ?? URL(string: serverAddress) ?? URL(fileURLWithPath: "/"), identityFingerprint: pushIdentityFingerprint ?? "")
    }

    private func pushRegistrationKey(for url: URL, identityFingerprint: String) -> String {
        "zoe.apns.tokenRegistrationID.\(url.absoluteString).\(identityFingerprint)"
    }

    private var pushRevocationPendingKey: String {
        pushRevocationPendingKey(for: configuredServerURL ?? URL(string: serverAddress) ?? URL(fileURLWithPath: "/"), identityFingerprint: pushIdentityFingerprint ?? "")
    }

    private func pushRevocationPendingKey(for url: URL, identityFingerprint: String) -> String {
        "zoe.apns.tokenRevocationPending.\(url.absoluteString).\(identityFingerprint)"
    }

    private func pushRegistrationOperationKey(for url: URL, identityFingerprint: String) -> String {
        "zoe.apns.tokenRegistrationOperation.\(url.absoluteString).\(identityFingerprint)"
    }

    private func pushRegistrationTokenDigestKey(for url: URL, identityFingerprint: String) -> String {
        "zoe.apns.tokenRegistrationDigest.\(url.absoluteString).\(identityFingerprint)"
    }

    private func pushRegisteredTokenDigestKey(for url: URL, identityFingerprint: String) -> String {
        "zoe.apns.registeredTokenDigest.\(url.absoluteString).\(identityFingerprint)"
    }

    private func pushRevocationOperationKey(for url: URL, identityFingerprint: String, tokenID: String) -> String {
        let tokenIDDigest = SHA256.hash(data: Data(tokenID.utf8)).map { String(format: "%02x", $0) }.joined()
        return "zoe.apns.tokenRevocationOperation.\(url.absoluteString).\(identityFingerprint).\(tokenIDDigest)"
    }

    private func pushRevocationQueueKey(for url: URL, identityFingerprint: String) -> String {
        "zoe.apns.tokenRevocationQueue.\(url.absoluteString).\(identityFingerprint)"
    }

    private func pushIdempotencyKey(operation: String, origin: URL, identityFingerprint: String, binding: String, operationID: String = UUID().uuidString) -> String {
        let material = "zoe.apns.\(operation)\u{0}\(origin.absoluteString)\u{0}\(identityFingerprint)\u{0}\(binding)\u{0}\(operationID)"
        return SHA256.hash(data: Data(material.utf8)).map { String(format: "%02x", $0) }.joined()
    }

    private func receiveAPNSToken(_ data: Data) {
        guard pushNotificationsEnabled else { return }
        pendingAPNSTokenHex = data.map { String(format: "%02x", $0) }.joined()
        Task { await registerPendingAPNSToken() }
    }

    private func registerPendingAPNSToken() async {
        guard pushNotificationsEnabled, canRegisterPushToken,
              let tokenHex = pendingAPNSTokenHex, tokenHex.count == 64,
              tokenHex != lastRegisteredAPNSTokenHex,
              tokenHex != tokenRegistrationInFlight, let api else { return }
        let origin = api.serverURL
        let identityFingerprint = pushIdentityFingerprint ?? ""
        let tokenDigest = SHA256.hash(data: Data(tokenHex.utf8)).map { String(format: "%02x", $0) }.joined()
        if defaults.string(forKey: pushRegisteredTokenDigestKey(for: origin, identityFingerprint: identityFingerprint)) == tokenDigest {
            lastRegisteredAPNSTokenHex = tokenHex
            return
        }
        let generation = connectionGeneration.value
        let preferenceEpoch = pushPreferenceEpoch
        let registrationAPI = api
        let registrationOrigin = api.serverURL
        let operationKey = pushRegistrationOperationKey(for: registrationOrigin, identityFingerprint: identityFingerprint)
        let tokenDigestKey = pushRegistrationTokenDigestKey(for: registrationOrigin, identityFingerprint: identityFingerprint)
        tokenRegistrationInFlight = tokenHex
        defer { if tokenRegistrationInFlight == tokenHex { tokenRegistrationInFlight = nil } }
        if defaults.string(forKey: operationKey) != nil {
            switch await recoverPendingAPNSRegistration(api: registrationAPI, origin: registrationOrigin, identityFingerprint: identityFingerprint) {
            case .completed: return
            case .retryLater: return
            case .none: break
            }
            guard generation == connectionGeneration.value,
                  registrationAPI === self.api,
                  registrationOrigin == configuredServerURL,
                  identityFingerprint == pushIdentityFingerprint else {
                return
            }
        }
        let idempotencyKey = defaults.string(forKey: operationKey) ?? pushIdempotencyKey(operation: "register", origin: registrationOrigin, identityFingerprint: identityFingerprint, binding: tokenHex)
        defaults.set(idempotencyKey, forKey: operationKey)
        defaults.set(SHA256.hash(data: Data(tokenHex.utf8)).map { String(format: "%02x", $0) }.joined(), forKey: tokenDigestKey)
        do {
            let receipt = try await registrationAPI.registerPushToken(platform: "\(pushPlatform)", tokenHex: tokenHex, idempotencyKey: idempotencyKey)
            guard preferenceEpoch == pushPreferenceEpoch, pushNotificationsEnabled,
                  generation == connectionGeneration.value, registrationAPI === self.api else {
                if let resourceID = receipt.resourceId {
                    await revokeAcceptedPushToken(resourceID, api: registrationAPI, origin: registrationOrigin, identityFingerprint: identityFingerprint)
                }
                if pushNotificationsEnabled { Task { await self.registerPendingAPNSToken() } }
                return
            }
            if let resourceID = receipt.resourceId {
                registeredPushTokenID = resourceID
                defaults.set(resourceID, forKey: pushRegistrationKey)
            }
            if receipt.resourceId != nil {
                defaults.removeObject(forKey: operationKey)
                defaults.set(defaults.string(forKey: tokenDigestKey) ?? tokenDigest, forKey: pushRegisteredTokenDigestKey(for: registrationOrigin, identityFingerprint: identityFingerprint))
                defaults.removeObject(forKey: tokenDigestKey)
                defaults.removeObject(forKey: pushRevocationPendingKey(for: registrationOrigin, identityFingerprint: identityFingerprint))
                lastRegisteredAPNSTokenHex = tokenHex
            }
        } catch {
            if preferenceEpoch != pushPreferenceEpoch || !pushNotificationsEnabled {
                // The server may have accepted the request before the connection failed. Retry with
                // the same scoped idempotency key to recover its receipt, then revoke that resource.
                do {
                    let receipt = try await registrationAPI.registerPushToken(platform: "\(pushPlatform)", tokenHex: tokenHex, idempotencyKey: idempotencyKey)
                    if let resourceID = receipt.resourceId {
                        await revokeAcceptedPushToken(resourceID, api: registrationAPI, origin: registrationOrigin, identityFingerprint: identityFingerprint)
                    }
                } catch {
                    defaults.set(true, forKey: pushRevocationPendingKey(for: registrationOrigin, identityFingerprint: identityFingerprint))
                }
            } else if generation == connectionGeneration.value {
                notice = uiText("The device could not register for push wakeups.")
            }
        }
    }

    private func revokeAcceptedPushToken(_ tokenID: String, api: APIClient, origin: URL, identityFingerprint: String) async {
        let registrationKey = pushRegistrationKey(for: origin, identityFingerprint: identityFingerprint)
        let pendingKey = pushRevocationPendingKey(for: origin, identityFingerprint: identityFingerprint)
        let queueKey = pushRevocationQueueKey(for: origin, identityFingerprint: identityFingerprint)
        var queue = (defaults.string(forKey: queueKey).flatMap { try? JSONDecoder().decode([String].self, from: Data($0.utf8)) }) ?? []
        if !queue.contains(tokenID) { queue.append(tokenID) }
        defaults.set(String(data: (try? JSONEncoder().encode(queue)) ?? Data("[]".utf8), encoding: .utf8) ?? "[]", forKey: queueKey)
        defaults.removeObject(forKey: pushRegistrationOperationKey(for: origin, identityFingerprint: identityFingerprint))
        defaults.removeObject(forKey: pushRegistrationTokenDigestKey(for: origin, identityFingerprint: identityFingerprint))
        defaults.set(true, forKey: pendingKey)
        let operationKey = pushRevocationOperationKey(for: origin, identityFingerprint: identityFingerprint, tokenID: tokenID)
        let idempotencyKey = defaults.string(forKey: operationKey) ?? pushIdempotencyKey(operation: "revoke", origin: origin, identityFingerprint: identityFingerprint, binding: tokenID)
        defaults.set(idempotencyKey, forKey: operationKey)
        do {
            _ = try await api.revokePushToken(tokenID: tokenID, idempotencyKey: idempotencyKey)
            defaults.removeObject(forKey: operationKey)
            queue.removeAll { $0 == tokenID }
            if queue.isEmpty { defaults.removeObject(forKey: queueKey) }
            else { defaults.set(String(data: (try? JSONEncoder().encode(queue)) ?? Data("[]".utf8), encoding: .utf8) ?? "[]", forKey: queueKey) }
            if defaults.string(forKey: registrationKey) == tokenID {
                defaults.removeObject(forKey: registrationKey)
                defaults.removeObject(forKey: pushRegisteredTokenDigestKey(for: origin, identityFingerprint: identityFingerprint))
            }
            if queue.isEmpty { defaults.removeObject(forKey: pendingKey) }
            if configuredServerURL == origin, pushIdentityFingerprint == identityFingerprint {
                if registeredPushTokenID == tokenID { registeredPushTokenID = nil }
                if queue.isEmpty { lastRegisteredAPNSTokenHex = nil }
                if !pushNotificationsEnabled { notice = uiText("Push wakeups are disabled on this device.") }
            }
        } catch {
            if configuredServerURL == origin, pushIdentityFingerprint == identityFingerprint {
                notice = uiText("Push wakeups are disabled locally, but the server could not confirm token revocation.")
            }
        }
    }

    private func revokePendingPushToken() async {
        guard !pushNotificationsEnabled, canRegisterPushToken, let api else { return }
        if defaults.string(forKey: pushRegistrationOperationKey(for: api.serverURL, identityFingerprint: pushIdentityFingerprint ?? "")) != nil {
            _ = await recoverPendingAPNSRegistration(api: api, origin: api.serverURL, identityFingerprint: pushIdentityFingerprint ?? "")
        }
        guard defaults.bool(forKey: pushRevocationPendingKey) else { return }
        let origin = api.serverURL
        let identityFingerprint = pushIdentityFingerprint ?? ""
        var tokenIDs = (defaults.string(forKey: pushRevocationQueueKey(for: origin, identityFingerprint: identityFingerprint)).flatMap { try? JSONDecoder().decode([String].self, from: Data($0.utf8)) }) ?? []
        if let tokenID = registeredPushTokenID, !tokenIDs.contains(tokenID) { tokenIDs.append(tokenID) }
        if tokenIDs.isEmpty {
            defaults.removeObject(forKey: pushRevocationPendingKey)
            return
        }
        for tokenID in tokenIDs {
            await revokeAcceptedPushToken(tokenID, api: api, origin: origin, identityFingerprint: identityFingerprint)
        }
        guard defaults.bool(forKey: pushRevocationPendingKey) else { return }
        let generation = connectionGeneration.value
        guard generation == connectionGeneration.value, api === self.api else { return }
    }

    private enum PendingRegistrationRecovery {
        case none
        case completed
        case retryLater
    }

    private func recoverPendingAPNSRegistration(api: APIClient, origin: URL, identityFingerprint: String) async -> PendingRegistrationRecovery {
        let operationKey = pushRegistrationOperationKey(for: origin, identityFingerprint: identityFingerprint)
        let tokenDigestKey = pushRegistrationTokenDigestKey(for: origin, identityFingerprint: identityFingerprint)
        guard let idempotencyKey = defaults.string(forKey: operationKey) else { return .none }
        let generation = connectionGeneration.value
        let preferenceEpoch = pushPreferenceEpoch
        do {
            let receipt = try await api.receipt(idempotencyKey: idempotencyKey)
            defaults.removeObject(forKey: operationKey)
            let registeredDigest = defaults.string(forKey: tokenDigestKey)
            defaults.removeObject(forKey: tokenDigestKey)
            guard let resourceID = receipt.resourceId else { return .completed }
            let contextIsCurrent = generation == connectionGeneration.value
                && api === self.api
                && origin == configuredServerURL
                && identityFingerprint == pushIdentityFingerprint
                && preferenceEpoch == pushPreferenceEpoch
            let currentTokenDigest = pendingAPNSTokenHex.map { SHA256.hash(data: Data($0.utf8)).map { String(format: "%02x", $0) }.joined() }
            if contextIsCurrent, pushNotificationsEnabled, registeredDigest != nil, registeredDigest == currentTokenDigest {
                registeredPushTokenID = resourceID
                defaults.set(resourceID, forKey: pushRegistrationKey(for: origin, identityFingerprint: identityFingerprint))
                defaults.set(registeredDigest!, forKey: pushRegisteredTokenDigestKey(for: origin, identityFingerprint: identityFingerprint))
                lastRegisteredAPNSTokenHex = pendingAPNSTokenHex
                defaults.removeObject(forKey: pushRevocationPendingKey(for: origin, identityFingerprint: identityFingerprint))
                return .completed
            }
            await revokeAcceptedPushToken(resourceID, api: api, origin: origin, identityFingerprint: identityFingerprint)
            return .none
        } catch let ZoeError.http(status, _, _, _) where status == 404 {
            defaults.removeObject(forKey: operationKey)
            defaults.removeObject(forKey: tokenDigestKey)
            let queueKey = pushRevocationQueueKey(for: origin, identityFingerprint: identityFingerprint)
            let registrationKey = pushRegistrationKey(for: origin, identityFingerprint: identityFingerprint)
            if defaults.string(forKey: queueKey) == nil,
               defaults.string(forKey: registrationKey) == nil {
                defaults.removeObject(forKey: pushRevocationPendingKey(for: origin, identityFingerprint: identityFingerprint))
            }
            return .none
        } catch {
            return .retryLater
        }
    }

    private var pushPlatform: String {
        #if os(iOS)
        "ios"
        #elseif os(macOS)
        "macos"
        #else
        "ios"
        #endif
    }

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
        await handleVoiceMediaLoss(.leaseRenewalFailed(message), mediaEpoch: mediaEpoch)
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
        if type == "voice.end" || type == "voice_end" ||
            ((type == "function_call" || type == "tool_call") && ((event["name"] as? String) == "voice.end" || (event["name"] as? String) == "voice_end")) {
            Task { @MainActor [weak self] in await self?.endVoiceSession() }
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

    private func listen(using api: APIClient) async {
        let generation = connectionGeneration.value
        let stream = EventStreamClient(api: api)
        var reconnecting = false
        while !Task.isCancelled && generation == connectionGeneration.value {
            do {
                if reconnecting {
                    try await refreshSnapshot()
                    guard isCurrentConnection(generation, sourceAPI: api), !Task.isCancelled else { return }
                    if let taskID = selectedTaskID { await loadTaskEntries(taskID: taskID) }
                }
                reconnecting = true
                let next = try await stream.run(after: snapshot?.eventCursor) { [weak self] push in
                    guard let self else { return }
                    guard !Task.isCancelled else { return }
                    await self.handle(push, expectedGeneration: generation, sourceAPI: api)
                }
                if Task.isCancelled || generation != connectionGeneration.value { return }
                if let next, snapshot?.eventCursor != next {
                    try await refreshSnapshot()
                }
                streamBackoff = 1
                setStale(uiText("The event stream ended; reconnecting."))
            } catch {
                if Task.isCancelled || generation != connectionGeneration.value { return }
                if Self.isAccessDenied(error) {
                    lockWorkspace(clearDrafts: false)
                    authentication = .disconnected
                    setOffline(error)
                    return
                }
                if snapshot == nil { setOffline(error) } else { setStale(uiText("Connection interrupted.")) }
            }
            let delay = streamBackoff
            streamBackoff = min(streamBackoff * 2, 30)
            try? await Task.sleep(nanoseconds: delay * 1_000_000_000)
        }
    }

    private func handle(_ push: ServerPush, expectedGeneration: UInt64, sourceAPI: APIClient?) async {
        guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI), !Task.isCancelled else { return }
        switch push {
        case let .event(envelope):
            if let artifact = exportArtifact, !Self.matchesExportAudit(envelope, artifact: artifact) {
                cancelExport(status: uiText("A server change may have changed export access. The prepared copy was removed."))
            }
            let memoryChanged = envelope.aggregateType == "memory" && ["memory.revised", "memory.deleted", "memory.proposal_accepted"].contains(envelope.type)
            if memoryChanged {
                await handleMemoryOrAuthorizationChange(status: uiText("Server memory changed; prepare a fresh export."))
                guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI), !Task.isCancelled else { return }
            }
            setStale(uiText("A server change arrived; refreshing the authoritative snapshot."))
            do { try await refreshSnapshot() }
            catch {
                guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI), !Task.isCancelled else { return }
                setStale(uiText("Snapshot refresh failed."))
            }
            guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI), !Task.isCancelled else { return }
            if envelope.aggregateType == "task", envelope.aggregateId == selectedTaskID {
                await loadTaskEntries(taskID: envelope.aggregateId)
            }
            if envelope.type.hasPrefix("notification.") { await refreshNotifications() }
        case .snapshotRequired:
            clearTaskHistory()
            clearMemoryAndPreparedExport(status: uiText("Server history expired; cached memory and the prepared export were cleared."))
            setStale(uiText("The saved event cursor expired; loading a fresh snapshot."))
            do { try await refreshSnapshot() }
            catch {
                guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI), !Task.isCancelled else { return }
                setStale(uiText("Snapshot repair failed."))
            }
            guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI), !Task.isCancelled else { return }
            if let taskID = selectedTaskID { await loadTaskEntries(taskID: taskID) }
            await refreshMemory()
        }
    }

    private func isCurrentConnection(_ generation: UInt64, sourceAPI: APIClient?) -> Bool {
        generation == connectionGeneration.value && (sourceAPI == nil || sourceAPI === api)
    }

    private func handleMemoryOrAuthorizationChange(status: String) async {
        clearMemoryAndPreparedExport(status: status)
        await refreshMemory()
    }

    private func recoverPendingCommands(expectedGeneration: UInt64, sourceAPI: APIClient, coordinator: CommandCoordinator, workspaceID: String?) async {
        guard !isBusy else { return }
        isBusy = true
        defer { isBusy = false }
        do {
            let recovered = try await coordinator.recoverPending(currentWorkspaceID: workspaceID, enforceWorkspace: true, onOutcome: { [weak self] outcome in
                guard let self, self.isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI) else { return }
                self.reconcileTaskOutcome(outcome)
                self.commandTitleByKey[outcome.receipt.idempotencyKey] = self.pendingCommandTitle(kind: outcome.command.kind, payload: outcome.command.payload)
                self.commandStatusByKey[outcome.receipt.idempotencyKey] = .acceptedAwaitingSnapshot
                await self.reloadPendingCommandSummaries()
                await self.reconcileRecoveredMemoryOutcome(outcome, expectedGeneration: expectedGeneration, sourceAPI: sourceAPI)
            }, onWorkspaceMismatch: { [weak self] command in
                guard let self, self.isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI) else { return }
                self.commandTitleByKey[command.idempotencyKey] = self.pendingCommandTitle(kind: command.kind, payload: command.payload)
                self.commandStatusByKey[command.idempotencyKey] = .differentWorkspace
                await self.reloadPendingCommandSummaries()
            }, onFailure: { [weak self] command, error in
                guard let self, self.isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI) else { return }
                self.commandTitleByKey[command.idempotencyKey] = self.pendingCommandTitle(kind: command.kind, payload: command.payload)
                self.commandStatusByKey[command.idempotencyKey] = self.isNeedsReview(error) ? .needsReview : .waitingForConfirmation
                await self.reloadPendingCommandSummaries()
            })
            guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI) else { return }
            guard !recovered.isEmpty else { return }
            notice = uiFormat("Recovered %lld command receipt(s) from this device's durable queue.", Int64(recovered.count))
            do { try await refreshSnapshot() }
            catch {
                guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI) else { return }
                setStale(uiText("Recovered commands, but the latest snapshot could not be loaded."))
            }
            guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI) else { return }
            if let taskID = selectedTaskID { await loadTaskEntries(taskID: taskID) }
        } catch {
            if isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI) {
                notice = isNeedsReview(error) ? uiText("A saved action needs review. The original action was not rewritten; check the updated server state before choosing again.") : userMessage(error)
                if isNeedsReview(error) { try? await refreshSnapshot() }
                await reloadPendingCommandSummaries()
            }
        }
    }

    private func reloadPendingCommandSummaries() async {
        guard let journal else { pendingCommandSummaries = []; return }
        let expectedGeneration = connectionGeneration.value
        let sourceAPI = api
        let commands: [PendingCommand]
        #if DEBUG
        if let readOverride = pendingCommandReadOverrideForTesting {
            commands = (try? await readOverride()) ?? []
        } else {
            commands = (try? await journal.all()) ?? []
        }
        #else
        commands = (try? await journal.all()) ?? []
        #endif
        guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI), self.journal === journal else { return }
        let currentWorkspaceID = snapshot?.workspaceId
        var restoredTaskIntents: [String: TaskIntent] = [:]
        for command in commands where currentWorkspaceID != nil && command.workspaceID == currentWorkspaceID {
            let context: String
            if command.kind == "task.create" { context = "new-task" }
            else if command.kind == "task.append", case let .object(fields) = command.payload,
                    case let .string(taskID)? = fields["task_id"] { context = "task:\(taskID)" }
            else { continue }
            if restoredTaskIntents[context] == nil { restoredTaskIntents[context] = TaskIntent(key: command.idempotencyKey) }
        }
        taskIntents = restoredTaskIntents
        var summaries = commands.map { command in
            let state: PendingCommandSummary.State
            if currentWorkspaceID == nil { state = .workspaceUnavailable }
            else if command.workspaceID == nil || command.workspaceID != currentWorkspaceID { state = .differentWorkspace }
            else { state = commandStatusByKey[command.idempotencyKey] ?? .waitingForConfirmation }
            let title = commandTitleByKey[command.idempotencyKey] ?? pendingCommandTitle(kind: command.kind, payload: command.payload)
            return PendingCommandSummary(id: command.idempotencyKey, title: title, state: state)
        }
        let pendingKeys = Set(commands.map(\.idempotencyKey))
        for (key, state) in commandStatusByKey where !pendingKeys.contains(key) && (state == .acceptedAwaitingSnapshot || state == .needsReview) {
            summaries.append(PendingCommandSummary(id: key, title: commandTitleByKey[key] ?? uiText("Saved action"), state: state))
        }
        pendingCommandSummaries = summaries
    }

    private func pendingCommandTitle(kind: String, payload: JSONValue) -> String {
        let object: [String: JSONValue]
        if case let .object(fields) = payload { object = fields } else { object = [:] }
        func string(_ key: String) -> String? { if case let .string(value)? = object[key] { return value }; return nil }
        let taskName = string("task_id").flatMap { task(id: $0)?.title }
        switch kind {
        case "task.create": return uiText("Create task")
        case "task.append": return taskName.map { uiFormat("Add entry to %@", $0) } ?? uiText("Add entry to task")
        case "task.stop": return taskName.map { uiFormat("Request stop for %@", $0) } ?? uiText("Request stop for task")
        case "task.approve": return string("decision") == "reject" ? uiText("Reject requested action") : uiText("Approve requested action")
        case "task.accept_result": return taskName.map { uiFormat("Review result for %@", $0) } ?? uiText("Review task result")
        case "project.create": return uiText("Create project")
        case "memory.remember": return uiText("Save memory")
        case "memory.delete": return uiText("Delete memory")
        default: return uiText("Submit saved action")
        }
    }

    private func isNeedsReview(_ error: Error?) -> Bool {
        guard let error = error as? ZoeError, case let .http(status, _, _, _) = error else { return false }
        return status == 409
    }

    private func reconcileRecoveredMemoryOutcome(_ outcome: CommandOutcome, expectedGeneration: UInt64, sourceAPI: APIClient) async {
        guard outcome.receipt.kind.hasPrefix("memory.") else { return }
        guard isCurrentConnection(expectedGeneration, sourceAPI: sourceAPI) else { return }
        clearMemoryAndPreparedExport(status: uiText("A recovered memory command changed server memory; cached memory and the prepared export were cleared."))
        await refreshMemory()
    }

    private func clearMemoryAndPreparedExport(status: String) {
        clearVisibleMemory()
        cancelExport(status: status)
    }

    private static func authorityIdentityChanged(from previous: SnapshotAuthority?, to latest: SnapshotAuthority?) -> Bool {
        guard let previous, let latest else { return previous != latest }
        return previous.instanceLineageId != latest.instanceLineageId || previous.generation != latest.generation
    }

    private static func exportAuthorityMatches(_ artifact: ScopedExportArtifact, snapshot: Snapshot?) -> Bool {
        guard let snapshot else { return false }
        guard let authority = snapshot.authority else {
            return artifact.validatedThroughCursor.isEmpty && artifact.snapshotCursor == snapshot.eventCursor
        }
        if !artifact.validatedThroughCursor.isEmpty {
            guard !artifact.exportID.isEmpty,
                  artifact.exportAuditEventID == artifact.exportID,
                  artifact.exportAuditEventType == "data.export.prepared",
                  artifact.exportAuditEventCursor == artifact.validatedThroughCursor,
                  let dataCursor = UInt64(artifact.snapshotCursor), dataCursor < UInt64.max,
                  String(dataCursor + 1) == artifact.validatedThroughCursor else { return false }
        }
        return artifact.instanceLineageID == authority.instanceLineageId
            && artifact.generation == authority.generation
            && artifact.deletionSequence == authority.deletionSequence
            && (artifact.validatedThroughCursor.isEmpty ? artifact.snapshotCursor : artifact.validatedThroughCursor) == snapshot.eventCursor
    }

    private static func matchesExportAudit(_ event: EventEnvelope, artifact: ScopedExportArtifact) -> Bool {
        !artifact.exportID.isEmpty
            && event.eventId == artifact.exportAuditEventID
            && event.cursor == artifact.exportAuditEventCursor
            && event.type == artifact.exportAuditEventType
            && event.aggregateType == "data_export"
            && event.aggregateId == artifact.exportID
    }

    private static func cleanupExpiredExportTemporaries() {
        APIClient.cleanupAbandonedExportDownloads()
        let directory = FileManager.default.temporaryDirectory
        guard let entries = try? FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil) else { return }
        for entry in entries where entry.lastPathComponent.hasPrefix("zoe-export-") && entry.pathExtension == "zip" {
            try? FileManager.default.removeItem(at: entry)
        }
    }

    private func clearVisibleMemory() {
        memoryRefreshSequence &+= 1
        pendingMemoryRefreshQuery = nil
        memoryClaims = []
        memoryMissing = []
        memoryConflicts = []
    }

    private static func isAccessDenied(_ error: Error) -> Bool {
        guard let failure = error as? ZoeError,
              case let .http(status, code, _, _) = failure else { return false }
        return status == 401 || (status == 403 && code == "authentication_required")
    }

    private func setStale(_ reason: String) {
        freshness = .stale(reason: reason, cursor: snapshot?.eventCursor ?? "")
    }

    private func setOffline(_ error: Error) {
        if Self.isAccessDenied(error), authentication.isConnected {
            lockWorkspace(clearDrafts: false)
            authentication = .disconnected
        }
        let message = userMessage(error)
        freshness = .offline(reason: message, cursor: snapshot?.eventCursor)
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
        uiText("Voice is unavailable right now. Text chat still works.")
    }

    private func cursor(_ candidate: String, isAtLeast baseline: String) -> Bool {
        guard candidate.allSatisfy(\.isNumber), baseline.allSatisfy(\.isNumber) else { return false }
        let left = candidate.drop(while: { $0 == "0" })
        let right = baseline.drop(while: { $0 == "0" })
        if left.count != right.count { return left.count > right.count }
        return left >= right
    }
}
