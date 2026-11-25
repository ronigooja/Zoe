@MainActor
public final class AppTerminationGate {
    public enum Decision { case terminateNow, terminateLater }

    public nonisolated static let shutdownTimeout: Duration = .seconds(3)
    private let timeout: Duration
    private var hasDeferredTermination = false
    private var timer: Task<Void, Never>?

    public init(timeout: Duration = AppTerminationGate.shutdownTimeout) {
        self.timeout = timeout
    }

    public func request(
        active: Bool,
        shutdown: @escaping @MainActor () async -> Void,
        reply: @escaping @MainActor () -> Void
    ) -> Decision {
        if hasDeferredTermination { return .terminateLater }
        guard active else { return .terminateNow }
        hasDeferredTermination = true

        // A task group would still join a non-cooperative shutdown on timeout.
        let clock = ContinuousClock()
        let deadline = clock.now.advanced(by: timeout)
        timer = Task { [weak self] in
            do { try await clock.sleep(until: deadline) } catch { return }
            self?.finish(reply: reply)
        }
        Task { [weak self] in
            await shutdown()
            self?.finish(reply: reply)
        }
        return .terminateLater
    }

    private func finish(reply: @MainActor () -> Void) {
        guard let timer else { return }
        self.timer = nil
        timer.cancel()
        reply()
    }
}
