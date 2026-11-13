import Foundation

public struct Snapshot: Codable, Equatable, Sendable {
    public var serverVersion: String
    public var eventCursor: String
    public var authority: SnapshotAuthority? = nil
    public var executionCapacity: ExecutionCapacity? = nil
    public var projects: [Project]
    public var tasks: [TaskRecord]
    public var approvals: [Approval]
    public var workspaceId: String
    public var subjectId: String
    public var runs: [Run]

    public init(serverVersion: String, eventCursor: String, projects: [Project], tasks: [TaskRecord], approvals: [Approval], runs: [Run], workspaceId: String = "default", subjectId: String = "owner", authority: SnapshotAuthority? = nil, executionCapacity: ExecutionCapacity? = nil) {
        self.serverVersion = serverVersion
        self.eventCursor = eventCursor
        self.authority = authority
        self.executionCapacity = executionCapacity
        self.projects = projects
        self.tasks = tasks
        self.approvals = approvals
        self.workspaceId = workspaceId
        self.subjectId = subjectId
        self.runs = runs
    }
}

public struct ExecutionCapacity: Codable, Equatable, Sendable {
    public var activeRuns: Int
    public var uncertainRuns: Int
    public var maxRuns: Int
    public var availableSlots: Int
    public var dispatchBlocked: Bool
    public var reasonCode: String?

    public var occupiedSlots: Int { activeRuns + uncertainRuns }
}

public struct SnapshotAuthority: Codable, Equatable, Sendable {
    public var instanceLineageId: String
    public var generation: Int64
    public var deletionSequence: String

    public init(instanceLineageId: String, generation: Int64, deletionSequence: String) {
        self.instanceLineageId = instanceLineageId
        self.generation = generation
        self.deletionSequence = deletionSequence
    }
}

public struct Project: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var name: String
    public var description: String?
    public var status: String
    public var version: Int64
    public var createdAt: String
    public var updatedAt: String
}

public struct TaskRecord: Codable, Equatable, Identifiable, Sendable {
    public var queueReason: String? = nil
    public var id: String
    public var projectId: String?
    public var title: String
    public var goal: String
    public var source: String
    public var profileId: String?
    public var status: TaskStatus
    public var version: Int64
    public var createdAt: String
    public var updatedAt: String
    public var acceptedAt: String?
    public var startedAt: String?
    public var finishedAt: String?
    public var result: String?
    public var error: String?
    public var evidence: [Evidence]
    public var canAcceptResult: Bool
}

public enum TaskStatus: String, Codable, CaseIterable, Sendable {
    case queued, active, waiting, awaitingUser = "awaiting_user", paused, complete, failed, canceled

    /// The control plane rejects appended entries after a task reaches a terminal state.
    public var canAcceptEntries: Bool {
        self != .complete && self != .failed && self != .canceled
    }

    public var label: String {
        switch self {
        case .queued: "Queued"
        case .active: "Active"
        case .waiting: "Waiting"
        case .awaitingUser: "Awaiting your review"
        case .paused: "Paused"
        case .complete: "Complete"
        case .failed: "Failed"
        case .canceled: "Canceled"
        }
    }
}

public struct Evidence: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var kind: String
    public var summary: String
    public var source: String
    public var createdAt: String
    public var verified: Bool
}

public struct Approval: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var taskId: String
    public var status: String
    public var prompt: String
    public var version: Int64
    public var createdAt: String
    public var expiresAt: String?
    public var requestedAction: JSONValue?
    public var requestedActionDigest: String?
}

public struct Run: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var taskId: String
    public var attemptId: String
    public var status: String
    public var desiredControl: String
    public var controlVersion: Int64
    public var startedAt: String
    public var stoppedAt: String?
    public var evidence: [Evidence]
    public var terminationConfirmed: Bool
}

public struct NotificationRecord: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var kind: String
    public var title: String
    public var body: String
    public var aggregateType: String
    public var aggregateId: String
    public var sourceEventId: String
    public var createdAt: String
    public var readAt: String?
}

public struct NotificationList: Codable, Equatable, Sendable {
    public var notifications: [NotificationRecord]
}

public struct CapabilityReleaseList: Codable, Equatable, Sendable {
    public var items: [CapabilityRelease]
}

public struct CapabilityRelease: Codable, Equatable, Identifiable, Sendable {
    public var id: String { releaseId }
    public var releaseId: String
    public var digest: String
    public var manifest: CapabilityManifest
    public var createdAt: String
}

public struct CapabilityManifest: Codable, Equatable, Sendable {
    public var schemaVersion: Int
    public var artifactRef: String
    public var artifactDigest: String
    public var entrypoint: String
    public var args: [String]?
    public var secretRefs: [String: String]?
    public var mounts: [CapabilityMount]?
    public var capabilities: [String]
    public var triggers: [CapabilityTrigger]?
    public var budget: CapabilityResourceBudget
    public var network: CapabilityNetworkPolicy
}

public struct CapabilityMount: Codable, Equatable, Sendable {
    public var volumeId: String
    public var target: String
    public var readOnly: Bool
}

public struct CapabilityTrigger: Codable, Equatable, Sendable {
    public var kind: String
    public var source: String
    public var filterJson: String?
}

public struct CapabilityResourceBudget: Codable, Equatable, Sendable {
    public var cpuMillis: Int64
    public var memoryBytes: Int64
    public var diskBytes: Int64
    public var timeoutSeconds: Int64
    public var concurrency: Int
}

public struct CapabilityNetworkPolicy: Codable, Equatable, Sendable {
    public var mode: String
    public var allowedHosts: [String]?
}

public struct CapabilityDeploymentList: Codable, Equatable, Sendable {
    public var items: [CapabilityDeployment]
}

public struct CapabilityDeployment: Codable, Equatable, Identifiable, Sendable {
    public var id: String { deploymentId }
    public var deploymentId: String
    public var workspaceId: String
    public var serviceId: String
    public var releaseDigest: String
    public var requestedOperation: String
    public var desiredState: String
    public var observedState: String
    public var grantId: String
    public var configRef: String
    public var configSha256: String
    public var version: Int64
    public var createdBy: String
    public var createdAt: String
    public var updatedAt: String
    public var latestAttemptId: String
    public var latestFence: Int64
    public var lastErrorCode: String
    public var revokedAt: String?
    public var revokedBy: String?
    public var revocationCleanupState: String?

    public var revocationCleanupStatus: CapabilityRevocationCleanupStatus? {
        guard revokedAt != nil else { return nil }
        switch revocationCleanupState {
        case "pending": return .pending(observedState: observedState)
        case "uncertain": return .uncertain
        case "confirmed": return .confirmed
        default: return .unknown(observedState: observedState)
        }
    }
}

public enum CapabilityRevocationCleanupStatus: Equatable, Sendable {
    case pending(observedState: String)
    case confirmed
    case unknown(observedState: String)
    case uncertain
}

public extension Array where Element == CapabilityDeployment {
    /// Returns the version bound to an existing service request, if the current
    /// inventory contains that service. The server uses it to reject stale UI intent.
    func version(forServiceID serviceID: String) -> Int64? {
        first(where: { $0.serviceId == serviceID })?.version
    }
}

public struct VoiceConfig: Codable, Equatable, Sendable {
    public var version: String
    public var defaults: VoiceDefaults
    public var voices: [VoiceChoice]
    public var voiceModes: [VoiceChoice]
    public var languages: [VoiceLanguage]
    public var webRTC: VoiceWebRTCConfig

    private enum CodingKeys: String, CodingKey {
        case version
        case defaults
        case voices
        case voiceModes
        case languages
        case webRTC = "webrtc"
    }
}

public struct VoiceDefaults: Codable, Equatable, Sendable {
    public var voice: String
    public var voiceMode: String
    public var languageCode: String
}

public struct VoiceChoice: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var name: String
    public var description: String?
}

public struct VoiceLanguage: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var nameZh: String
    public var nameEn: String
}

public struct VoiceWebRTCConfig: Codable, Equatable, Sendable {
    public var dataChannel: VoiceDataChannelConfig
    public var iceServers: [VoiceICEServer]
    public var receiveAudio: Bool
    public var receiveVideo: Bool
}

public struct VoiceDataChannelConfig: Codable, Equatable, Sendable {
    public var label: String
    public var negotiated: Bool
    public var id: Int
}

public struct VoiceICEServer: Codable, Equatable, Sendable {
    public var urls: [String]
}

public struct VoiceSessionResponse: Codable, Equatable, Sendable {
    public var voiceSessionId: String
    public var generation: Int64
    public var state: String
    public var answerSdp: String
    public var accountLeaseId: String? = nil
    public var accountLeaseGeneration: Int64? = nil
}

public struct VoicePoolStatus: Codable, Equatable, Sendable {
    public var blocked: Bool
    public var reasonCode: String
    public var updatedAt: String
    public var accountLeaseIdentity: Bool
    public var accountScopedTermination: Bool
    public var trustedTerminationEvidence: Bool
}

public struct VoiceOperationResponse: Codable, Equatable, Sendable {
    public var ok: Bool
    public var state: String?
    public var upstreamTermination: String?
    public var poolBlocked: Bool?
}

public struct TaskEntry: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var taskId: String
    public var kind: String
    public var content: String
    public var createdAt: String
}

public struct TaskEntriesPage: Codable, Equatable, Sendable {
    public var messages: [TaskEntry]
    public var nextCursor: String?
}

public struct CommandReceipt: Codable, Equatable, Sendable {
    public var commandId: String
    public var idempotencyKey: String
    public var workspaceId: String
    public var kind: String
    public var status: String
    public var resourceType: String?
    public var resourceId: String?
    public var resourceVersion: Int64?
    public var acceptedAt: String
    public var eventCursor: String
}

public struct APIFailureBody: Codable, Sendable {
    public struct Failure: Codable, Sendable {
        public var code: String
        public var message: String
        public var details: [String: JSONValue]?
    }
    public var error: Failure
}

public enum ZoeError: Error, LocalizedError, Sendable {
    case invalidServerURL
    case missingCredential
    case credentialAccessDenied(String)
    case exportTooLarge
    case http(status: Int, code: String, message: String, details: [String: JSONValue]?)
    case invalidResponse
    case persistence(String)
    case pendingCommandWorkspaceMismatch

    public var errorDescription: String? {
        switch self {
        case .invalidServerURL: "Enter a valid Zoe server URL."
        case .missingCredential: "Enter your server address and instance key to connect."
        case let .credentialAccessDenied(storage): "Allow Zoe to access \(storage), then try again."
        case .exportTooLarge: "This export is larger than the 1 GiB download limit."
        case let .http(_, _, message, _): message
        case .invalidResponse: "The server returned a response the app could not read."
        case let .persistence(message): message
        case .pendingCommandWorkspaceMismatch: "This saved action belongs to a different workspace and was not retried."
        }
    }
}

public enum JSONValue: Codable, Equatable, Sendable {
    case string(String)
    case integer(Int64)
    case number(Double)
    case bool(Bool)
    case object([String: JSONValue])
    case array([JSONValue])
    case null

    public init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() { self = .null }
        else if let value = try? container.decode(Bool.self) { self = .bool(value) }
        else if let value = try? container.decode(Int64.self) { self = .integer(value) }
        else if let value = try? container.decode(Double.self) { self = .number(value) }
        else if let value = try? container.decode(String.self) { self = .string(value) }
        else if let value = try? container.decode([String: JSONValue].self) { self = .object(value) }
        else if let value = try? container.decode([JSONValue].self) { self = .array(value) }
        else { throw DecodingError.dataCorruptedError(in: container, debugDescription: "Unsupported JSON value") }
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case let .string(value): try container.encode(value)
        case let .integer(value): try container.encode(value)
        case let .number(value): try container.encode(value)
        case let .bool(value): try container.encode(value)
        case let .object(value): try container.encode(value)
        case let .array(value): try container.encode(value)
        case .null: try container.encodeNil()
        }
    }
}

public struct EventEnvelope: Codable, Equatable, Sendable {
    public var schemaVersion: Int
    public var eventId: String
    public var cursor: String
    public var type: String
    public var occurredAt: String
    public var workspaceId: String
    public var subjectId: String?
    public var aggregateType: String
    public var aggregateId: String
    public var aggregateVersion: Int64
    public var payload: JSONValue
}

public enum ClientFreshness: Equatable, Sendable {
    case loading
    case current(serverVersion: String, cursor: String)
    case stale(reason: String, cursor: String)
    case offline(reason: String, cursor: String?)
}



/// Workspace access is granted only after a successful authenticated snapshot.
public enum InstanceConnectionState: Equatable, Sendable {
    case checking, disconnected, unavailable, connected
    public var isConnected: Bool { self == .connected }
}
