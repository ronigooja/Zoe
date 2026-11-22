import Foundation

public struct Snapshot: Codable, Equatable, Sendable {
    public var serverVersion: String
    public var eventCursor: String
    public var workspaceId: String
    public var subjectId: String

    public init(serverVersion: String, eventCursor: String, workspaceId: String = "default", subjectId: String = "owner") {
        self.serverVersion = serverVersion
        self.eventCursor = eventCursor
        self.workspaceId = workspaceId
        self.subjectId = subjectId
    }
}

public struct VoiceConfig: Codable, Equatable, Sendable {
    public var version: String
    public var defaults: VoiceDefaults
    public var voices: [VoiceChoice]
    public var voiceModes: [VoiceChoice]
    public var languages: [VoiceLanguage]
    public var webRTC: VoiceWebRTCConfig
    public var gatewayDialOrigin: String?

    public var gatewayOrigin: URL? {
        gatewayDialOrigin.flatMap(Self.validateGatewayOrigin)
    }

    public static func validateGatewayOrigin(_ origin: String) -> URL? {
        guard !origin.contains("\\"), !origin.contains("%"),
              origin.rangeOfCharacter(from: .whitespacesAndNewlines.union(.controlCharacters)) == nil,
              var parts = URLComponents(string: origin), parts.scheme?.lowercased() == "https",
              let host = parts.host, !host.isEmpty, parts.user == nil, parts.password == nil,
              parts.percentEncodedPath.isEmpty, parts.query == nil, parts.fragment == nil,
              !origin.hasSuffix(":"), parts.port.map({ (1...65535).contains($0) }) ?? true else { return nil }
        parts.scheme = "https"
        parts.host = host.lowercased()
        if parts.port == 443 { parts.port = nil }
        return parts.url
    }

    /// Applies when starting a new call; unavailable preferences never reach the gateway.
    public func resolvedVoice(preferred: String?) -> String {
        if let preferred, voices.contains(where: { $0.id == preferred }) { return preferred }
        return defaults.voice
    }

    private enum CodingKeys: String, CodingKey {
        case version
        case defaults
        case voices
        case voiceModes
        case languages
        case webRTC = "webrtc"
        case gatewayDialOrigin
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

public struct VoiceDialRequest: Encodable, Sendable {
    public let requestId: String
    public let offerSdp: String
    public let voice: String?
    public let voiceMode: String?
    public let languageCode: String?

    public init(requestID: String, offerSDP: String, voice: String? = nil, voiceMode: String? = nil, languageCode: String? = nil) {
        requestId = requestID
        offerSdp = offerSDP
        self.voice = voice
        self.voiceMode = voiceMode
        self.languageCode = languageCode
    }
}

public struct VoiceDialResponse: Decodable, Equatable, Sendable {
    public let voiceSessionId: String
    public let generation: Int64
    public let gatewaySessionId: String
    public let answerSdp: String
}

public struct VoicePoolStatus: Codable, Equatable, Sendable {
    public var blocked: Bool
    public var reasonCode: String
    public var updatedAt: String
    public var accountLeaseIdentity: Bool
    public var accountScopedTermination: Bool
    public var trustedTerminationEvidence: Bool
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
    case http(status: Int, code: String, message: String, details: [String: JSONValue]?)
    case invalidResponse
    case persistence(String)
    case pendingCommandWorkspaceMismatch

    public var errorDescription: String? {
        switch self {
        case .invalidServerURL: "Enter a valid Zoe server URL."
        case .missingCredential: "Enter your server address and instance key to connect."
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

/// Workspace access is granted only after a successful authenticated snapshot.
public enum InstanceConnectionState: Equatable, Sendable {
    case checking, disconnected, unavailable, connected
    public var isConnected: Bool { self == .connected }
}
