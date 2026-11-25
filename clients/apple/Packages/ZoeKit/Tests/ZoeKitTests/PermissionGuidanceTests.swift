#if os(macOS)
import Foundation
import XCTest
import ZoeKit
@testable import ZoeUI

final class PermissionGuidanceTests: XCTestCase {
    func testVersionBoundary() {
        let old = OperatingSystemVersion(majorVersion: 26, minorVersion: 9, patchVersion: 0)
        let new = OperatingSystemVersion(majorVersion: 27, minorVersion: 0, patchVersion: 0)
        XCTAssertTrue(PermissionGuidance.text(for: .accessibility, operatingSystemVersion: old).contains("Privacy & Security > Accessibility"))
        XCTAssertTrue(PermissionGuidance.text(for: .accessibility, operatingSystemVersion: new).contains("Device Control and Data Access"))
        XCTAssertTrue(PermissionGuidance.text(for: .screenRecording, operatingSystemVersion: new).contains("name may differ"))
    }

    func testSettingsURLs() {
        XCTAssertEqual(PermissionGuidance.settingsURL(for: .accessibility).absoluteString, "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility")
        XCTAssertEqual(PermissionGuidance.settingsURL(for: .screenRecording).absoluteString, "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture")
        XCTAssertEqual(PermissionGuidance.settingsRootURL.absoluteString, "x-apple.systempreferences:com.apple.preference.security")
    }
}
#endif
