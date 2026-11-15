import A2UISwiftCore

public enum SurfaceKind: String, Codable, Sendable {
    case popover, window
}

public struct SurfaceSize: Codable, Sendable, Equatable {
    public var width: Int
    public var height: Int

    public init(width: Int, height: Int) {
        self.width = width
        self.height = height
    }

    public var screenPayload: JSONValue {
        .object(["width": .integer(Int64(width)), "height": .integer(Int64(height))])
    }

    public var screenIdempotencyKey: String { "ui.screen-\(width)-\(height)" }
}

public struct SurfaceSpec: Codable, Sendable, Identifiable {
    public var id: String
    public var kind: SurfaceKind
    public var messages: [A2uiMessage]
    public var size: SurfaceSize?

    public init(id: String, kind: SurfaceKind, messages: [A2uiMessage], size: SurfaceSize? = nil) {
        self.id = id
        self.kind = kind
        self.messages = messages
        self.size = size
    }
}

@MainActor public protocol SurfaceHost: AnyObject {
    func open(_ spec: SurfaceSpec)
    func update(id: String, messages: [A2uiMessage])
    func close(id: String)
}

/// Message history and lifecycle shared by native containers. Updates to unknown IDs are ignored.
public struct SurfaceStore {
    public private(set) var surfaces: [String: SurfaceSpec] = [:]
    public init() {}

    public mutating func open(_ spec: SurfaceSpec) {
        var spec = spec
        spec.messages = surfaceMessageHistory([], applying: spec.messages)
        surfaces[spec.id] = spec
    }

    public mutating func update(id: String, messages: [A2uiMessage]) {
        guard var spec = surfaces[id] else { return }
        spec.messages = surfaceMessageHistory(spec.messages, applying: messages)
        surfaces[id] = spec
    }

    public mutating func close(id: String) { surfaces.removeValue(forKey: id) }
}
