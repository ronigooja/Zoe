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

    func testGenericRenderingKeepsEmbeddedJSONAsWrittenByDefault() {
        let text = "Context {\"a\":1} end"
        XCTAssertEqual(ModelMonitorPayloadText.render(.string(text)), text)
        XCTAssertEqual(ModelMonitorPayloadText.render(.string(text), embeddedJSON: false), text)
    }

    func testEmbeddedInlineObjectIsRenderedAsStructure() {
        let text = ModelMonitorPayloadText.render(.string("Call {\"name\":\"Zoe\"} done"), embeddedJSON: true)
        XCTAssertEqual(text, "Call name: Zoe done")
    }

    func testEmbeddedMultilineArrayOfObjectsIsRendered() {
        let text = ModelMonitorPayloadText.render(.string("Items:\n[\n{\"id\":1},\n{\"id\":2}\n]"), embeddedJSON: true)
        XCTAssertEqual(text, "Items:\n- id: 1\n- id: 2")
    }

    func testEscapedQuotesAndNestedBracesInsideStringsDoNotBreakPairing() {
        let text = ModelMonitorPayloadText.render(.string(#"x {"a":"} { \" ]","b":{"c":[{"d":2}]}} y"#), embeddedJSON: true)
        XCTAssertEqual(text, "x a: } { \" ]\nb:\n  c:\n    - d: 2 y")
    }

    func testMalformedAndOrdinaryBracketProseStaysAsWritten() {
        let prose = "see [note] and {draft: no quotes} and [1, 2] plus {\"open\":"
        XCTAssertEqual(ModelMonitorPayloadText.render(.string(prose), embeddedJSON: true), prose)
    }

    func testScalarArrayAndEmptyCandidatesStayAsWritten() {
        let prose = "count [1, 2, 3] and {} and []"
        XCTAssertEqual(ModelMonitorPayloadText.render(.string(prose), embeddedJSON: true), prose)
    }

    func testNonEmptyObjectCandidateInProseIsRendered() {
        XCTAssertEqual(ModelMonitorPayloadText.render(.string("and {\"n\":1}"), embeddedJSON: true), "and n: 1")
    }

    func testEmbeddedRenderingDoesNotChangeMessageLiteralText() {
        XCTAssertEqual(ModelMonitorPayloadText.renderText("{\"a\":1}", embeddedJSON: false), "{\"a\":1}")
        XCTAssertEqual(ModelMonitorPayloadText.renderText("{\"a\":1}", embeddedJSON: true), "a: 1")
    }

    func testOversizedEmbeddedCandidateStaysAsWritten() {
        let raw = "pre {\"v\":\"" + String(repeating: "x", count: 70_000) + "\"} post"
        XCTAssertEqual(ModelMonitorPayloadText.render(.string(raw), embeddedJSON: true), raw)
    }

    func testOnlyOpenCodeContextRendersEmbeddedJSON() {
        let context = ModelMonitorEvent(component: "opencode_context", callID: "c", time: nil, kind: .request,
                                        payload: .string("a {\"b\":1}"), truncated: false)
        let worker = ModelMonitorEvent(component: "worker", callID: "c", time: nil, kind: .request,
                                       payload: .string("a {\"b\":1}"), truncated: false)
        XCTAssertTrue(context.rendersEmbeddedJSON)
        XCTAssertFalse(worker.rendersEmbeddedJSON)
    }

    func testCopyStillUsesCompleteFormattedJSON() {
        let event = ModelMonitorEvent(component: "worker", callID: "call-1", time: nil, kind: .request,
                                      payload: .object(["text": .string("a\"b")]), truncated: false)
        XCTAssertEqual(event.formattedPayload?.contains("a\\\"b"), true)
    }
}
