import XCTest
@testable import ZoeUI

final class SavedKeyMaskTests: XCTestCase {
    func testNoSavedKeyShowsNone() {
        XCTAssertEqual(SavedKeyMask.text(for: nil), "Saved key: none")
    }

    func testEmptyKeyShowsOnlyBullets() {
        XCTAssertEqual(SavedKeyMask.text(for: ""), "Saved key: ••••")
    }

    func testShortKeyShowsOnlyBullets() {
        XCTAssertEqual(SavedKeyMask.text(for: "abcdefghijklmno"), "Saved key: ••••")
    }

    func testExactlySixteenCharactersShowsLastFour() {
        XCTAssertEqual(SavedKeyMask.text(for: "abcdefghijklmnop"), "Saved key: ••••mnop")
    }

    func testLongKeyShowsOnlyLastFourCharacters() {
        let key = String(repeating: "x", count: 40) + "WXYZ"
        XCTAssertEqual(SavedKeyMask.text(for: key), "Saved key: ••••WXYZ")
    }

    func testNonASCIIKeyUsesCharacterSuffix() {
        XCTAssertEqual(SavedKeyMask.text(for: String(repeating: "密", count: 12) + "ab道🌍"), "Saved key: ••••ab道🌍")
        XCTAssertEqual(SavedKeyMask.text(for: String(repeating: "🌍", count: 15)), "Saved key: ••••")
        XCTAssertEqual(SavedKeyMask.text(for: String(repeating: "🌍", count: 16)), "Saved key: ••••🌍🌍🌍🌍")
    }
}
