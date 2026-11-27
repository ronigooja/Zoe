#if os(macOS)
import CoreGraphics
import XCTest
@testable import ZoeUI

final class ModelMonitorWindowSizeTests: XCTestCase {
    func testLargeScreenUsesPreferredFirstOpenSize() {
        let size = ModelMonitorView.firstOpenContentSize(visible: CGRect(x: 0, y: 0, width: 1920, height: 1080))
        XCTAssertEqual(size, CGSize(width: 900, height: 600))
    }

    func testSmallScreenIsClampedToVisibleAreaWithMargin() {
        let size = ModelMonitorView.firstOpenContentSize(visible: CGRect(x: 0, y: 0, width: 800, height: 600))
        XCTAssertEqual(size, CGSize(width: 760, height: 520))
    }

    func testTinyScreenNeverGoesBelowMinimum() {
        let size = ModelMonitorView.firstOpenContentSize(visible: CGRect(x: 0, y: 0, width: 200, height: 200))
        XCTAssertEqual(size, ModelMonitorView.minimumContentSize)
    }

    func testWindowTitleCarriesLiveState() {
        XCTAssertTrue(ModelMonitorView.windowTitle(for: .live).hasSuffix("Live"))
    }
}
#endif
