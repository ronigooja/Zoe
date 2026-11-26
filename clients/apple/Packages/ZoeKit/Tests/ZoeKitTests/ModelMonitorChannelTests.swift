import Foundation
import XCTest
@testable import ZoeKit

final class ModelMonitorChannelTests: XCTestCase {
    private func next(_ updates: inout AsyncStream<ModelMonitorUpdate>.Iterator) async throws -> ModelMonitorUpdate {
        let update = await updates.next()
        return try XCTUnwrap(update)
    }

    private func subscriptionID(in data: Data, type: String) throws -> String {
        let frame = try JSONDecoder().decode(JSONValue.self, from: data)
        XCTAssertEqual(frame[mcp: "channel"], .string("model_monitor"))
        XCTAssertEqual(frame[mcp: "type"], .string(type))
        guard case let .string(id)? = frame[mcp: "subscription_id"] else {
            XCTFail("missing subscription_id")
            return ""
        }
        return id
    }

    private func event(_ id: String, sequence: Int, type: String = "request", callID: String = "call-1") -> Data {
        Data(#"{"channel":"model_monitor","type":"event","subscription_id":"\#(id)","sequence":\#(sequence),"event":{"component":"worker","call_id":"\#(callID)","time":"2026-11-18T10:00:00Z","type":"\#(type)","payload":{"model":"m"},"truncated":false}}"#.utf8)
    }

    private func sentTypes(_ socket: FixtureMCPSocket) async throws -> [String] {
        var types: [String] = []
        for data in await socket.sent {
            let frame = try JSONDecoder().decode(JSONValue.self, from: data)
            guard case let .string(type)? = frame[mcp: "type"] else {
                XCTFail("frame without type")
                continue
            }
            types.append(type)
        }
        return types
    }

    func testSubscribesOnOpenSequencesGapsAndEndsWithoutReconnect() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) })
        let channel = await ModelMonitorChannel.attach(to: transport)
        var updates = await channel.begin().makeAsyncIterator()

        let connecting = try await next(&updates)
        XCTAssertEqual(connecting, .status(.connecting))
        try await awaitFixture(socket, .sent(1))
        let id = try subscriptionID(in: await socket.sent[0], type: "subscribe")
        let subscribing = try await next(&updates)
        XCTAssertEqual(subscribing, .status(.subscribing))

        await socket.push(Data(#"{"channel":"model_monitor","type":"subscribed","subscription_id":"\#(id)"}"#.utf8))
        let live = try await next(&updates)
        XCTAssertEqual(live, .status(.live))

        await socket.push(event(id, sequence: 1))
        guard case let .entry(firstEntry) = try await next(&updates), case let .event(firstEvent) = firstEntry.kind else {
            XCTFail("expected first event")
            return
        }
        XCTAssertEqual(firstEvent.component, "worker")
        XCTAssertEqual(firstEvent.kind, .request)
        XCTAssertEqual(firstEvent.callID, "call-1")
        XCTAssertNotNil(firstEvent.time)

        // Sequence 2 is missing, so a gap is emitted before sequence 3.
        await socket.push(event(id, sequence: 3, type: "response"))
        let gap = try await next(&updates)
        XCTAssertTrue(gap.isGap)
        guard case let .entry(thirdEntry) = try await next(&updates), case let .event(thirdEvent) = thirdEntry.kind else {
            XCTFail("expected sequence 3 event")
            return
        }
        XCTAssertEqual(thirdEvent.kind, .response)

        // A server gap frame is non-terminal and keeps the subscription live.
        await socket.push(Data(#"{"channel":"model_monitor","type":"gap","subscription_id":"\#(id)","sequence":5}"#.utf8))
        let serverGap = try await next(&updates)
        XCTAssertTrue(serverGap.isGap)

        // Terminal end: the subscription ends and does not reconnect on its own.
        await socket.push(Data(#"{"channel":"model_monitor","type":"error","subscription_id":"\#(id)","error":{"code":"subscription_ended"}}"#.utf8))
        let ended = try await next(&updates)
        XCTAssertEqual(ended, .status(.ended))
        try await awaitFixture(socket, .closed)
        await clock.advance(100)
        XCTAssertEqual(plan.count, 1)
        let sentCount = await socket.sent.count
        XCTAssertEqual(sentCount, 1)
    }

    func testReconnectMarksGapAndResubscribesWithNewSubscription() async throws {
        let first = FixtureMCPSocket(), second = FixtureMCPSocket()
        let plan = SocketPlan([first, second]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) })
        let channel = await ModelMonitorChannel.attach(to: transport)
        var updates = await channel.begin().makeAsyncIterator()

        try await awaitFixture(first, .sent(1))
        let firstID = try subscriptionID(in: await first.sent[0], type: "subscribe")
        await first.push(Data(#"{"channel":"model_monitor","type":"subscribed","subscription_id":"\#(firstID)"}"#.utf8))
        let connecting = try await next(&updates)
        XCTAssertEqual(connecting, .status(.connecting))
        let subscribing = try await next(&updates)
        XCTAssertEqual(subscribing, .status(.subscribing))
        let live = try await next(&updates)
        XCTAssertEqual(live, .status(.live))
        // Read the event before disconnecting, so the socket cannot close before the frame is consumed.
        await first.push(event(firstID, sequence: 1))
        let firstEvent = try await next(&updates)
        XCTAssertTrue(firstEvent.isEvent)
        await first.disconnect()

        // The reconnect backoff is the only clock sleep; it is observed directly, not by polling.
        await clock.awaitSleeper(seconds: 1)
        await clock.advance(1)
        try await awaitFixture(second, .sent(1))
        let secondID = try subscriptionID(in: await second.sent[0], type: "subscribe")
        XCTAssertNotEqual(firstID, secondID)

        await channel.stop()
        try await awaitFixture(second, .closed)
        let sent = await second.sent
        XCTAssertEqual(sent.count, 2)
        XCTAssertEqual(try subscriptionID(in: sent[1], type: "unsubscribe"), secondID)

        let gap = try await next(&updates)
        XCTAssertTrue(gap.isGap)
        let reconnecting = try await next(&updates)
        XCTAssertEqual(reconnecting, .status(.reconnecting))
        let resubscribing = try await next(&updates)
        XCTAssertEqual(resubscribing, .status(.subscribing))
        let idle = try await next(&updates)
        XCTAssertEqual(idle, .status(.idle))
        let reset = try await next(&updates)
        XCTAssertEqual(reset, .reset)
        let finished = await updates.next()
        XCTAssertNil(finished)
    }

    func testMonitorDemandDoesNotTearDownSharedSocket() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) })
        await transport.setNeeded(true)
        let channel = await ModelMonitorChannel.attach(to: transport)
        _ = await channel.begin()
        try await awaitFixture(socket, .sent(1))
        await channel.stop()
        try await awaitFixture(socket, .sent(2))
        // The MCP demand keeps the shared socket open after the monitor closes.
        let closes = await socket.closeCount
        XCTAssertEqual(closes, 0)
        XCTAssertEqual(plan.count, 1)
        await transport.setNeeded(false)
    }

    func testLateRegistrationOnOpenSocketSubscribesWithoutMissedOpen() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) })
        // Another channel opens the shared socket first; the monitor joins later and never receives the original opened callback.
        await transport.setNeeded(true)
        try await awaitFixture(socket, .opened)
        let channel = await ModelMonitorChannel.attach(to: transport)
        _ = await channel.begin()
        try await awaitFixture(socket, .sent(1))
        _ = try subscriptionID(in: await socket.sent[0], type: "subscribe")
        await transport.setNeeded(false)
    }

    func testStopAfterStartLeavesNoSubscriptionOrReconnect() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) })
        let channel = await ModelMonitorChannel.attach(to: transport)
        _ = await channel.begin()
        try await awaitFixture(socket, .sent(1))
        await channel.stop()
        try await awaitFixture(socket, .closed)
        let types = try await sentTypes(socket)
        XCTAssertEqual(types, ["subscribe", "unsubscribe"])
        await clock.advance(100)
        XCTAssertEqual(plan.count, 1)
    }

    func testStopBeforeBeginDoesNotCancelLaterStart() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) })
        let channel = await ModelMonitorChannel.attach(to: transport)
        await channel.stop()
        var updates = await channel.begin().makeAsyncIterator()
        let first = try await next(&updates)
        XCTAssertEqual(first, .status(.connecting))
        try await awaitFixture(socket, .sent(1))
        let types = try await sentTypes(socket)
        XCTAssertEqual(types, ["subscribe"])
        await channel.stop()
        try await awaitFixture(socket, .closed)
    }

    /// Overlap without a circular wait: `begin()` is parked inside the lifecycle queue after demand is set, `stop()` is launched
    /// as a child task, and the test waits only for `stop()` to be chained behind `begin()` before releasing the gate.
    /// Demand created a socket that is discarded without `opened()` or any subscribe, so no reconnect may follow.
    func testStopEnqueuedBehindParkedStartLeavesIdleWithoutReconnect() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) })
        let channel = await ModelMonitorChannel.attach(to: transport)
        let parked = AsyncGate(), release = AsyncGate(), stopEnqueued = AsyncGate()
        await channel.setStartGateForTesting {
            await parked.open()
            await release.wait()
        }
        let begin = Task { await channel.begin() }
        await parked.wait()
        // Arm the enqueue signal only now, so the first observed enqueue is stop's.
        await channel.setEnqueuedObserverForTesting { await stopEnqueued.open() }
        let stop = Task { await channel.stop() }
        await stopEnqueued.wait()
        await channel.setEnqueuedObserverForTesting(nil)
        // Stop is chained behind the parked begin and cannot run until release; releasing now cannot deadlock.
        await release.open()
        _ = await begin.value
        await stop.value
        await channel.setStartGateForTesting(nil)

        let finalStatus = await channel.status
        XCTAssertEqual(finalStatus, .idle)
        // A subscribe may have been sent before stop. Any such subscribe must be matched by an unsubscribe, with no residue.
        let types = try await sentTypes(socket)
        XCTAssertEqual(types.filter { $0 == "subscribe" }.count, types.filter { $0 == "unsubscribe" }.count)
        // Demand is gone, so no reconnect may happen after stop: the socket count must not grow across a backoff interval.
        let createdAtStop = plan.count
        await clock.advance(100)
        XCTAssertEqual(plan.count, createdAtStop)
        let sentAfter = try await sentTypes(socket)
        XCTAssertEqual(sentAfter, types)
    }

    /// Stop after a parked start has subscribed: the unsubscribe matches the subscribe and demand is released.
    func testStopAfterSubscribedStartSendsMatchingUnsubscribe() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) })
        let channel = await ModelMonitorChannel.attach(to: transport)
        _ = await channel.begin()
        try await awaitFixture(socket, .sent(1))
        await channel.stop()
        try await awaitFixture(socket, .closed)
        let types = try await sentTypes(socket)
        XCTAssertEqual(types, ["subscribe", "unsubscribe"])
        XCTAssertEqual(plan.count, 1)
    }

    func testBufferIsBoundedAndKeepsGapsWhenFiltered() {
        var buffer = ModelMonitorBuffer(capacity: 2)
        let event = ModelMonitorEvent(component: "steward", callID: "c", time: nil, kind: .request, payload: nil, truncated: false)
        buffer.append(ModelMonitorEntry(id: 1, kind: .event(event)))
        buffer.append(ModelMonitorEntry(id: 2, kind: .gap))
        buffer.append(ModelMonitorEntry(id: 3, kind: .event(ModelMonitorEvent(component: "worker", callID: "d", time: nil, kind: .response, payload: nil, truncated: false))))
        XCTAssertEqual(buffer.entries.map(\.id), [2, 3])
        XCTAssertEqual(buffer.droppedCount, 1)
        XCTAssertEqual(buffer.visible(components: ["steward"]).map(\.id), [2])
        buffer.clear()
        XCTAssertTrue(buffer.entries.isEmpty)
        XCTAssertEqual(buffer.droppedCount, 0)
    }
}

/// One-shot barrier: `wait()` suspends until `open()`; `open()` may run first.
actor AsyncGate {
    private var opened = false
    private var waiters: [CheckedContinuation<Void, Never>] = []

    func open() {
        opened = true
        for waiter in waiters { waiter.resume() }
        waiters.removeAll()
    }

    func wait() async {
        if opened { return }
        await withCheckedContinuation { waiters.append($0) }
    }
}

private extension ModelMonitorUpdate {
    var isGap: Bool {
        if case let .entry(entry) = self, case .gap = entry.kind { return true }
        return false
    }

    var isEvent: Bool {
        if case let .entry(entry) = self, case .event = entry.kind { return true }
        return false
    }
}
