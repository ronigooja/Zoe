import XCTest
import A2UISwiftCore
@testable import ZoeKit
@testable import ZoeUI

final class UISurfaceTests: XCTestCase {
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
        XCTAssertFalse(event.isDeletion)
        XCTAssertTrue(try XCTUnwrap(UISurfaceEvent.decode(payload(deletion: true))).isDeletion)
    }

    func testMalformedPayloadIsIgnored() throws {
        XCTAssertNil(UISurfaceEvent.decode(.object([:])))
        let data = try JSONEncoder().encode(payload())
        let json = String(decoding: data, as: UTF8.self)
        for invalid in [json.replacingOccurrences(of: "v0.9.1", with: "v0.8"),
                        json.replacingOccurrences(of: "popover", with: "panel"),
                        json.replacingOccurrences(of: "surfaceId", with: "surface_id")] {
            XCTAssertNil(UISurfaceEvent.decode(try JSONDecoder().decode(JSONValue.self, from: Data(invalid.utf8))))
        }
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
