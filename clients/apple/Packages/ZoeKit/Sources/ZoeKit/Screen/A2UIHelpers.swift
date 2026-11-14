import A2UISwiftCore

public extension ScreenAction {
    init(_ action: ResolvedAction) {
        self.init(intent: action.name, args: action.context.mapValues(JSONValue.init))
    }
}

private extension JSONValue {
    init(_ value: AnyCodable) {
        switch value {
        case let .string(value): self = .string(value)
        case let .number(value):
            if let integer = Int64(exactly: value) { self = .integer(integer) }
            else { self = .number(value) }
        case let .bool(value): self = .bool(value)
        case .null: self = .null
        case let .array(value): self = .array(value.map(JSONValue.init))
        case let .dictionary(value): self = .object(value.mapValues(JSONValue.init))
        }
    }
}

/// Native labels use the first Text component in message order, after applying updates and deletes.
/// A dynamic (nonliteral) first Text has no native label; this helper does not evaluate bindings.
public func firstTextLabel(in messages: [A2uiMessage]) -> String? {
    var order: [String] = []
    var components: [String: RawComponent] = [:]
    for message in messages {
        switch message {
        case .createSurface, .deleteSurface:
            order.removeAll()
            components.removeAll()
        case let .updateComponents(payload):
            for component in payload.components {
                if components[component.id] == nil { order.append(component.id) }
                var merged = component
                if let previous = components[component.id] {
                    if merged.component.isEmpty { merged.component = previous.component }
                    merged.properties = previous.properties.merging(component.properties) { _, new in new }
                }
                components[component.id] = merged
            }
        case .updateDataModel: break
        }
    }
    guard let id = order.first(where: { components[$0]?.component == "Text" }) else { return nil }
    return components[id]?.properties["text"]?.stringValue
}
