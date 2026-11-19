import XCTest
@testable import ZoeUI

final class SavedKeyMaskTests: XCTestCase {
    func testNoSavedKeyShowsInstanceKeyTitle() {
        XCTAssertEqual(SavedKeyMask.text(for: nil), "Instance key")
    }

    func testEmptyKeyShowsOnlyBullets() {
        XCTAssertEqual(SavedKeyMask.text(for: ""), "••••")
    }

    func testShortKeyShowsOnlyBullets() {
        XCTAssertEqual(SavedKeyMask.text(for: "abcdefghijklmno"), "••••")
    }

    func testExactlySixteenCharactersShowsLastFour() {
        XCTAssertEqual(SavedKeyMask.text(for: "abcdefghijklmnop"), "••••mnop")
    }

    func testLongKeyShowsOnlyLastFourCharacters() {
        let key = String(repeating: "x", count: 40) + "WXYZ"
        XCTAssertEqual(SavedKeyMask.text(for: key), "••••WXYZ")
    }

    func testNonASCIIKeyUsesCharacterSuffix() {
        XCTAssertEqual(SavedKeyMask.text(for: String(repeating: "密", count: 12) + "ab道🌍"), "••••ab道🌍")
        XCTAssertEqual(SavedKeyMask.text(for: String(repeating: "🌍", count: 15)), "••••")
        XCTAssertEqual(SavedKeyMask.text(for: String(repeating: "🌍", count: 16)), "••••🌍🌍🌍🌍")
    }
}
