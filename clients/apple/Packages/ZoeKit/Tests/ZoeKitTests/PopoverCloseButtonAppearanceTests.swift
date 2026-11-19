#if os(macOS)
import XCTest
@testable import ZoeUI

final class PopoverCloseButtonAppearanceTests: XCTestCase {
    func testNormalStateHasNoBackgroundAndSecondaryIcon() {
        XCTAssertEqual(PopoverCloseButtonAppearance.backgroundOpacity(hovering: false, pressed: false), 0)
        XCTAssertFalse(PopoverCloseButtonAppearance.iconIsPrimary(hovering: false))
    }

    func testHoverShowsTenPercentBackgroundAndPrimaryIcon() {
        XCTAssertEqual(PopoverCloseButtonAppearance.backgroundOpacity(hovering: true, pressed: false), 0.10)
        XCTAssertTrue(PopoverCloseButtonAppearance.iconIsPrimary(hovering: true))
    }

    func testPressedShowsEighteenPercentBackground() {
        XCTAssertEqual(PopoverCloseButtonAppearance.backgroundOpacity(hovering: true, pressed: true), 0.18)
        XCTAssertEqual(PopoverCloseButtonAppearance.backgroundOpacity(hovering: false, pressed: true), 0.18)
    }

    func testGeometryMatchesSpecification() {
        XCTAssertEqual(PopoverCloseButtonAppearance.iconSize, 15)
        XCTAssertEqual(PopoverCloseButtonAppearance.circleDiameter, 22)
        XCTAssertEqual(PopoverCloseButtonAppearance.animationDuration, 0.12)
    }
}
#endif
