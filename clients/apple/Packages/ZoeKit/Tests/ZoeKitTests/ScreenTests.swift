import XCTest
import A2UISwiftCore
@testable import ZoeKit

@MainActor
final class ScreenTests: XCTestCase {
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
