#if os(macOS)
import A2UISwiftCore
import Foundation
import XCTest
@testable import ZoeUI

@MainActor
final class SurfaceActionTrackerTests: XCTestCase {
    private func makeSurface() throws -> (SurfaceModel, DataContext) {
        let catalog = basicCatalog
        let surface = SurfaceModel(id: UUID().uuidString, catalog: catalog)
        try surface.dataModel.set("/url", value: .string("https://example.test/a"))
        try surface.dataModel.set("/note", value: .string("keep me"))
        return (surface, DataContext(surface: surface, path: "/"))
    }

    func testBoundPathsComeOnlyFromDataBindingsInKeyOrder() {
        let context: [String: DynamicValue] = [
            "url": .dataBinding(path: "/url"),
            "label": .string("literal"),
            "count": .number(2),
            "note": .dataBinding(path: "/note")
        ]
        XCTAssertEqual(uiBoundPaths(in: context), ["/note", "/url"])
        XCTAssertEqual(uiBoundPaths(in: nil), [])
        XCTAssertEqual(uiBoundPaths(in: ["a": .string("x")]), [])
    }

    func testSuccessClearsOnlyThePathsBoundByThatAction() throws {
        let (surface, context) = try makeSurface()
        let tracker = SurfaceActionTracker()
        tracker.begin(component: "submit", context: context, paths: ["/url"])
        XCTAssertTrue(tracker.isPending("submit"))
        tracker.finish(component: "submit", success: true)
        XCTAssertFalse(tracker.isPending("submit"))
        XCTAssertNil(surface.dataModel.get("/url")?.stringValue)
        XCTAssertEqual(surface.dataModel.get("/note")?.stringValue, "keep me")
    }

    func testFailureKeepsEveryValueAndRestoresTheControl() throws {
        let (surface, context) = try makeSurface()
        let tracker = SurfaceActionTracker()
        tracker.begin(component: "submit", context: context, paths: ["/url", "/note"])
        tracker.finish(component: "submit", success: false)
        XCTAssertFalse(tracker.isPending("submit"))
        XCTAssertTrue(tracker.didFail("submit"))
        XCTAssertEqual(surface.dataModel.get("/url")?.stringValue, "https://example.test/a")
        XCTAssertEqual(surface.dataModel.get("/note")?.stringValue, "keep me")
    }

    func testDuplicateTapWhilePendingIsIgnoredAndRetryClearsFailure() throws {
        let (_, context) = try makeSurface()
        let tracker = SurfaceActionTracker()
        tracker.begin(component: "submit", context: context, paths: ["/url"])
        tracker.begin(component: "submit", context: context, paths: ["/note"])
        tracker.finish(component: "submit", success: false)
        XCTAssertTrue(tracker.didFail("submit"))
        tracker.begin(component: "submit", context: context, paths: ["/url"])
        XCTAssertFalse(tracker.didFail("submit"))
        XCTAssertTrue(tracker.isPending("submit"))
    }

    func testOutcomeRouterReachesOnlyRegisteredSurface() {
        let surfaceID = "router-test-" + UUID().uuidString
        var reports: [(String, Bool)] = []
        SurfaceActionOutcomes.shared.register(surfaceID: surfaceID) { component, success in reports.append((component, success)) }
        SurfaceActionOutcomes.shared.report(surfaceID: surfaceID, component: "submit", success: true)
        SurfaceActionOutcomes.shared.report(surfaceID: "other-" + surfaceID, component: "submit", success: false)
        SurfaceActionOutcomes.shared.unregister(surfaceID: surfaceID)
        SurfaceActionOutcomes.shared.report(surfaceID: surfaceID, component: "submit", success: false)
        XCTAssertEqual(reports.count, 1)
        XCTAssertEqual(reports.first?.1, true)
    }
}
#endif
