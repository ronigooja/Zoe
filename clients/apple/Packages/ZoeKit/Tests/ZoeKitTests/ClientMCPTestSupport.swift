import Foundation
import XCTest
@testable import ZoeKit

actor ManualMCPClock: MCPTimerClock {
    private struct Sleep {
        let delay: TimeInterval
        let deadline: TimeInterval
        let continuation: CheckedContinuation<Void, Error>
    }
    private var pending: [UUID: Sleep] = [:]
    private(set) var now: TimeInterval = 0
    private(set) var requested: [TimeInterval] = []
    var sleepers: [TimeInterval] { pending.values.map(\.delay).sorted() }
    private var sleepWaiters: [(TimeInterval, CheckedContinuation<Void, Never>)] = []

    /// Suspends until a sleep with exactly `seconds` is pending. Unlike polling `sleepers`, this is resumed by the sleep itself.
    func awaitSleeper(seconds: TimeInterval) async {
        if pending.values.contains(where: { $0.delay == seconds }) { return }
        await withCheckedContinuation { sleepWaiters.append((seconds, $0)) }
    }

    func sleep(seconds: TimeInterval) async throws {
        let id = UUID()
        try await withTaskCancellationHandler {
            try Task.checkCancellation()
            try await withCheckedThrowingContinuation { continuation in
                requested.append(seconds)
                pending[id] = Sleep(delay: seconds, deadline: now + seconds, continuation: continuation)
                let ready = sleepWaiters.filter { $0.0 == seconds }
                sleepWaiters.removeAll { $0.0 == seconds }
                for (_, waiter) in ready { waiter.resume() }
            }
        } onCancel: {
            Task { await self.cancel(id) }
        }
    }

    func advance(_ seconds: TimeInterval) {
        now += seconds
        for (id, sleep) in pending where sleep.deadline <= now {
            pending.removeValue(forKey: id)
            sleep.continuation.resume()
        }
    }

    private func cancel(_ id: UUID) {
        pending.removeValue(forKey: id)?.continuation.resume(throwing: CancellationError())
    }
}

private struct FixtureTimeout: Error {}

func mcpEventually(file: StaticString = #filePath, line: UInt = #line, _ predicate: () async -> Bool) async throws {
    // This is only a scheduling watchdog; behavior is driven by fixture events.
    let deadline = ContinuousClock.now.advanced(by: .seconds(3))
    while ContinuousClock.now < deadline {
        if await predicate() { return }
        try await Task.sleep(for: .milliseconds(1))
    }
    XCTFail("Fixture did not reach expected state", file: file, line: line)
    throw FixtureTimeout()
}

extension JSONValue {
    subscript(mcp key: String) -> JSONValue? {
        guard case let .object(object) = self else { return nil }
        return object[key]
    }
}
