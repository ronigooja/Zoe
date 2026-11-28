import Foundation
import XCTest
@testable import ZoeKit

final class ModelMonitorConversationTests: XCTestCase {
    private func request(_ id: Int, component: String = "worker", conversation: String? = "conv-1", start: Bool = false,
                         incomplete: Bool = false, truncated: Bool = false, messages: [ModelMonitorMessage]?, callID: String? = nil) -> ModelMonitorEntry {
        ModelMonitorEntry(id: id, kind: .event(ModelMonitorEvent(
            component: component, callID: callID ?? "call-\(id)", time: nil, kind: .request, payload: .string("p\(id)"), truncated: truncated,
            conversationID: conversation, conversationStart: start, incomplete: incomplete, requestMessages: messages)))
    }

    private func msg(_ role: String, _ text: String) -> ModelMonitorMessage {
        ModelMonitorMessage(role: role, text: text)
    }

    private func appended(_ buffer: ModelMonitorBuffer, id: Int) -> [ModelMonitorMessage]? {
        for entry in buffer.entries where entry.id == id {
            if case let .event(event) = entry.kind { return event.appendedMessages }
        }
        XCTFail("missing entry \(id)")
        return nil
    }

    func testExactOrderedPrefixYieldsOnlyAppendedMessages() {
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(request(1, messages: [msg("system", "s"), msg("user", "a")]))
        buffer.append(request(2, messages: [msg("system", "s"), msg("user", "a"), msg("assistant", "b"), msg("user", "c")]))
        XCTAssertNil(appended(buffer, id: 1))
        XCTAssertEqual(appended(buffer, id: 2), [msg("assistant", "b"), msg("user", "c")])
    }

    func testChangedOrReorderedHistoryIsNotShownAsDelta() {
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(request(1, messages: [msg("user", "a"), msg("user", "b")]))
        buffer.append(request(2, messages: [msg("user", "b"), msg("user", "a"), msg("user", "c")]))
        XCTAssertNil(appended(buffer, id: 2))
    }

    func testShorterSnapshotIsNotShownAsDelta() {
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(request(1, messages: [msg("user", "a"), msg("user", "b")]))
        buffer.append(request(2, messages: [msg("user", "a")]))
        XCTAssertNil(appended(buffer, id: 2))
    }

    func testConversationStartIsNeverDeltaEvenWithPrefix() {
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(request(1, messages: [msg("user", "a")]))
        buffer.append(request(2, start: true, messages: [msg("user", "a"), msg("user", "b")]))
        XCTAssertNil(appended(buffer, id: 2))
    }

    func testConversationStartWithoutIDIsStillMarkedAndNotGrouped() {
        let event = ModelMonitorEvent(component: "worker", callID: "c", time: nil, kind: .request, payload: nil, truncated: false,
                                      conversationStart: true)
        XCTAssertTrue(event.conversationStart)
        XCTAssertNil(event.conversationID)
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(ModelMonitorEntry(id: 1, kind: .event(event)))
        buffer.append(request(2, conversation: nil, messages: [msg("user", "a")]))
        buffer.append(request(3, conversation: nil, messages: [msg("user", "a"), msg("user", "b")]))
        XCTAssertNil(appended(buffer, id: 3))
    }

    func testGapBreaksHistoryEvenWhenSnapshotsMatch() {
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(request(1, messages: [msg("user", "a")]))
        buffer.append(ModelMonitorEntry(id: 2, kind: .gap))
        buffer.append(request(3, messages: [msg("user", "a"), msg("user", "b")]))
        XCTAssertNil(appended(buffer, id: 3))
    }

    func testIncompleteSnapshotBreaksHistoryAndIsNotBaseline() {
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(request(1, messages: [msg("user", "a")]))
        buffer.append(request(2, incomplete: true, messages: [msg("user", "a"), msg("user", "b")]))
        buffer.append(request(3, messages: [msg("user", "a"), msg("user", "b"), msg("user", "c")]))
        XCTAssertNil(appended(buffer, id: 3))
    }

    func testGroupIsolatedByComponentAndConversation() {
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(request(1, component: "worker", messages: [msg("user", "a")]))
        buffer.append(request(2, component: "steward", messages: [msg("user", "a"), msg("user", "b")]))
        buffer.append(request(3, component: "worker", conversation: "conv-2", messages: [msg("user", "a"), msg("user", "b")]))
        XCTAssertNil(appended(buffer, id: 2))
        XCTAssertNil(appended(buffer, id: 3))
    }

    func testInterleavedReplyDoesNotBreakRequestGroup() {
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(request(1, messages: [msg("user", "a")]))
        buffer.append(ModelMonitorEntry(id: 2, kind: .event(ModelMonitorEvent(component: "worker", callID: "c", time: nil, kind: .response,
                                                                              payload: nil, truncated: false, conversationID: "conv-1"))))
        buffer.append(request(3, messages: [msg("user", "a"), msg("user", "b")]))
        XCTAssertEqual(appended(buffer, id: 3), [msg("user", "b")])
    }

    func testTrimmedBaselineIsNotUsed() {
        var buffer = ModelMonitorBuffer(capacity: 2)
        buffer.append(request(1, messages: [msg("user", "a")]))
        buffer.append(request(2, conversation: "other", messages: [msg("user", "unrelated")]))
        buffer.append(request(3, messages: [msg("user", "a"), msg("user", "b")]))
        XCTAssertNil(appended(buffer, id: 3))
    }

    func testTruncatedSnapshotIsNotUsedAndPrefixIsByteExact() {
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(request(1, truncated: true, messages: [msg("user", "a")]))
        buffer.append(request(2, messages: [msg("user", "a"), msg("user", "é")]))
        buffer.append(request(3, messages: [msg("user", "a"), msg("user", "e\u{301}"), msg("user", "b")]))
        XCTAssertNil(appended(buffer, id: 2))
        XCTAssertNil(appended(buffer, id: 3))
    }

    func testOpenCodeFeedKeepsDeltaWithinItsOwnBuffer() {
        var feed = ModelMonitorFeed(capacity: 10)
        feed.append(request(1, component: "opencode_context", messages: [msg("user", "a")]))
        feed.append(request(2, component: "opencode_context", messages: [msg("user", "a"), msg("user", "b")]))
        let entries = feed.visibleEntries(component: "opencode_context")
        guard case let .event(event) = entries.last?.kind else { return XCTFail("expected event") }
        XCTAssertEqual(event.appendedMessages, [msg("user", "b")])
        XCTAssertTrue(feed.visibleEntries(component: nil).isEmpty)
    }

    func testDeltaDoesNotChangePayloadOrCopy() {
        let payload = JSONValue.object(["messages": .array([.string("a"), .string("b")])])
        let event = ModelMonitorEvent(component: "worker", callID: "c", time: nil, kind: .request, payload: payload, truncated: false,
                                      conversationID: "conv-1", requestMessages: [msg("user", "a")])
        var buffer = ModelMonitorBuffer(capacity: 10)
        buffer.append(ModelMonitorEntry(id: 1, kind: .event(event)))
        buffer.append(ModelMonitorEntry(id: 2, kind: .event(ModelMonitorEvent(component: "worker", callID: "c2", time: nil, kind: .request,
                                                                              payload: payload, truncated: false, conversationID: "conv-1",
                                                                              requestMessages: [msg("user", "a"), msg("user", "b")]))))
        guard case let .event(displayed) = buffer.entries[1].kind else { return XCTFail("expected event") }
        XCTAssertEqual(displayed.appendedMessages, [msg("user", "b")])
        // Payload and its copy text are the original formatted payload, unaffected by the delta.
        XCTAssertEqual(displayed.payload, payload)
        XCTAssertEqual(displayed.formattedPayload, ModelMonitorEvent(component: "worker", callID: "c2", time: nil, kind: .request,
                                                                     payload: payload, truncated: false).formattedPayload)
    }

    func testCorrelationHashIsFirstEightBytesLowercaseHex() {
        let hash = ModelMonitorDiagnostics.correlationHash(subscriptionID: "sub-1")
        XCTAssertEqual(hash.count, 16)
        XCTAssertTrue(hash.allSatisfy { "0123456789abcdef".contains($0) })
        // SHA-256("abc") begins ba7816bf8f01cfea; the first eight bytes are the hash prefix.
        XCTAssertEqual(ModelMonitorDiagnostics.correlationHash(subscriptionID: "abc"), "ba7816bf8f01cfea")
    }

    func testDiagnosticsCoalesceRepeatedReasonWithinIntervalAndReportCount() {
        var diagnostics = ModelMonitorDiagnostics()
        let start = Date(timeIntervalSince1970: 1_000)
        XCTAssertEqual(diagnostics.note(.serverGap, now: start, correlationHash: "h", actual: 5),
                       "reason=server_gap;correlation_hash=h;actual=5")
        XCTAssertNil(diagnostics.note(.serverGap, now: start.addingTimeInterval(1), correlationHash: "h", actual: 6))
        XCTAssertNil(diagnostics.note(.serverGap, now: start.addingTimeInterval(2), correlationHash: "h", actual: 7))
        // A different reason is not coalesced with the first.
        XCTAssertEqual(diagnostics.note(.sequenceJump, now: start.addingTimeInterval(2), correlationHash: "h", expected: 2, actual: 3),
                       "reason=sequence_jump;correlation_hash=h;expected=2;actual=3")
        XCTAssertEqual(diagnostics.note(.serverGap, now: start.addingTimeInterval(11), correlationHash: "h", actual: 8),
                       "reason=server_gap;correlation_hash=h;actual=8;coalesced=2")
    }

    func testDiagnosticsFlushFinalOccurrencesWithTheirMetadata() {
        let start = Date(timeIntervalSince1970: 1_000)
        var diagnostics = ModelMonitorDiagnostics()
        _ = diagnostics.note(.sequenceJump, now: start, correlationHash: "older", expected: 1, actual: 2)
        XCTAssertNil(diagnostics.note(.sequenceJump, now: start, correlationHash: "newer", expected: 3, actual: 6))
        XCTAssertEqual(diagnostics.flush(), ["reason=sequence_jump;correlation_hash=newer;expected=3;actual=6;coalesced=1"])
        XCTAssertTrue(diagnostics.flush().isEmpty)
    }

    func testSequenceJumpDiagnosticCarriesExpectedAndActualMetadataOnly() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) })
        let lines = DiagnosticLines()
        let channel = await ModelMonitorChannel.attach(to: transport, diagnostic: { lines.append($0) })
        var updates = await channel.begin().makeAsyncIterator()
        _ = await updates.next()
        try await awaitFixture(socket, .sent(1))
        let subscribe = try JSONDecoder().decode(JSONValue.self, from: await socket.sent[0])
        guard case let .string(id)? = subscribe[mcp: "subscription_id"] else { return XCTFail("missing id") }
        await socket.push(Data(#"{"channel":"model_monitor","type":"subscribed","subscription_id":"\#(id)"}"#.utf8))
        await socket.push(Data(#"{"channel":"model_monitor","type":"event","subscription_id":"\#(id)","sequence":1,"event":{"component":"worker","call_id":"c","time":"2026-11-18T10:00:00Z","type":"request","payload":{"text":"secret body"},"truncated":false}}"#.utf8))
        await socket.push(Data(#"{"channel":"model_monitor","type":"event","subscription_id":"\#(id)","sequence":4,"event":{"component":"worker","call_id":"c","time":"2026-11-18T10:00:00Z","type":"response","payload":{"text":"secret body"},"truncated":false}}"#.utf8))
        // Updates after connecting: subscribing, live, event 1, gap, event 4.
        for _ in 0..<5 { _ = await updates.next() }
        let recorded = lines.values
        let jumps = recorded.filter { $0.hasPrefix("reason=sequence_jump;") }
        XCTAssertEqual(jumps.count, 1)
        let line = try XCTUnwrap(jumps.first)
        XCTAssertTrue(line.hasPrefix("reason=sequence_jump;correlation_hash=" + ModelMonitorDiagnostics.correlationHash(subscriptionID: id)))
        XCTAssertTrue(line.hasSuffix(";expected=2;actual=4"))
        XCTAssertFalse(recorded.contains { $0.contains("secret") })
        await channel.stop()
    }

    func testOlderFramesWithoutNewFieldsDecodeAsBeforeAndMalformedMessagesDropOnlyTheSnapshot() async throws {
        let socket = FixtureMCPSocket(), plan = SocketPlan([socket]), clock = ManualMCPClock()
        let transport = ClientWebSocketTransport(api: try mcpTestAPI(), clock: clock, factory: { plan.make($0) })
        let channel = await ModelMonitorChannel.attach(to: transport)
        var updates = await channel.begin().makeAsyncIterator()
        _ = await updates.next()
        try await awaitFixture(socket, .sent(1))
        let subscribe = try JSONDecoder().decode(JSONValue.self, from: await socket.sent[0])
        guard case let .string(id)? = subscribe[mcp: "subscription_id"] else { return XCTFail("missing id") }
        await socket.push(Data(#"{"channel":"model_monitor","type":"subscribed","subscription_id":"\#(id)"}"#.utf8))
        await socket.push(Data(#"{"channel":"model_monitor","type":"event","subscription_id":"\#(id)","sequence":1,"event":{"component":"worker","call_id":"old","time":"2026-11-18T10:00:00Z","type":"request","payload":{"model":"m"},"truncated":false}}"#.utf8))
        await socket.push(Data(#"{"channel":"model_monitor","type":"event","subscription_id":"\#(id)","sequence":2,"event":{"component":"worker","call_id":"bad","time":"2026-11-18T10:00:00Z","type":"request","payload":{"model":"m"},"truncated":false,"conversation_id":"x","request_messages":"not-a-list"}}"#.utf8))
        // Updates after connecting: subscribing, live, old event, malformed-snapshot event.
        var events: [ModelMonitorEvent] = []
        for _ in 0..<4 {
            if let event = await updates.next()?.entryEvent { events.append(event) }
        }
        XCTAssertEqual(events.map(\.callID), ["old", "bad"])
        XCTAssertNil(events[0].conversationID)
        XCTAssertNil(events[0].requestMessages)
        // Malformed snapshot drops only the snapshot; the event and its conversation ID still decode.
        XCTAssertEqual(events[1].conversationID, "x")
        XCTAssertNil(events[1].requestMessages)
        await channel.stop()
    }
}

/// Thread-safe collector for diagnostic lines emitted from the channel actor.
final class DiagnosticLines: @unchecked Sendable {
    private let lock = NSLock()
    private var lines: [String] = []
    func append(_ line: String) { lock.withLock { lines.append(line) } }
    var values: [String] { lock.withLock { lines } }
}

private extension ModelMonitorUpdate {
    var entryEvent: ModelMonitorEvent? {
        if case let .entry(entry) = self, case let .event(event) = entry.kind { return event }
        return nil
    }
}
