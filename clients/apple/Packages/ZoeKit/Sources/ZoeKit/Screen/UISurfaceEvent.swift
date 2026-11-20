import Foundation
import CryptoKit
import A2UISwiftCore

public struct UISurfaceEvent: Decodable, Sendable {
    public let requestID: String
    public let surfaceID: String
    public let surfaceVersion: Int64
    public let taskID: String?
    public let questionID: String?
    public let container: SurfaceKind
    public let awaitingInput: Bool
    public let messages: [A2uiMessage]
    public let size: SurfaceSize?

    private enum CodingKeys: String, CodingKey {
        case requestID = "request_id", surfaceID = "surface_id", container
        case surfaceVersion = "surface_version", taskID = "task_id", questionID = "question_id"
        case awaitingInput = "awaiting_input", messages, size
    }

    public static func decode(_ payload: JSONValue) -> UISurfaceEvent? {
        decodeWithDiagnostic(payload).event
    }

    public static func decodeWithDiagnostic(_ payload: JSONValue) -> (event: UISurfaceEvent?, failure: String?) {
        guard let data = try? JSONEncoder().encode(payload),
              let event = try? JSONDecoder().decode(Self.self, from: data) else { return (nil, "payload_decode") }
        guard !event.requestID.isEmpty else { return (nil, "request_id") }
        guard !event.surfaceID.isEmpty else { return (nil, "surface_id") }
        guard event.surfaceVersion > 0 else { return (nil, "surface_version") }
        guard !event.awaitingInput || (event.taskID?.isEmpty == false && event.questionID?.isEmpty == false) else { return (nil, "input_binding") }
        guard !event.messages.isEmpty else { return (nil, "messages_empty") }
        guard case .createSurface = event.messages[0] else { return (nil, "first_not_createSurface") }
        for message in event.messages.dropFirst() {
            switch message {
            case .updateComponents, .updateDataModel: break
            default: return (nil, "messages_sequence")
            }
        }
        for message in event.messages {
            let id: String
            switch message {
            case .createSurface(let p): id = p.surfaceId
            case .updateComponents(let p): id = p.surfaceId
            case .updateDataModel(let p): id = p.surfaceId
            default: return (nil, "messages_sequence")
            }
            guard id == event.surfaceID else { return (nil, "surface_id_mismatch") }
        }
        return (event, nil)
    }

    public var binding: UISurfaceBinding {
        UISurfaceBinding(requestID: requestID, surfaceID: surfaceID, surfaceVersion: surfaceVersion)
    }

    public func renderedPayload(success: Bool) -> JSONValue {
        .object(["request_id": .string(requestID), "surface_id": .string(surfaceID),
                 "surface_version": .integer(surfaceVersion), "success": .bool(success)])
    }

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

    public var renderedIdempotencyKey: String {
        let identity = Data([requestID, surfaceID, String(surfaceVersion)].map { "\($0.utf8.count):\($0)" }.joined().utf8)
        return "ui-rendered-" + SHA256.hash(data: identity).map { String(format: "%02x", $0) }.joined()
    }
}

/// Tracks server ownership independently of whether the user dismissed a native container.
public struct UISurfaceRequests {
    public private(set) var requestIDs: [String: String] = [:]
    public private(set) var versions: [String: Int64] = [:]
    public private(set) var outcomes: [UISurfaceBinding: Bool] = [:]
    public init() {}
    /// Reserve before rendering or suspending. A failed render may retry its exact
    /// revision; successful equal revisions never reopen dismissed containers.
    @discardableResult public mutating func apply(_ event: UISurfaceEvent) -> Bool {
        let previous = versions[event.surfaceID] ?? 0
        guard event.surfaceVersion > previous ||
                (event.surfaceVersion == previous && outcomes[event.binding] == false) else { return false }
        versions[event.surfaceID] = event.surfaceVersion
        requestIDs[event.surfaceID] = event.requestID
        return true
    }
    public mutating func recordRender(_ event: UISurfaceEvent, success: Bool) { outcomes[event.binding] = success }
    public mutating func reconcile(_ snapshot: UISnapshot) -> [String] {
        let current = Set(snapshot.surfaces.map { $0.event.surfaceID })
        let removed = requestIDs.keys.filter { !current.contains($0) }
        for id in removed { requestIDs.removeValue(forKey: id) }
        return removed
    }
    public mutating func removeAll() { requestIDs.removeAll(); versions.removeAll(); outcomes.removeAll() }
}

public struct UISurfaceBinding: Hashable, Sendable {
    public let requestID: String
    public let surfaceID: String
    public let surfaceVersion: Int64
    public init(requestID: String, surfaceID: String, surfaceVersion: Int64) {
        self.requestID = requestID; self.surfaceID = surfaceID; self.surfaceVersion = surfaceVersion
    }
}

public func uiActionPayload(binding: UISurfaceBinding, action: ResolvedAction) throws -> JSONValue {
    let context = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(action.context))
    return .object([
        "request_id": .string(binding.requestID), "surface_id": .string(binding.surfaceID),
        "surface_version": .integer(binding.surfaceVersion),
        "name": .string(action.name), "source_component_id": .string(action.sourceComponentId),
        "context": context
    ])
}

/// The cursor and all surfaces are decoded/validated before any native state is changed.
public struct UISnapshot: Decodable, Sendable {
    public let cursor: Int64
    public let surfaces: [Surface]
    public struct Surface: Sendable {
        public let event: UISurfaceEvent
        public let renderStatus: String
    }
    private enum CodingKeys: String, CodingKey { case cursor, surfaces }
    public init(from decoder: Decoder) throws {
        let fields = try decoder.container(keyedBy: CodingKeys.self)
        cursor = try fields.decode(Int64.self, forKey: .cursor)
        guard cursor >= 0 else { throw ZoeError.invalidResponse }
        let values = try fields.decode([JSONValue].self, forKey: .surfaces)
        var ids = Set<String>()
        surfaces = try values.map { value in
            guard case let .object(object) = value,
                  case let .string(status) = object["render_status"],
                  ["generated", "rendered", "input_received", "failed"].contains(status),
                  let event = UISurfaceEvent.decode(value), ids.insert(event.surfaceID).inserted else { throw ZoeError.invalidResponse }
            return Surface(event: event, renderStatus: status)
        }
    }
}
