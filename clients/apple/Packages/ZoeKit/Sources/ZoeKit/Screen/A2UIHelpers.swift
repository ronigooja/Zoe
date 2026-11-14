import A2UISwiftCore

/// A create replaces renderer state; leading deletes before any create need no renderer.
public func surfaceMessageHistory(_ history: [A2uiMessage], applying messages: [A2uiMessage]) -> [A2uiMessage] {
    var result = history
    for message in messages {
        if case .createSurface = message { result.removeAll() }
        if result.isEmpty, case .deleteSurface = message { continue }
        result.append(message)
    }
    return result
}
