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
        guard let data = try? JSONEncoder().encode(payload),
              let event = try? JSONDecoder().decode(Self.self, from: data),
              !event.requestID.isEmpty, !event.surfaceID.isEmpty, !event.messages.isEmpty else { return nil }
        if !event.isDeletion {
            guard case .createSurface = event.messages[0] else { return nil }
            for message in event.messages.dropFirst() {
                switch message {
                case .createSurface, .deleteSurface: return nil
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
            guard id == event.surfaceID else { return nil }
        }
        return event
    }

    public var renderedPayload: JSONValue { .object(["request_id": .string(requestID)]) }

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
