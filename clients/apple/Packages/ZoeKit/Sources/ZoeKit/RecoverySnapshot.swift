import Foundation

/// A global checkpoint; unlike UISnapshot, this replaces all retained control state.
public struct RecoverySnapshot: Decodable, Sendable {
    public let cursor: String
    public let snapshot: RecoveryControlSnapshot
    public let ui: UISnapshot
    public let voice: RecoveryVoice
}

public struct RecoveryControlSnapshot: Decodable, Sendable {
    public let subjectID: String
    public let eventCursor: String
    public let serverVersion: String
    public let authority: JSONValue
    public var instanceLineageID: String? {
        guard case let .object(fields) = authority,
              case let .string(identity) = fields["instance_lineage_id"],
              !identity.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { return nil }
        return identity
    }
    public let projects: [JSONValue]
    public let tasks: [JSONValue]
    public let approvals: [JSONValue]
    public let runs: [JSONValue]
    public let executionCapacity: JSONValue
    enum CodingKeys: String, CodingKey {
        case subjectID = "subject_id"
        case eventCursor = "event_cursor", serverVersion = "server_version"
        case authority, projects, tasks, approvals, runs
        case executionCapacity = "execution_capacity"
    }
}

public struct RecoveryVoice: Decodable, Sendable {
    public struct Session: Decodable, Sendable {
        public let id: String
        public let generation: Int64
        public let state: String
    }
    public struct Pool: Decodable, Sendable {
        public let blocked: Bool
        public let reasonCode: String
        enum CodingKeys: String, CodingKey { case blocked, reasonCode = "reason_code" }
    }
    public let currentSession: Session?
    public let pool: Pool
    enum CodingKeys: String, CodingKey { case currentSession = "current_session", pool }
}
