#if os(macOS)
import A2UISwiftCore
import Foundation
import Observation

/// Data-model paths bound by an action's context, sorted by context key. Only `dataBinding` values count.
public func uiBoundPaths(in context: [String: DynamicValue]?) -> [String] {
    guard let context else { return [] }
    return context.keys.sorted().compactMap { key in
        if case let .dataBinding(path) = context[key] { return path }
        return nil
    }
}

/// Pending and failure state for bound input controls on one surface, keyed by component id.
@MainActor @Observable public final class SurfaceActionTracker {
    public private(set) var pending = Set<String>()
    public private(set) var failed = Set<String>()
    @ObservationIgnored private var bound: [String: (context: DataContext, paths: [String])] = [:]

    public init() {}

    /// Called on tap, before the library dispatches the action. The paths belong to this action only.
    public func begin(component: String, context: DataContext, paths: [String]) {
        guard !pending.contains(component) else { return }
        pending.insert(component)
        failed.remove(component)
        bound[component] = (context, paths)
    }

    /// Restores the control. Success clears only the paths bound by this action. Failure keeps every value.
    public func finish(component: String, success: Bool) {
        guard pending.remove(component) != nil else { return }
        let captured = bound.removeValue(forKey: component)
        if success {
            for path in captured?.paths ?? [] {
                try? captured?.context.set(path, value: nil)
            }
        } else {
            failed.insert(component)
        }
    }

    public func isPending(_ component: String) -> Bool { pending.contains(component) }
    public func didFail(_ component: String) -> Bool { failed.contains(component) }
}
#endif
