import Foundation
import XCTest
@testable import ZoeKit

private struct TestCredential: BearerTokenProviding {
    func bearerToken() throws -> String { "fixture" }
}

func mcpTestAPI() throws -> APIClient {
    try APIClient(serverURL: URL(string: "https://example.invalid/base")!, credentials: TestCredential())
}

actor FixtureMCPSocket: ClientWebSocket {
    let openError: ClientWebSocketError?
    var opened = false
    var closeCount = 0
    var oversizeCloses = 0
    var pingCount = 0
    var sent: [Data] = []
    private var inbound: [Data] = []
    private var reader: CheckedContinuation<Data, Error>?
    private var pinger: CheckedContinuation<Void, Error>?

    /// Events a test can await deterministically instead of polling the counters above.
    enum Event: Equatable, Sendable { case opened, sent(Int), closed }

    /// A pending wait. It is resolved exactly once: by a matching event, or by its own timeout, whichever comes first.
    private struct Waiter {
        let event: Event
        let continuation: CheckedContinuation<Bool, Never>
    }
    private var waiters: [UUID: Waiter] = [:]

    init(openError: ClientWebSocketError? = nil) { self.openError = openError }

    /// Suspends until `event` has occurred (true) or `timeout` elapses (false). A reached event returns immediately.
    /// The timeout is part of the wait, so a timed-out waiter is removed and never leaks.
    func waitFor(_ event: Event, timeout: Duration) async -> Bool {
        if matches(event) { return true }
        let id = UUID()
        let timer = Task {
            try? await Task.sleep(for: timeout)
            self.resolve(id, with: false)
        }
        let result = await withCheckedContinuation { (continuation: CheckedContinuation<Bool, Never>) in
            waiters[id] = Waiter(event: event, continuation: continuation)
        }
        timer.cancel()
        return result
    }

    private func matches(_ event: Event) -> Bool {
        switch event {
        case .opened: opened
        case let .sent(count): sent.count >= count
        case .closed: closeCount > 0
        }
    }

    /// Resumes one waiter at most once. A waiter that already resolved is absent, so a late timer is a no-op.
    private func resolve(_ id: UUID, with value: Bool) {
        guard let waiter = waiters.removeValue(forKey: id) else { return }
        waiter.continuation.resume(returning: value)
    }

    private func record(_ event: Event) {
        let snapshot = waiters
        for (id, waiter) in snapshot where waiter.event == event || matches(waiter.event) {
            resolve(id, with: true)
        }
    }

    func open() async throws {
        if let openError { throw openError }
        opened = true
        record(.opened)
    }
    func receive() async throws -> Data {
        guard closeCount == 0 else { throw ClientWebSocketError.disconnected }
        if !inbound.isEmpty { return inbound.removeFirst() }
        return try await withCheckedThrowingContinuation { reader = $0 }
    }
    func push(_ data: Data) {
        if let reader { self.reader = nil; reader.resume(returning: data) }
        else { inbound.append(data) }
    }
    func send(_ data: Data) async throws {
        sent.append(data)
        record(.sent(sent.count))
    }
    func ping() async throws {
        guard closeCount == 0 else { throw ClientWebSocketError.disconnected }
        pingCount += 1
        try await withCheckedThrowingContinuation { pinger = $0 }
    }
    func pong() { pinger?.resume(); pinger = nil }
    func close() async { disconnect() }
    func closeTooLarge() async { oversizeCloses += 1; disconnect() }
    func disconnect() {
        closeCount += 1
        reader?.resume(throwing: ClientWebSocketError.disconnected); reader = nil
        pinger?.resume(throwing: ClientWebSocketError.disconnected); pinger = nil
        record(.closed)
    }
}

/// Waits for a named fixture event with a bounded timeout. Fails the test with the event name instead of hanging.
func awaitFixture(_ socket: FixtureMCPSocket, _ event: FixtureMCPSocket.Event, timeout: Duration = .seconds(5), file: StaticString = #filePath, line: UInt = #line) async throws {
    guard await socket.waitFor(event, timeout: timeout) else {
        XCTFail("Fixture event \(event) did not occur within \(timeout)", file: file, line: line)
        throw FixtureTimeout()
    }
}

private struct FixtureTimeout: Error {}

final class SocketPlan: @unchecked Sendable {
    private let lock = NSLock()
    private let sockets: [FixtureMCPSocket]
    private var requests: [URLRequest] = []

    init(_ sockets: [FixtureMCPSocket]) { self.sockets = sockets }
    func make(_ request: URLRequest) -> any ClientWebSocket {
        lock.lock(); defer { lock.unlock() }
        let index = requests.count
        requests.append(request)
        return index < sockets.count ? sockets[index] : FixtureMCPSocket(openError: .unauthorized)
    }
    var count: Int { lock.lock(); defer { lock.unlock() }; return requests.count }
}

private actor SocketEvents {
    var values: [String] = []
    func append(_ value: String) { values.append(value) }
}

final class ClientWebSocketTransportTests: XCTestCase {
    func testAuthenticatedUpgradeRequestUsesWebSocketScheme() throws {
        let request = try mcpTestAPI().clientWebSocketRequest()
        XCTAssertEqual(request.url?.scheme, "wss")
        XCTAssertTrue(request.url?.path.hasSuffix("/v1/ws") == true)
        XCTAssertEqual(request.httpMethod, "GET")
        XCTAssertNotNil(request.value(forHTTPHeaderField: "Authorization"))
    }

    func testUnauthorizedStopsUntilCredentialsChangeAndRejectsOversize() async throws {
        let one = FixtureMCPSocket(openError: .unauthorized), two = FixtureMCPSocket(openError: .unauthorized)
        let plan = SocketPlan([one, two]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) }, onOpen: {}, onClose: {}, onMCPFrame: { _ in })
        await transport.setNeeded(true)
        try await mcpEventually { await one.closeCount == 1 }
        await clock.advance(100)
        XCTAssertEqual(plan.count, 1)
        do {
            try await transport.send(Data(repeating: 0, count: ClientWebSocketLimits.frameBytes + 1))
            XCTFail("expected oversized frame rejection")
        } catch let error as ClientWebSocketError { XCTAssertEqual(error, .frameTooLarge) }
        await transport.credentialsChanged()
        try await mcpEventually { await two.closeCount == 1 }
        XCTAssertEqual(plan.count, 2)
        await transport.setNeeded(false)
    }

    func testBackoffGrowsCapsAndResetsOnSuccessfulOpen() async throws {
        let failures = [1.0, 2, 4, 8, 16, 30, 30]
        let good = FixtureMCPSocket(), final = FixtureMCPSocket()
        let plan = SocketPlan(failures.map { _ in FixtureMCPSocket(openError: .disconnected) } + [good, final])
        let clock = ManualMCPClock(), events = SocketEvents()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) }, onOpen: { await events.append("open") }, onClose: { await events.append("close") }, onMCPFrame: { _ in })
        await transport.setNeeded(true)
        for (index, delay) in failures.enumerated() {
            try await mcpEventually { await clock.sleepers == [delay] }
            XCTAssertEqual(plan.count, index + 1)
            await clock.advance(delay)
        }
        try await mcpEventually { await events.values == ["open"] }
        await good.disconnect()
        try await mcpEventually { await clock.sleepers == [1] }
        await clock.advance(1)
        try await mcpEventually { await events.values == ["open", "close", "open"] }
        await transport.setNeeded(false)
        try await mcpEventually { await events.values == ["open", "close", "open", "close"] }
    }

    func testConnectionNeededClosesAndNeverReconnectsWhileOff() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), events = SocketEvents(), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) }, onOpen: { await events.append("open") }, onClose: { await events.append("close") }, onMCPFrame: { _ in })
        await transport.setNeeded(false)
        await transport.credentialsChanged()
        XCTAssertEqual(plan.count, 0)
        await transport.setNeeded(true)
        try await mcpEventually { await events.values == ["open"] }
        await transport.setNeeded(false)
        try await mcpEventually { await events.values == ["open", "close"] }
        try await mcpEventually { await clock.sleepers.isEmpty }
        await clock.advance(100)
        XCTAssertEqual(plan.count, 1)
        let closes = await socket.closeCount
        XCTAssertGreaterThanOrEqual(closes, 1)
    }

    func testPingPongAndInboundFrameExtendDeadlineThenSilenceCloses() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), events = SocketEvents(), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) }, onOpen: { await events.append("open") }, onClose: { await events.append("close") }, onMCPFrame: { _ in await events.append("frame") })
        await transport.setNeeded(true)
        try await mcpEventually { await clock.sleepers == [15] }
        await clock.advance(15)
        try await mcpEventually { await socket.pingCount == 1 }
        await socket.pong()
        // The pong alone must keep the socket open at the original deadline.
        try await mcpEventually { await transport.lastActivity == 15 }
        await clock.advance(15)
        try await mcpEventually { await socket.pingCount == 2 }
        try await mcpEventually { await clock.sleepers == [15] }
        await clock.advance(10)
        await socket.push(Data(#"{"channel":"mcp"}"#.utf8))
        try await mcpEventually { await events.values == ["open", "frame"] }
        await clock.advance(5)
        // A ping remains outstanding, so no second ping is queued behind it.
        try await mcpEventually { await clock.sleepers == [15] }
        let pings = await socket.pingCount
        XCTAssertEqual(pings, 2)
        await clock.advance(15)
        try await mcpEventually { await clock.sleepers == [10] }
        await clock.advance(10)
        try await mcpEventually { await events.values == ["open", "frame", "close"] }
        await transport.setNeeded(false)
    }

    func testUnknownChannelErrorIsDiagnosedWithoutReplyAndUnsupportedChannelGetsReply() async throws {
        let socket = FixtureMCPSocket(), events = SocketEvents()
        let diagnostic = expectation(description: "diagnosed inbound transport error")
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), factory: { _ in socket }, onOpen: {}, onClose: {}, onMCPFrame: { _ in await events.append("frame") }, diagnostic: { _ in diagnostic.fulfill() })
        await transport.setNeeded(true)
        await socket.push(Data(#"{"channel":"transport","type":"error","error":{"code":"unknown_channel","channel":"fixture"}}"#.utf8))
        await fulfillment(of: [diagnostic], timeout: 1)
        await socket.push(Data(#"{"channel":"other"}"#.utf8))
        await socket.push(Data(#"{"channel":"mcp"}"#.utf8))
        try await mcpEventually { await events.values == ["frame"] }
        let sent = await socket.sent
        XCTAssertEqual(sent.count, 1)
        let response = try JSONDecoder().decode(JSONValue.self, from: XCTUnwrap(sent.first))
        XCTAssertEqual(response[mcp: "error"]?[mcp: "code"], .string("unknown_channel"))
        XCTAssertEqual(response[mcp: "error"]?[mcp: "channel"], .string("other"))
        await transport.setNeeded(false)
    }

    func testOversizedInboundFrameRequestsClose1009() async throws {
        let socket = FixtureMCPSocket(), events = SocketEvents()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), factory: { _ in socket }, onOpen: {}, onClose: { await events.append("close") }, onMCPFrame: { _ in XCTFail("oversized frame routed") })
        await transport.setNeeded(true)
        await socket.push(Data(repeating: 0, count: ClientWebSocketLimits.frameBytes + 1))
        try await mcpEventually { await events.values == ["close"] }
        let closes = await socket.oversizeCloses
        XCTAssertEqual(closes, 1)
        await transport.setNeeded(false)
    }
}
