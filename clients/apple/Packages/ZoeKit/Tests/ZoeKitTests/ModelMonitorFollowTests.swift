import XCTest
@testable import ZoeKit

final class ModelMonitorFollowTests: XCTestCase {
    func testFreshStateFollowsAndFirstMeasurementOnlySetsBaseline() {
        var follow = ModelMonitorFollow()
        XCTAssertTrue(follow.isAtBottom)
        // A user scrolled away before any baseline cannot be told apart from layout, so the first measurement is not a scroll.
        follow.measure(contentHeight: 400, distanceFromBottom: 200)
        XCTAssertTrue(follow.isAtBottom)
    }

    func testUserScrollAtStableHeightPausesAndReturningResumes() {
        var follow = ModelMonitorFollow()
        follow.measure(contentHeight: 400, distanceFromBottom: 0)
        follow.measure(contentHeight: 400, distanceFromBottom: 150)
        XCTAssertFalse(follow.isAtBottom)
        follow.measure(contentHeight: 400, distanceFromBottom: 3)
        XCTAssertTrue(follow.isAtBottom)
    }

    func testAppendedRowDoesNotPauseFollowing() {
        var follow = ModelMonitorFollow()
        follow.measure(contentHeight: 400, distanceFromBottom: 0)
        follow.noteAppend()
        // Content grew by one row; the viewport is now above the new bottom, but this is growth, not a user scroll.
        follow.measure(contentHeight: 460, distanceFromBottom: 60)
        XCTAssertTrue(follow.isAtBottom)
        // The next stable measurement is an ordinary scroll check.
        follow.measure(contentHeight: 460, distanceFromBottom: 0)
        XCTAssertTrue(follow.isAtBottom)
    }

    func testUserScrollUpWhileContentGrowsIsDetectedAfterGrowthSettles() {
        var follow = ModelMonitorFollow()
        follow.measure(contentHeight: 400, distanceFromBottom: 0)
        follow.measure(contentHeight: 400, distanceFromBottom: 150)
        XCTAssertFalse(follow.isAtBottom)
        follow.noteAppend()
        follow.measure(contentHeight: 460, distanceFromBottom: 210)
        XCTAssertFalse(follow.isAtBottom)
    }

    func testJumpIgnoresAnimatedPositionsUntilBottomIsReached() {
        var follow = ModelMonitorFollow()
        follow.measure(contentHeight: 400, distanceFromBottom: 0)
        follow.measure(contentHeight: 400, distanceFromBottom: 150)
        XCTAssertFalse(follow.isAtBottom)
        follow.jump()
        // First measurement after the jump is the baseline; the animation then passes through mid-scroll positions at a stable height.
        follow.measure(contentHeight: 400, distanceFromBottom: 150)
        follow.measure(contentHeight: 400, distanceFromBottom: 90)
        XCTAssertTrue(follow.isAtBottom)
        follow.measure(contentHeight: 400, distanceFromBottom: 0)
        XCTAssertTrue(follow.isAtBottom)
        // Once the end is reached, a later user scroll pauses again.
        follow.measure(contentHeight: 400, distanceFromBottom: 150)
        XCTAssertFalse(follow.isAtBottom)
    }

    func testResetFollowsFromEndForTabSwitchAndEmptyTab() {
        var follow = ModelMonitorFollow()
        follow.measure(contentHeight: 400, distanceFromBottom: 0)
        follow.measure(contentHeight: 400, distanceFromBottom: 150)
        XCTAssertFalse(follow.isAtBottom)
        follow.reset()
        XCTAssertTrue(follow.isAtBottom)
        // After reset, the first measurement is a baseline again, so a stale position from the old tab cannot pause it.
        follow.measure(contentHeight: 120, distanceFromBottom: 90)
        XCTAssertTrue(follow.isAtBottom)
    }
}
