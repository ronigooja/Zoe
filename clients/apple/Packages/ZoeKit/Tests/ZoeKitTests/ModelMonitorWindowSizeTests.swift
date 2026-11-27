#if os(macOS)
import CoreGraphics
import XCTest
@testable import ZoeUI

final class ModelMonitorWindowSizeTests: XCTestCase {
    private typealias Sizing = ModelMonitorView.WindowSizing

    func testLargeScreenUsesPreferredFirstOpenSize() {
        let size = Sizing.firstOpenContentSize(visible: CGRect(x: 0, y: 0, width: 1920, height: 1080))
        XCTAssertEqual(size, CGSize(width: 900, height: 600))
    }

    func testSmallScreenFitsAfterAllowance() {
        // 800x600 minus the 40x80 allowance is 760x520, which is below the preferred size and above the minimum.
        let size = Sizing.firstOpenContentSize(visible: CGRect(x: 0, y: 0, width: 800, height: 600))
        XCTAssertEqual(size, CGSize(width: 760, height: 520))
    }

    func testFittingTakesPrecedenceOverMinimumOnTinyScreen() {
        // 200x200 minus the allowance is 160x120: smaller than the 320x360 minimum, and the window must still fit on screen.
        let visible = CGRect(x: 0, y: 0, width: 200, height: 200)
        XCTAssertEqual(Sizing.firstOpenContentSize(visible: visible), CGSize(width: 160, height: 120))
        XCTAssertEqual(Sizing.effectiveMinimumSize(visible: visible), CGSize(width: 160, height: 120))
    }

    func testEffectiveMinimumIsFixedWhenScreenIsLargeEnough() {
        let visible = CGRect(x: 0, y: 0, width: 1440, height: 900)
        XCTAssertEqual(Sizing.effectiveMinimumSize(visible: visible), Sizing.minimumContentSize)
    }

    func testVisibleOriginDoesNotChangeSize() {
        let size = Sizing.firstOpenContentSize(visible: CGRect(x: 1200, y: 40, width: 800, height: 600))
        XCTAssertEqual(size, CGSize(width: 760, height: 520))
    }

    func testWindowTitleCarriesLiveState() {
        XCTAssertTrue(ModelMonitorView.windowTitle(for: .live).hasSuffix("Live"))
    }
}
#endif
