import Foundation
import CryptoKit
import A2UISwiftCore

public struct UISurfaceEvent: Decodable, Sendable {
    public let requestID: String
    public let surfaceID: String
    public let container: SurfaceKind
    public let awaitingInput: Bool
    public let messages: [A2uiMessage]

    private enum CodingKeys: String, CodingKey {
        case requestID = "request_id", surfaceID = "surface_id", container
        case awaitingInput = "awaiting_input", messages
    }

    public var isDeletion: Bool {
        if messages.count == 1, case .deleteSurface = messages[0] { return true }
        return false
    }

    public static func decode(_ payload: JSONValue) -> UISurfaceEvent? {
        decodeWithDiagnostic(payload).event
    }

    public static func decodeWithDiagnostic(_ payload: JSONValue) -> (event: UISurfaceEvent?, failure: String?) {
        guard let data = try? JSONEncoder().encode(payload),
              let event = try? JSONDecoder().decode(Self.self, from: data) else { return (nil, "payload_decode") }
        guard !event.requestID.isEmpty else { return (nil, "request_id") }
        guard !event.surfaceID.isEmpty else { return (nil, "surface_id") }
        guard !event.messages.isEmpty else { return (nil, "messages_empty") }
        if !event.isDeletion {
            guard case .createSurface = event.messages[0] else { return (nil, "first_not_createSurface") }
            for message in event.messages.dropFirst() {
                switch message {
                case .createSurface, .deleteSurface: return (nil, "messages_sequence")
                default: break
                }
            }
        }
        for message in event.messages {
            let id: String
            switch message {
            case .createSurface(let p): id = p.surfaceId
            case .updateComponents(let p): id = p.surfaceId
            case .updateDataModel(let p): id = p.surfaceId
            case .deleteSurface(let p): id = p.surfaceId
            }
            guard id == event.surfaceID else { return (nil, "surface_id_mismatch") }
        }
        return (event, nil)
    }

    public var renderedPayload: JSONValue { .object(["request_id": .string(requestID)]) }

    public var diagnosticSummary: String {
        return "container=\(container.rawValue);messages=\(messages.count);awaiting_input=\(awaitingInput ? 1 : 0)"
    }

    /// Cursor values are identifiers, never arbitrary payload text.
    public static func receivedSummary(payload: JSONValue, cursor: String) -> String {
        let fields: [String: JSONValue]
        if case let .object(object) = payload { fields = object } else { fields = [:] }
        let container: String
        if case let .string(value) = fields["container"], SurfaceKind(rawValue: value) != nil { container = value } else { container = "unknown" }
        let count: Int
        if case let .array(messages) = fields["messages"] { count = messages.count } else { count = 0 }
        let awaiting: Int
        if case let .bool(value) = fields["awaiting_input"] { awaiting = value ? 1 : 0 } else { awaiting = 0 }
        // Do not copy an opaque server string into diagnostics: only numeric cursors are safe.
        let safeCursor = !cursor.isEmpty && cursor.allSatisfy({ $0.isASCII && $0.isNumber }) ? String(cursor.prefix(48)) : "opaque"
        return "container=\(container);messages=\(count);awaiting_input=\(awaiting);cursor=\(safeCursor)"
    }

    public func renderedIdempotencyKey(cursor: String, version: Int64) -> String {
        let identity = Data([requestID, cursor, String(version)].map { "\($0.utf8.count):\($0)" }.joined().utf8)
        return "ui-rendered-" + SHA256.hash(data: identity).map { String(format: "%02x", $0) }.joined()
    }
}

/// Tracks server ownership independently of whether the user dismissed a native container.
public struct UISurfaceRequests {
    public private(set) var requestIDs: [String: String] = [:]
    public init() {}
    public mutating func apply(_ event: UISurfaceEvent) {
        if event.isDeletion { requestIDs.removeValue(forKey: event.surfaceID) }
        else { requestIDs[event.surfaceID] = event.requestID }
    }
    public mutating func removeAll() { requestIDs.removeAll() }
}

public func uiActionPayload(requestID: String, surfaceID: String, action: ResolvedAction) throws -> JSONValue {
    let context = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(action.context))
    return .object([
        "request_id": .string(requestID), "surface_id": .string(surfaceID),
        "name": .string(action.name), "source_component_id": .string(action.sourceComponentId),
        "context": context
    ])
}
