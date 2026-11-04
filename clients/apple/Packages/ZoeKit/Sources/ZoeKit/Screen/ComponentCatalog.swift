import Foundation

public enum ComponentCatalog {
    public static let version: Int = 1

    private static func descriptor(_ type: String, _ purpose: String, _ fallback: String) -> ComponentDescriptor {
        let required: [String: JSONValue]
        switch type {
        case "stack": required = ["axis": .object(["type": .string("string"), "required": .bool(true)])]
        case "grid": required = ["columns": .object(["type": .string("number"), "required": .bool(true)])]
        case "title", "text": required = ["text": .object(["type": .string("string"), "required": .bool(true)])]
        case "decision": required = ["question": .object(["type": .string("string"), "required": .bool(true)]), "options": .object(["type": .string("array"), "required": .bool(true)])]
        case "collection": required = ["items": .object(["type": .string("array"), "required": .bool(true)])]
        default: required = [:]
        }
        let exampleProps = required.reduce(into: [String: JSONValue]()) { result, entry in
            switch entry.key { case "axis", "text", "question": result[entry.key] = .string("example"); case "columns": result[entry.key] = .number(1); default: result[entry.key] = .array([]) }
        }
        return ComponentDescriptor(type: type, purpose: purpose, propsSchema: required,
                                   example: ScreenNode(id: "example", type: type, props: exampleProps), fallback: fallback)
    }

    public static let descriptors: [ComponentDescriptor] = [
        descriptor("stack", "Ordered layout of children", "stack"),
        descriptor("grid", "Grid layout", "stack"),
        descriptor("split", "Split layout", "stack"), descriptor("group", "Labeled group of children", "stack"),
        descriptor("spacer", "Flexible or minimum space", "spacer"), descriptor("canvas", "Positioned children", "stack"),
        descriptor("layer", "Children stacked on top of one another", "stack"), descriptor("scroll", "Scrollable children", "stack"),
        descriptor("frame", "Size-constrained children", "stack"), descriptor("background", "Background around children", "stack"),
        descriptor("title", "A heading", "text"), descriptor("text", "Plain text", "text"), descriptor("richText", "Markdown text", "text"),
        descriptor("list", "A list of values", "text"), descriptor("table", "Rows and columns of values", "text"), descriptor("progress", "Progress toward a value", "text"),
        descriptor("statusDot", "A compact state indicator", "text"), descriptor("image", "An image resource", "text"), descriptor("icon", "A named icon", "text"),
        descriptor("link", "A labeled URL", "text"), descriptor("countdown", "Time remaining until a date", "text"), descriptor("divider", "A visual divider", "text"),
        descriptor("button", "An action control", "text"), descriptor("choice", "A choice among options", "text"), descriptor("input", "A text or typed input", "text"),
        descriptor("toggle", "A boolean choice", "text"), descriptor("metric", "A value with trend", "text"), descriptor("timeline", "Time-ordered events", "text"),
        descriptor("steps", "Staged progress", "text"), descriptor("decision", "A choice needing an answer", "text"), descriptor("compare", "Several objects by dimension", "text"),
        descriptor("collection", "Same-kind items", "stack"), descriptor("article", "Long text", "text"), descriptor("chart", "Numeric series", "text"),
        descriptor("place", "A location", "text"), descriptor("media", "Image, audio, or video", "text"), descriptor("notice", "A one-off alert", "text"),
        descriptor("facts", "Key-value detail", "text"), descriptor("status", "Live state", "text"), descriptor("form", "User-filled fields", "text"),
        descriptor("quote", "Cited or quoted text", "text"), descriptor("code", "Short literal text", "text"), descriptor("gauge", "A value within a range", "text"),
        descriptor("calendar", "Dates and events", "text"), descriptor("map", "Several places or a route", "text"), descriptor("gallery", "Many images", "text"),
        descriptor("ranking", "Ordered scored items", "text"), descriptor("checklist", "Items with done state", "text")
    ]

    private static let knownTypes = Set(descriptors.map(\.type))

    public static func resolve(_ node: ScreenNode) -> ScreenNode {
        let resolvedChildren = node.children.map(resolve)
        guard !knownTypes.contains(node.type) else {
            var result = node
            result.children = resolvedChildren
            return result
        }

        var fallbackChildren: [ScreenNode] = node.props.keys.sorted().map { key in
            ScreenNode(id: "\(node.id).prop.\(key)", type: "text",
                       props: ["text": .string("\(key): \(jsonText(node.props[key]!))")])
        }
        fallbackChildren += resolvedChildren.enumerated().flatMap { offset, child in
            readableLines(child).enumerated().map { lineOffset, line in
                ScreenNode(id: "\(node.id).child.\(offset).\(lineOffset)", type: "text",
                           props: ["text": .string(line)])
            }
        }
        var result = node
        result.type = "stack"
        result.props = [:]
        result.children = fallbackChildren
        return result
    }

    private static func jsonText(_ value: JSONValue) -> String {
        switch value {
        case .null: "null"
        case let .bool(value): String(value)
        case let .number(value): String(value)
        case let .integer(value): String(value)
        case let .string(value): value
        case let .array(values): "[\(values.map(jsonText).joined(separator: ", "))]"
        case let .object(values): "{\(values.keys.sorted().map { "\($0): \(jsonText(values[$0]!))" }.joined(separator: ", "))}"
        }
    }

    private static func jsonText(_ value: [String: JSONValue]) -> String {
        jsonText(.object(value))
    }

    private static func readableLines(_ node: ScreenNode) -> [String] {
        var lines = node.props.keys.sorted().map { "\($0): \(jsonText(node.props[$0]!))" }
        if lines.isEmpty { lines.append(node.type) }
        for child in node.children {
            lines += readableLines(child).map { "\(child.type): \($0)" }
        }
        return lines
    }
}
