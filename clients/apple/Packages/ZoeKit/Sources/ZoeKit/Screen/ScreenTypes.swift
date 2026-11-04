import Foundation

public enum ScreenEmphasis: String, Codable, Sendable { case low, normal, high }
public enum ScreenTone: String, Codable, Sendable { case neutral, info, attention, success, critical }

public struct ScreenAction: Codable, Equatable, Sendable {
    public var intent: String
    public var args: [String: JSONValue]
    public init(intent: String, args: [String: JSONValue] = [:]) { self.intent = intent; self.args = args }
}

public struct ScreenNode: Codable, Equatable, Sendable, Identifiable {
    public var id: String
    public var type: String
    public var props: [String: JSONValue]
    public var children: [ScreenNode]
    public var emphasis: ScreenEmphasis
    public var tone: ScreenTone
    public var ttlSeconds: Double?
    public var transition: String?
    public var action: ScreenAction?
    public var bind: String?
    public init(id: String, type: String, props: [String: JSONValue] = [:], children: [ScreenNode] = [],
                emphasis: ScreenEmphasis = .normal, tone: ScreenTone = .neutral, ttlSeconds: Double? = nil,
                transition: String? = nil, action: ScreenAction? = nil, bind: String? = nil) {
        self.id = id; self.type = type; self.props = props; self.children = children
        self.emphasis = emphasis; self.tone = tone; self.ttlSeconds = ttlSeconds
        self.transition = transition; self.action = action; self.bind = bind
    }
}

public struct ScreenDocument: Codable, Equatable, Sendable {
    public var version: Int
    public var revision: Int64
    public var root: ScreenNode
    public var data: [String: JSONValue]
    public init(version: Int, revision: Int64, root: ScreenNode, data: [String: JSONValue] = [:]) {
        self.version = version; self.revision = revision; self.root = root; self.data = data
    }
}

public enum SurfaceKind: String, Codable, Sendable { case window, panel, statusItem, popover, dockBadge }

public struct SurfaceWindowSpec: Codable, Equatable, Sendable {
    public var visible: Bool?
    public var width: Double?
    public var height: Double?
    public var x: Double?
    public var y: Double?
    public var display: Int?
    public var floating: Bool?
    public var opacity: Double?
    public var clickThrough: Bool?
    public var takesFocus: Bool?
    public var fullscreen: Bool?
    public var showInDock: Bool?
    public var showInMenuBar: Bool?
    public var material: String?
    public init() {}
}

public struct SurfaceSpec: Codable, Equatable, Sendable, Identifiable {
    public var id: String
    public var kind: SurfaceKind
    public var document: ScreenDocument
    public var window: SurfaceWindowSpec
    public init(id: String, kind: SurfaceKind, document: ScreenDocument, window: SurfaceWindowSpec = SurfaceWindowSpec()) {
        self.id = id; self.kind = kind; self.document = document; self.window = window
    }
}

/// Implemented by the macOS AppKit layer; faked in ZoeKit tests.
@MainActor public protocol SurfaceHost: AnyObject {
    func open(_ spec: SurfaceSpec)
    func update(id: String, document: ScreenDocument?, window: SurfaceWindowSpec?)
    func close(id: String)
    func openSurfaceIDs() -> [String]
}

public struct SettingDescriptor: Codable, Equatable, Sendable {
    public var key: String
    public var type: String
    public var range: String?
    public var summary: String
    public var aiMutable: Bool
    public var userOnlyReason: String?
    public init(key: String, type: String, range: String? = nil, summary: String, aiMutable: Bool = true, userOnlyReason: String? = nil) {
        self.key = key; self.type = type; self.range = range; self.summary = summary
        self.aiMutable = aiMutable; self.userOnlyReason = userOnlyReason
    }
}

public struct SettingAuditRecord: Codable, Equatable, Sendable, Identifiable {
    public var id: String
    public var key: String
    public var oldValue: JSONValue
    public var newValue: JSONValue
    public var at: Date
    public var undone: Bool
    public init(id: String, key: String, oldValue: JSONValue, newValue: JSONValue, at: Date, undone: Bool = false) {
        self.id = id; self.key = key; self.oldValue = oldValue; self.newValue = newValue; self.at = at; self.undone = undone
    }
}

public enum SettingWriteError: Error, Equatable, Sendable { case unknownKey, userOnly, invalidValue }

public struct ComponentDescriptor: Codable, Equatable, Sendable {
    public var type: String
    public var purpose: String
    public var propsSchema: [String: JSONValue]
    public var example: ScreenNode
    public var fallback: String
    public var libraryVersion: Int
    public init(type: String, purpose: String, propsSchema: [String: JSONValue] = [:], example: ScreenNode? = nil,
                fallback: String, libraryVersion: Int = 1) {
        self.type = type; self.purpose = purpose; self.propsSchema = propsSchema
        self.example = example ?? ScreenNode(id: "example", type: type)
        self.fallback = fallback; self.libraryVersion = libraryVersion
    }
}

public extension JSONValue {
    /// Numeric value for either integer or floating-point JSON numbers.
    var screenNumber: Double? {
        switch self {
        case let .number(value): value
        case let .integer(value): Double(value)
        default: nil
        }
    }
}
