import Foundation
import A2UISwiftCore

@MainActor public final class ScreenToolRouter {
    private let host: SurfaceHost
    private let settings: SettingsRegistry
    private let startVoice: () -> Void
    private let endVoice: () -> Void
    private let setSilenceTimeout: (Double) -> Void
    private let showTextInput: () -> Void
    private let hideTextInput: () -> Void

    public init(host: SurfaceHost, settings: SettingsRegistry,
                startVoice: @escaping () -> Void, endVoice: @escaping () -> Void,
                setSilenceTimeout: @escaping (Double) -> Void,
                showTextInput: @escaping () -> Void, hideTextInput: @escaping () -> Void) {
        self.host = host
        self.settings = settings
        self.startVoice = startVoice
        self.endVoice = endVoice
        self.setSilenceTimeout = setSilenceTimeout
        self.showTextInput = showTextInput
        self.hideTextInput = hideTextInput
    }

    public func handle(tool: String, args: [String: JSONValue]) async -> JSONValue {
        switch tool {
        case "surface.open":
            let fields = objectMap(args["spec"]) ?? args
            guard let decoded = messages(fields["messages"]) else { return error("invalid surface messages") }
            guard let spec = surfaceSpec(fields, messages: decoded) else { return error("invalid surface spec") }
            host.open(spec)
            return .object(["ok": .bool(true)])
        case "surface.update":
            guard let id = string(args["id"]) else { return error("missing surface id") }
            let patch = objectMap(args["patch"]) ?? args
            guard patch["messages"] != nil || patch["window"] != nil else { return error("missing surface patch") }
            let decoded = messages(patch["messages"])
            if patch["messages"] != nil && decoded == nil { return error("invalid surface messages") }
            if patch["window"] != nil && window(patch["window"]) == nil { return error("invalid surface window") }
            host.update(id: id, messages: decoded, window: window(patch["window"]))
            return .object(["ok": .bool(true)])
        case "surface.close":
            guard let id = string(args["id"]) else { return error("missing surface id") }
            host.close(id: id)
            return .object(["ok": .bool(true)])
        case "surface.list":
            return .object(["surfaces": .array(host.openSurfaceIDs().map { .string($0) })])
        case "settings.list":
            return .object(["settings": .array(settings.descriptors().map(settingJSON))])
        case "settings.read":
            guard let key = string(args["key"]) else { return error("missing setting key") }
            guard let value = settings.value(for: key) else { return error("unknown setting key") }
            return .object(["key": .string(key), "value": value])
        case "settings.write":
            guard let key = string(args["key"]), let value = args["value"] else { return error("missing setting key or value") }
            do {
                let record = try settings.write(key: key, value: value, by: .ai)
                return .object(["ok": .bool(true), "recordID": .string(record?.id ?? "")])
            } catch let failure { return self.error(settingError(failure)) }
        case "settings.undo":
            guard let id = string(args["recordID"] ?? args["recordId"]) else { return error("missing record id") }
            return settings.undo(recordID: id) ? .object(["ok": .bool(true)]) : error("unknown audit record")
        case "voice.start":
            startVoice(); return .object(["ok": .bool(true)])
        case "voice.end":
            endVoice(); return .object(["ok": .bool(true)])
        case "voice.setSilenceTimeout":
            guard let seconds = number(args["seconds"]), seconds.isFinite, seconds >= 0 else { return error("invalid timeout seconds") }
            setSilenceTimeout(seconds); return .object(["ok": .bool(true)])
        case "textInput.show":
            showTextInput(); return .object(["ok": .bool(true)])
        case "textInput.hide":
            hideTextInput(); return .object(["ok": .bool(true)])
        default: return error("unknown tool")
        }
    }

    private func surfaceSpec(_ args: [String: JSONValue], messages: [A2uiMessage]) -> SurfaceSpec? {
        guard let id = string(args["id"]), let kindName = string(args["kind"]),
              let kind = SurfaceKind(rawValue: kindName) else { return nil }
        guard args["window"] == nil || window(args["window"]) != nil else { return nil }
        return SurfaceSpec(id: id, kind: kind, messages: messages, window: window(args["window"]) ?? SurfaceWindowSpec())
    }

    private func messages(_ value: JSONValue?) -> [A2uiMessage]? {
        guard case .array = value, let value, let data = try? JSONEncoder().encode(value) else { return nil }
        return try? JSONDecoder().decode([A2uiMessage].self, from: data)
    }

    private func window(_ value: JSONValue?) -> SurfaceWindowSpec? {
        guard let object = objectMap(value) else { return nil }
        var result = SurfaceWindowSpec()
        result.visible = bool(object["visible"]); result.width = number(object["width"]); result.height = number(object["height"])
        result.x = number(object["x"]); result.y = number(object["y"]); result.display = integer(object["display"])
        result.floating = bool(object["floating"]); result.opacity = number(object["opacity"])
        result.clickThrough = bool(object["clickThrough"]); result.takesFocus = bool(object["takesFocus"])
        result.fullscreen = bool(object["fullscreen"]); result.showInDock = bool(object["showInDock"])
        result.showInMenuBar = bool(object["showInMenuBar"])
        result.material = string(object["material"])
        return result
    }

    private func settingJSON(_ descriptor: SettingDescriptor) -> JSONValue {
        var value: [String: JSONValue] = ["key": .string(descriptor.key), "type": .string(descriptor.type), "summary": .string(descriptor.summary), "aiMutable": .bool(descriptor.aiMutable)]
        if let range = descriptor.range { value["range"] = .string(range) }
        if let reason = descriptor.userOnlyReason { value["userOnlyReason"] = .string(reason) }
        return .object(value)
    }

    private func error(_ message: String) -> JSONValue { .object(["error": .string(message)]) }
    private func settingError(_ error: Error) -> String {
        guard let error = error as? SettingWriteError else { return "setting write failed" }
        switch error {
        case .unknownKey: return "unknown setting key"
        case .userOnly: return "setting is user-only"
        case .invalidValue: return "invalid setting value"
        }
    }
    private func string(_ value: JSONValue?) -> String? { if case let .string(value) = value { value } else { nil } }
    private func number(_ value: JSONValue?) -> Double? { value?.screenNumber }
    private func bool(_ value: JSONValue?) -> Bool? { if case let .bool(value) = value { value } else { nil } }
    private func integer(_ value: JSONValue?) -> Int? { number(value).map(Int.init) }
    private func objectMap(_ value: JSONValue?) -> [String: JSONValue]? { if case let .object(value) = value { value } else { nil } }
}
