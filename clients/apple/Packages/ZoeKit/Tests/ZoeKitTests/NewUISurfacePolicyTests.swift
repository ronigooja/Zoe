import XCTest
@testable import ZoeUI

final class NewUISurfacePolicyTests: XCTestCase {
    func testAddWindowPreservesAllPresentedSurfaces() {
        let plan = NewUISurfacePlan(behavior: .addWindow,
            presentedIDs: ["window", "popover", "awaiting-input"], newSurfaceID: "new")
        XCTAssertEqual(plan.closeIDs, [])
        XCTAssertEqual(plan.showID, "new")
    }

    func testReplacementClosesAllOldSurfacePresentations() {
        let presented: Set<String> = ["window", "popover", "awaiting-input"]
        let plan = NewUISurfacePlan(behavior: .closeOldWindows,
            presentedIDs: presented, newSurfaceID: "new")
        XCTAssertEqual(plan.closeIDs, presented)
        XCTAssertEqual(plan.showID, "new")
    }

    func testFirstArrivalHasNothingToCloseForEitherPreference() {
        for behavior in NewUISurfaceBehavior.allCases {
            let plan = NewUISurfacePlan(behavior: behavior, presentedIDs: [], newSurfaceID: "new")
            XCTAssertEqual(plan.closeIDs, [])
            XCTAssertEqual(plan.showID, "new")
        }
    }

    func testExistingSurfaceUpdateDoesNotCloseOtherPresentations() {
        let plan = NewUISurfacePlan(behavior: .closeOldWindows,
            presentedIDs: ["existing", "other"], newSurfaceID: "existing")
        XCTAssertEqual(plan.closeIDs, [])
        XCTAssertEqual(plan.showID, "existing")
    }

    @MainActor
    func testPreferenceDefaultsPersistenceAndUnknownValueFallback() {
        let name = "NewUISurfacePolicyTests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: name)!
        defer { defaults.removePersistentDomain(forName: name) }
        let model = ZoeAppModel(defaults: defaults)
        XCTAssertEqual(model.newUISurfaceBehavior, .addWindow)
        model.setNewUISurfaceBehavior(.closeOldWindows)
        XCTAssertEqual(ZoeAppModel(defaults: defaults).newUISurfaceBehavior, .closeOldWindows)
        model.setNewUISurfaceBehavior(.addWindow)
        XCTAssertEqual(ZoeAppModel(defaults: defaults).newUISurfaceBehavior, .addWindow)
        defaults.set("unknown", forKey: "zoe.ui.newUISurfaceBehavior")
        XCTAssertEqual(ZoeAppModel(defaults: defaults).newUISurfaceBehavior, .addWindow)
    }
}
