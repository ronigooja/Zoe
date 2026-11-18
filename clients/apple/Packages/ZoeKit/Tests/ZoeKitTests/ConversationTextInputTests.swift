import XCTest
import ZoeKit
@testable import ZoeUI

@MainActor
final class ConversationTextInputTests: XCTestCase {
    func testValidationUsesUTF8Bytes() {
        XCTAssertFalse(ConversationTextDraft.isValid(" \n\t"))
        XCTAssertTrue(ConversationTextDraft.isValid(String(repeating: "é", count: 10_000)))
        XCTAssertFalse(ConversationTextDraft.isValid(String(repeating: "é", count: 10_000) + "a"))
    }

    func testRetryAndAcceptance() async {
        let draft = ConversationTextDraft()
        draft.text = "hello"
        var keys: [String] = []
        await draft.send { _, key in
            keys.append(key)
            throw URLError(.timedOut)
        }
        XCTAssertEqual(draft.text, "hello")
        XCTAssertEqual(draft.status, "Not confirmed yet. It will be retried.")
        await draft.send { _, key in keys.append(key) }
        XCTAssertEqual(keys[0], keys[1])
        XCTAssertEqual(draft.text, "")
        XCTAssertEqual(draft.status, "Accepted. It is delivered to the conversation when a call is active.")
        draft.text = "hello"
        await draft.send { _, key in keys.append(key); throw URLError(.timedOut) }
        XCTAssertNotEqual(keys[1], keys[2])
        draft.text = "edited"
        await draft.send { _, key in keys.append(key); throw URLError(.timedOut) }
        XCTAssertNotEqual(keys[2], keys[3])
    }

    func testDefinitiveRejectionKeepsDraft() async {
        let draft = ConversationTextDraft()
        draft.text = "hello"
        await draft.send { _, _ in throw ZoeError.http(status: 403, code: "denied", message: "denied", details: nil) }
        XCTAssertEqual(draft.text, "hello")
        XCTAssertEqual(draft.status, "Server rejected the text (403).")
    }
}
