import XCTest
import A2UISwiftCore
@testable import ZoeKit
@testable import ZoeUI

final class UISurfaceTests: XCTestCase {
    func testOptionalSizeAndInvalidFractionalSize() throws {
        guard case var .object(fields) = try payload() else { return XCTFail("object") }
        XCTAssertNil(try XCTUnwrap(UISurfaceEvent.decode(.object(fields))).size)
        fields["size"] = .object(["width": .integer(700), "height": .integer(500)])
        XCTAssertEqual(try XCTUnwrap(UISurfaceEvent.decode(.object(fields))).size, SurfaceSize(width: 700, height: 500))
        fields["size"] = .object(["width": .number(700.5), "height": .integer(500)])
        XCTAssertEqual(UISurfaceEvent.decodeWithDiagnostic(.object(fields)).failure, "payload_decode")
    }

    func testScreenCommandPayloadAndKey() {
        let size = SurfaceSize(width: 1440, height: 875)
        XCTAssertEqual(size.screenPayload, .object(["width": .integer(1440), "height": .integer(875)]))
        XCTAssertEqual(size.screenIdempotencyKey, "ui.screen-1440-875")
        XCTAssertEqual(size.screenIdempotencyKey, SurfaceSize(width: 1440, height: 875).screenIdempotencyKey)
        XCTAssertNotEqual(size.screenIdempotencyKey, SurfaceSize(width: 1441, height: 875).screenIdempotencyKey)
        XCTAssertNotEqual(size.screenIdempotencyKey, SurfaceSize(width: 1440, height: 876).screenIdempotencyKey)
    }

    private func payload(request: String = "r1", version: Int64 = 1) throws -> JSONValue {
        let messages = #"[{"version":"v0.9.1","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}},{"version":"v0.9","updateComponents":{"surfaceId":"s1","components":[{"id":"root","component":"Text","text":"Hello"}]}}]"#
        return try JSONDecoder().decode(JSONValue.self, from: Data("{\"request_id\":\"\(request)\",\"surface_id\":\"s1\",\"surface_version\":\(version),\"container\":\"popover\",\"awaiting_input\":true,\"task_id\":\"t1\",\"question_id\":\"q1\",\"messages\":\(messages)}".utf8))
    }

    func testValidPayloadWithoutDeletedDecoding() throws {
        let event = try XCTUnwrap(UISurfaceEvent.decode(payload()))
        XCTAssertEqual(event.requestID, "r1")
        XCTAssertEqual(event.surfaceID, "s1")
        XCTAssertEqual(event.container, .popover)
        XCTAssertTrue(event.awaitingInput)
        XCTAssertEqual(event.messages.count, 2)
        XCTAssertEqual(event.diagnosticSummary, "container=popover;messages=2;awaiting_input=1")
        XCTAssertFalse(event.diagnosticSummary.contains("Hello"))
        XCTAssertFalse(event.diagnosticSummary.contains("https://"))
    }

    func testLegacyDeletedFieldIsIgnoredInEventsAndSnapshots() throws {
        guard case var .object(fields) = try payload() else { return XCTFail("object") }
        for legacyValue in [false, true] {
            fields["deleted"] = .bool(legacyValue)
            XCTAssertNotNil(UISurfaceEvent.decode(.object(fields)))
            fields["render_status"] = .string("rendered")
            let snapshot = JSONValue.object(["cursor": .integer(20), "surfaces": .array([.object(fields)])])
            XCTAssertEqual(try JSONDecoder().decode(UISnapshot.self, from: JSONEncoder().encode(snapshot)).surfaces.count, 1)
        }
    }

    func testRemovedStatusContainerIsRejected() throws {
        guard case var .object(fields) = try payload() else { return XCTFail("object") }
        fields["container"] = .string("statusItem")
        let payload = JSONValue.object(fields)
        let data = try JSONEncoder().encode(payload)
        XCTAssertThrowsError(try JSONDecoder().decode(UISurfaceEvent.self, from: data))
        XCTAssertNil(UISurfaceEvent.decode(payload))
        let result = UISurfaceEvent.decodeWithDiagnostic(payload)
        XCTAssertNil(result.event)
        XCTAssertEqual(result.failure, "payload_decode")
    }

    func testMalformedPayloadIsIgnored() throws {
        XCTAssertEqual(UISurfaceEvent.decodeWithDiagnostic(.object([:])).failure, "payload_decode")
        XCTAssertNil(UISurfaceEvent.decode(.object([:])))
        let data = try JSONEncoder().encode(payload())
        let json = String(decoding: data, as: UTF8.self)
        for invalid in [json.replacingOccurrences(of: "v0.9.1", with: "v0.8"),
                        json.replacingOccurrences(of: "popover", with: "panel"),
                        json.replacingOccurrences(of: "surfaceId", with: "surface_id")] {
            XCTAssertNil(UISurfaceEvent.decode(try JSONDecoder().decode(JSONValue.self, from: Data(invalid.utf8))))
        }
    }

    func testDiagnosticFailureStepsAndNoPayloadLeakage() throws {
        var object: [String: JSONValue]
        guard case let .object(fields) = try payload() else { return XCTFail("object") }
        object = fields
        object["request_id"] = .string("")
        XCTAssertEqual(UISurfaceEvent.decodeWithDiagnostic(.object(object)).failure, "request_id")
        object = fields
        object["messages"] = .array([])
        XCTAssertEqual(UISurfaceEvent.decodeWithDiagnostic(.object(object)).failure, "messages_empty")
        object = fields
        object["surface_id"] = .string("different")
        XCTAssertEqual(UISurfaceEvent.decodeWithDiagnostic(.object(object)).failure, "surface_id_mismatch")
        object = fields
        if case let .array(messages) = fields["messages"] { object["messages"] = .array(Array(messages.dropFirst())) }
        XCTAssertEqual(UISurfaceEvent.decodeWithDiagnostic(.object(object)).failure, "first_not_createSurface")
        object = fields
        object["request_id"] = .string("sk-secret-payload")
        object["container"] = .string("sk-secret-payload")
        let summary = UISurfaceEvent.receivedSummary(payload: .object(object), cursor: "sk-secret-payload")
        XCTAssertFalse(summary.contains("sk-secret-payload"))
        XCTAssertLessThanOrEqual(summary.count, 200)
        XCTAssertEqual(UISurfaceEvent.receivedSummary(payload: try payload(), cursor: "10"), "container=popover;messages=2;awaiting_input=1;cursor=10")
    }

    func testRequestMapReplacementAndDisconnect() throws {
        var map = UISurfaceRequests()
        map.apply(try XCTUnwrap(UISurfaceEvent.decode(payload())))
        XCTAssertEqual(map.requestIDs, ["s1": "r1"])
        map.apply(try XCTUnwrap(UISurfaceEvent.decode(payload(request: "r2", version: 2))))
        XCTAssertEqual(map.requestIDs, ["s1": "r2"])
        map.removeAll()
        XCTAssertTrue(map.requestIDs.isEmpty)
    }

    func testExactCommandPayloadsAndStableKey() throws {
        let event = try XCTUnwrap(UISurfaceEvent.decode(payload()))
        XCTAssertEqual(event.renderedPayload(success: true), .object([
            "request_id": .string("r1"), "surface_id": .string("s1"), "surface_version": .integer(1), "success": .bool(true)
        ]))
        XCTAssertEqual(event.renderedIdempotencyKey, try XCTUnwrap(UISurfaceEvent.decode(payload())).renderedIdempotencyKey)
        XCTAssertNotEqual(event.renderedIdempotencyKey, try XCTUnwrap(UISurfaceEvent.decode(payload(version: 2))).renderedIdempotencyKey)
        let action = ResolvedAction(name: "approve", sourceComponentId: "button", context: ["value": .bool(true), "nested": .dictionary(["count": .number(2)])])
        XCTAssertEqual(try uiActionPayload(binding: event.binding, action: action), .object([
            "request_id": .string("r1"), "surface_id": .string("s1"), "name": .string("approve"),
            "surface_version": .integer(1),
            "source_component_id": .string("button"), "context": .object(["value": .bool(true), "nested": .object(["count": .integer(2)])])
        ]))
    }

    func testRequiredVersionAndInputBinding() throws {
        guard case let .object(fields) = try payload() else { return XCTFail("object") }
        for key in ["request_id", "surface_id", "surface_version", "container", "awaiting_input", "messages", "task_id", "question_id"] {
            var invalid = fields
            invalid.removeValue(forKey: key)
            XCTAssertNil(UISurfaceEvent.decode(.object(invalid)), key)
        }
        for version in [JSONValue.integer(0), .integer(-1), .number(1.5), .string("1")] {
            var invalid = fields
            invalid["surface_version"] = version
            XCTAssertNil(UISurfaceEvent.decode(.object(invalid)))
        }
    }

    func testStaleReplayAndFailedACKState() throws {
        var state = UISurfaceRequests()
        let first = try XCTUnwrap(UISurfaceEvent.decode(payload(version: 2)))
        XCTAssertTrue(state.apply(first))
        state.recordRender(first, success: false)
        XCTAssertEqual(state.outcomes[first.binding], false)
        XCTAssertTrue(state.apply(first), "A failed native render must permit retry of the exact revision")
        XCTAssertEqual(state.outcomes[first.binding], false, "Reservation alone does not claim successful rendering")
        state.recordRender(first, success: true)
        XCTAssertEqual(state.outcomes[first.binding], true, "Successful retry replaces the failed outcome")
        XCTAssertFalse(state.apply(first), "A successfully rendered revision must not reopen on duplicate replay")
        XCTAssertFalse(state.apply(try XCTUnwrap(UISurfaceEvent.decode(payload()))))
    }

    func testSnapshotValidationAndRepairOrdering() throws {
        guard case var .object(fields) = try payload(version: 2) else { return XCTFail("object") }
        fields["render_status"] = .string("generated")
        let value: JSONValue = .object(["cursor": .integer(20), "surfaces": .array([.object(fields)])])
        let snapshot = try JSONDecoder().decode(UISnapshot.self, from: JSONEncoder().encode(value))
        XCTAssertEqual(snapshot.cursor, 20)
        var state = UISurfaceRequests()
        XCTAssertTrue(state.apply(try XCTUnwrap(UISurfaceEvent.decode(payload()))))
        XCTAssertTrue(state.reconcile(snapshot).isEmpty)
        XCTAssertTrue(state.apply(snapshot.surfaces[0].event))
        // A commit racing the snapshot is delivered after cursor 20, not discarded as a replay.
        XCTAssertTrue(state.apply(try XCTUnwrap(UISurfaceEvent.decode(payload(version: 3)))))
        XCTAssertFalse(state.apply(snapshot.surfaces[0].event))
        fields.removeValue(forKey: "render_status")
        let invalid: JSONValue = .object(["cursor": .integer(99), "surfaces": .array([.object(fields)])])
        XCTAssertThrowsError(try JSONDecoder().decode(UISnapshot.self, from: JSONEncoder().encode(invalid)))
        XCTAssertEqual(state.versions["s1"], 3)
    }

    func testActionUsesImmutableRenderedBinding() throws {
        var state = UISurfaceRequests()
        let old = try XCTUnwrap(UISurfaceEvent.decode(payload()))
        state.apply(old)
        let clickedBinding = old.binding
        state.apply(try XCTUnwrap(UISurfaceEvent.decode(payload(request: "r2", version: 2))))
        let action = ResolvedAction(name: "answer", sourceComponentId: "submit", context: ["value": .string("full answer")])
        guard case let .object(fields) = try uiActionPayload(binding: clickedBinding, action: action) else { return XCTFail("object") }
        XCTAssertEqual(fields["request_id"], .string("r1"))
        XCTAssertEqual(fields["surface_version"], .integer(1))
        XCTAssertEqual(fields["context"], .object(["value": .string("full answer")]))
    }

    @MainActor func testMainActorDeliveryRejectsStaleGenerationAndMalformedEvents() async throws {
        let model = ZoeAppModel()
        model.setAuthenticationForTesting(.connected)
        var delivered = 0
        model.onUISurface = { _ in delivered += 1; return false }
        var envelope = EventEnvelope(schemaVersion: 1, eventId: "e1", cursor: "10", type: "ui.surface", occurredAt: "", workspaceId: "w1", aggregateType: "ui_surface", aggregateId: "s1", aggregateVersion: 1, payload: try payload())
        await model.handleEventForTesting(envelope)
        XCTAssertEqual(delivered, 1)
        await model.handleEventForTesting(envelope, expectedConnectionEpoch: UInt64.max)
        XCTAssertEqual(delivered, 1)
        envelope.payload = .object([:])
        await model.handleEventForTesting(envelope)
        XCTAssertEqual(delivered, 1)
        model.setAuthenticationForTesting(.disconnected)
        envelope.payload = try payload()
        await model.handleEventForTesting(envelope)
        XCTAssertEqual(delivered, 1)
    }
}
