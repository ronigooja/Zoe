import XCTest
import A2UISwiftCore
@testable import ZoeKit

private final class TestClock: SilenceClock {
    var date: Date
    init(_ date: Date = Date(timeIntervalSince1970: 0)) { self.date = date }
    func now() -> Date { date }
}


@MainActor
final class ScreenTests: XCTestCase {
    func testSilenceDoesNotCountInteractionAndTimesOutDeterministically() {
        let clock = TestClock()
        let controller = SilenceController(timeoutSeconds: 10, clock: clock)
        controller.noteVoiceActivity()
        controller.setInteractionInProgress(true)
        clock.date = Date(timeIntervalSince1970: 100)
        XCTAssertEqual(controller.tick(), .none)
        clock.date = Date(timeIntervalSince1970: 1000)
        XCTAssertEqual(controller.tick(), .none)
        controller.setInteractionInProgress(false)
        XCTAssertEqual(controller.tick(), .none)
        clock.date = Date(timeIntervalSince1970: 1010)
        XCTAssertEqual(controller.tick(), .timedOut)
        XCTAssertEqual(controller.tick(), .none)
        controller.reset()
        XCTAssertEqual(controller.tick(), .none)
    }

    func testDefaultSilenceTimeoutIsSixtySeconds() {
        let clock = TestClock()
        let controller = SilenceController(clock: clock)
        controller.noteVoiceActivity()
        clock.date = Date(timeIntervalSince1970: 59)
        XCTAssertEqual(controller.tick(), .none)
        clock.date = Date(timeIntervalSince1970: 60)
        XCTAssertEqual(controller.tick(), .timedOut)
    }

    func testSurfaceLifecycleAndUnknownUpdate() throws {
        var store = SurfaceStore()
        store.update(id: "missing", messages: [])
        XCTAssertTrue(store.surfaces.isEmpty)
        for kind in [SurfaceKind.popover, .window] {
            store.open(SurfaceSpec(id: kind.rawValue, kind: kind, messages: []))
        }
        XCTAssertEqual(store.surfaces.count, 2)
        store.open(SurfaceSpec(id: "window", kind: .popover, messages: []))
        XCTAssertEqual(store.surfaces["window"]?.kind, .popover)
        store.close(id: "window")
        store.close(id: "window")
        XCTAssertEqual(store.surfaces.count, 1)
    }

    func testMessagesResetOnCreateAndPreserveUpdates() throws {
        let decoder = JSONDecoder()
        func message(_ json: String) throws -> A2uiMessage {
            try decoder.decode(A2uiMessage.self, from: Data(json.utf8))
        }
        let create = try message(#"{"version":"v0.9","createSurface":{"surfaceId":"one","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}"#)
        let update = A2uiMessage.updateComponents(UpdateComponentsPayload(surfaceId: "one", components: [RawComponent(id: "root", component: "Text", properties: ["text": .string("Hello")])]))
        var store = SurfaceStore()
        store.open(SurfaceSpec(id: "one", kind: .window, messages: [create, update]))
        XCTAssertEqual(store.surfaces["one"]?.messages.count, 2)
        store.update(id: "one", messages: [update])
        XCTAssertEqual(store.surfaces["one"]?.messages.count, 3)
        store.update(id: "one", messages: [create, update])
        XCTAssertEqual(store.surfaces["one"]?.messages.count, 2)
    }
}
