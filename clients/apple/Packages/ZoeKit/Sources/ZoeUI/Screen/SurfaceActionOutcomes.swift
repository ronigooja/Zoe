import Foundation

/// Routes the outcome of a bound surface action back to the native control that sent it.
/// Every action path reports exactly once, on success or failure, so a pending control always restores.
@MainActor public final class SurfaceActionOutcomes {
    public static let shared = SurfaceActionOutcomes()

    private var handlers: [String: @MainActor (String, Bool) -> Void] = [:]

    private init() {}

    public func register(surfaceID: String, handler: @escaping @MainActor (String, Bool) -> Void) {
        handlers[surfaceID] = handler
    }

    public func unregister(surfaceID: String) {
        handlers.removeValue(forKey: surfaceID)
    }

    public func report(surfaceID: String, component: String, success: Bool) {
        handlers[surfaceID]?(component, success)
    }
}
