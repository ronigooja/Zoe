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

    private func payload(request: String = "r1", deletion: Bool = false) throws -> JSONValue {
        let messages = deletion
            ? #"[{"version":"v0.9","deleteSurface":{"surfaceId":"s1"}}]"#
            : #"[{"version":"v0.9.1","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}},{"version":"v0.9","updateComponents":{"surfaceId":"s1","components":[{"id":"root","component":"Text","text":"Hello"}]}}]"#
        return try JSONDecoder().decode(JSONValue.self, from: Data("{\"request_id\":\"\(request)\",\"surface_id\":\"s1\",\"container\":\"popover\",\"awaiting_input\":true,\"messages\":\(messages)}".utf8))
    }

    func testValidAndDeletePayloadDecoding() throws {
        let event = try XCTUnwrap(UISurfaceEvent.decode(payload()))
        XCTAssertEqual(event.requestID, "r1")
        XCTAssertEqual(event.surfaceID, "s1")
        XCTAssertEqual(event.container, .popover)
        XCTAssertTrue(event.awaitingInput)
        XCTAssertEqual(event.messages.count, 2)
        XCTAssertEqual(event.diagnosticSummary, "container=popover;messages=2;awaiting_input=1")
        XCTAssertFalse(event.diagnosticSummary.contains("Hello"))
        XCTAssertFalse(event.diagnosticSummary.contains("https://"))
        XCTAssertFalse(event.isDeletion)
        XCTAssertTrue(try XCTUnwrap(UISurfaceEvent.decode(payload(deletion: true))).isDeletion)
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

    func testRequestMapReplacementDeletionAndDisconnect() throws {
        var map = UISurfaceRequests()
        map.apply(try XCTUnwrap(UISurfaceEvent.decode(payload())))
        XCTAssertEqual(map.requestIDs, ["s1": "r1"])
        map.apply(try XCTUnwrap(UISurfaceEvent.decode(payload(request: "r2"))))
        XCTAssertEqual(map.requestIDs, ["s1": "r2"])
        map.apply(try XCTUnwrap(UISurfaceEvent.decode(payload(deletion: true))))
        XCTAssertTrue(map.requestIDs.isEmpty)
        map.apply(try XCTUnwrap(UISurfaceEvent.decode(payload())))
        map.removeAll()
        XCTAssertTrue(map.requestIDs.isEmpty)
    }

    func testExactCommandPayloadsAndStableKey() throws {
        let event = try XCTUnwrap(UISurfaceEvent.decode(payload()))
        XCTAssertEqual(event.renderedPayload, .object(["request_id": .string("r1")]))
        XCTAssertEqual(event.renderedIdempotencyKey(cursor: "10", version: 2), event.renderedIdempotencyKey(cursor: "10", version: 2))
        XCTAssertNotEqual(event.renderedIdempotencyKey(cursor: "10", version: 2), event.renderedIdempotencyKey(cursor: "11", version: 2))
        XCTAssertNotEqual(event.renderedIdempotencyKey(cursor: "10", version: 2), event.renderedIdempotencyKey(cursor: "10", version: 3))
        let action = ResolvedAction(name: "approve", sourceComponentId: "button", context: ["value": .bool(true), "nested": .dictionary(["count": .number(2)])])
        XCTAssertEqual(try uiActionPayload(requestID: "r1", surfaceID: "s1", action: action), .object([
            "request_id": .string("r1"), "surface_id": .string("s1"), "name": .string("approve"),
            "source_component_id": .string("button"), "context": .object(["value": .bool(true), "nested": .object(["count": .integer(2)])])
        ]))
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
