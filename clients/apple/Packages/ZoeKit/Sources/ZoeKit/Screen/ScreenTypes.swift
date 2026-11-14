import Foundation
import A2UISwiftCore

public struct ScreenAction: Codable, Equatable, Sendable {
    public var intent: String
    public var args: [String: JSONValue]
    public init(intent: String, args: [String: JSONValue] = [:]) { self.intent = intent; self.args = args }
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

public struct SurfaceSpec: Codable, Sendable, Identifiable {
    public var id: String
    public var kind: SurfaceKind
    public var messages: [A2uiMessage]
    public var window: SurfaceWindowSpec
    public init(id: String, kind: SurfaceKind, messages: [A2uiMessage], window: SurfaceWindowSpec = SurfaceWindowSpec()) {
        self.id = id; self.kind = kind; self.messages = messages; self.window = window
    }
}

/// Implemented by the macOS AppKit layer; faked in ZoeKit tests.
@MainActor public protocol SurfaceHost: AnyObject {
    func open(_ spec: SurfaceSpec)
    func update(id: String, messages: [A2uiMessage]?, window: SurfaceWindowSpec?)
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
