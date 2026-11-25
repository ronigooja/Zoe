#if os(macOS)
import XCTest
@testable import ZoeUI

final class RemoteControlIdleTimeoutTests: XCTestCase {
    func testClampsMinutesToSupportedRange() {
        XCTAssertEqual(RemoteControlIdleTimeout.clampedMinutes(0), 1)
        XCTAssertEqual(RemoteControlIdleTimeout.clampedMinutes(-5), 1)
        XCTAssertEqual(RemoteControlIdleTimeout.clampedMinutes(30), 30)
        XCTAssertEqual(RemoteControlIdleTimeout.clampedMinutes(1440), 1440)
        XCTAssertEqual(RemoteControlIdleTimeout.clampedMinutes(1441), 1440)
    }

    func testConvertsMinutesToSeconds() {
        XCTAssertEqual(RemoteControlIdleTimeout.seconds(fromMinutes: 30), 1800)
        XCTAssertEqual(RemoteControlIdleTimeout.seconds(fromMinutes: 0), 60)
        XCTAssertEqual(RemoteControlIdleTimeout.seconds(fromMinutes: 5000), 86400)
    }

    func testConvertsSecondsToWholeMinutes() {
        XCTAssertEqual(RemoteControlIdleTimeout.minutes(fromSeconds: 1800), 30)
        XCTAssertEqual(RemoteControlIdleTimeout.minutes(fromSeconds: 90), 2)
        XCTAssertEqual(RemoteControlIdleTimeout.minutes(fromSeconds: 0), 1)
        XCTAssertEqual(RemoteControlIdleTimeout.minutes(fromSeconds: .infinity), 1)
        XCTAssertEqual(RemoteControlIdleTimeout.minutes(fromSeconds: 1_000_000), 1440)
    }
}
#endif
