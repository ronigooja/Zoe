import XCTest
@testable import ZoeKit

final class ModelMonitorPayloadTextTests: XCTestCase {
    func testEscapedQuoteAndNewlineAreShownAsCharacters() {
        // The decoded string holds a real quote and a real newline; both are shown as written, never as escape sequences.
        let text = ModelMonitorPayloadText.render(.object(["text": .string("say \"hi\"\nnext")]))
        XCTAssertEqual(text, "text:\n  say \"hi\"\n  next")
    }

    func testLiteralBackslashesAreNotUnescaped() {
        // Backslash-n in the decoded text is two characters and must stay two characters.
        let text = ModelMonitorPayloadText.render(.string("path C:\\new\\table"))
        XCTAssertEqual(text, "path C:\\new\\table")
    }

    func testStringHoldingJSONObjectIsRenderedAsStructure() {
        let nested = JSONValue.string("{\"name\":\"Zoe\",\"count\":2}")
        let text = ModelMonitorPayloadText.render(.object(["arguments": nested]))
        XCTAssertEqual(text, "arguments:\n  count: 2\n  name: Zoe")
    }

    func testNonJSONTextStaysAsWritten() {
        XCTAssertEqual(ModelMonitorPayloadText.render(.string("[not json")), "[not json")
    }

    func testNestedEncodedJSONDecodesAtMostTwoLevels() {
        // Level 0 and level 1 strings decode; the level 2 string stays as written, with its quotes intact.
        let value = JSONValue.string(#"{"a":"{\"b\":\"{\\\"c\\\":1}\"}"}"#)
        XCTAssertEqual(ModelMonitorPayloadText.render(value), "a:\n  b: {\"c\":1}")
    }

    func testOversizedNestedStringIsNotDecoded() {
        let raw = "{\"v\":\"" + String(repeating: "x", count: ModelMonitorPayloadText.maxNestedBytes) + "\"}"
        XCTAssertEqual(ModelMonitorPayloadText.render(.string(raw)), raw)
    }

    func testArraysAndEmptyContainersRender() {
        let value = JSONValue.object(["items": .array([.integer(1), .bool(true), .null]), "empty": .object([:])])
        XCTAssertEqual(ModelMonitorPayloadText.render(value), "empty: {}\nitems:\n  - 1\n  - true\n  - null")
    }

    func testCopyStillUsesCompleteFormattedJSON() {
        let event = ModelMonitorEvent(component: "worker", callID: "call-1", time: nil, kind: .request,
                                      payload: .object(["text": .string("a\"b")]), truncated: false)
        XCTAssertEqual(event.formattedPayload?.contains("a\\\"b"), true)
    }
}
