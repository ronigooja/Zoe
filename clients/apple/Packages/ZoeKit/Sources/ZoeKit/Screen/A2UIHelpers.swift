import A2UISwiftCore

/// A create replaces renderer state; updates accumulate until the next create.
public func surfaceMessageHistory(_ history: [A2uiMessage], applying messages: [A2uiMessage]) -> [A2uiMessage] {
    var result = history
    for message in messages {
        if case .createSurface = message { result.removeAll() }
        result.append(message)
    }
    return result
}
