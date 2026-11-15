import XCTest
@testable import ZoeKit

final class SSELineSplitterTests: XCTestCase {
    func testLineTerminatorsAndPartialFeeds() {
        let cases: [(name: String, feeds: [[UInt8]], expected: [String])] = [
            ("LF", [Array("first\nsecond\n".utf8)], ["first", "second"]),
            ("CRLF", [Array("first\r\nsecond\r\n".utf8)], ["first", "second"]),
            ("CR", [Array("first\rsecond\r".utf8)], ["first", "second"]),
            ("split CRLF", [Array("first\r".utf8), Array("\n\r".utf8), Array("\nsecond\r".utf8)], ["first", "", "second"]),
            ("empty LF lines", [Array("\n\n".utf8)], ["", ""]),
            ("empty CRLF lines", [Array("\r\n\r\n".utf8)], ["", ""]),
            ("empty CR lines", [Array("\r\r".utf8)], ["", ""]),
            ("mixed terminators", [Array("first\r\nsecond\nthird\r\n\r".utf8)], ["first", "second", "third", ""]),
            ("split UTF-8", [[0xE4], [0xBD, 0xA0, 0xF0, 0x9F], [0x8C, 0x8D, 0x0A]], ["你🌍"]),
            ("invalid UTF-8", [[0xFF, 0x0A]], ["�"]),
            ("unterminated tail", [Array("complete\npartial".utf8)], ["complete"]),
            ("only unterminated tail", [Array("partial".utf8)], []),
            ("empty stream", [[]], [])
        ]
        for testCase in cases {
            var splitter = SSELineSplitter()
            var lines: [String] = []
            for feed in testCase.feeds {
                for byte in feed {
                    if let line = splitter.consume(byte: byte) { lines.append(line) }
                }
            }
            XCTAssertEqual(lines, testCase.expected, testCase.name)
        }
    }

    func testByteStreamDispatchesTwoEventsAndIgnoresHeartbeat() throws {
        let first = #"{"schema_version":1,"event_id":"ev-1","cursor":"1","type":"task.created","occurred_at":"2026-10-20T12:00:00Z","workspace_id":"workspace","aggregate_type":"task","aggregate_id":"task-1","aggregate_version":1,"payload":{"title":"你🌍"}}"#
        let second = #"{"schema_version":1,"event_id":"ev-2","cursor":"2","type":"task.updated","occurred_at":"2026-10-20T12:00:00Z","workspace_id":"workspace","aggregate_type":"task","aggregate_id":"task-1","aggregate_version":2,"payload":{"status":"active"}}"#
        for terminator in ["\n", "\r\n", "\r"] {
            let stream = ["id: 1", "event: task.created", "data: \(first)", "", ": heartbeat", "", "id: 2", "event: task.updated", "data: \(second)", "", ""].joined(separator: terminator)
            var splitter = SSELineSplitter()
            var parser = SSEParser()
            var events: [EventEnvelope] = []
            for byte in stream.utf8 {
                guard let line = splitter.consume(byte: byte),
                      let push = try parser.consume(line: line) else { continue }
                guard case let .event(event) = push else {
                    return XCTFail("Expected an event, not snapshot repair")
                }
                events.append(event)
            }
            XCTAssertNil(try parser.finish())
            XCTAssertEqual(events.map(\.cursor), ["1", "2"])
            XCTAssertEqual(events.map(\.type), ["task.created", "task.updated"])
        }
    }

    func testByteStreamDropsUnterminatedRecordAtEOF() throws {
        let payload = #"{"schema_version":1,"event_id":"ev-1","cursor":"1","type":"task.created","occurred_at":"2026-10-20T12:00:00Z","workspace_id":"workspace","aggregate_type":"task","aggregate_id":"task-1","aggregate_version":1,"payload":{}}"#
        for ending in ["", "\n"] {
            var splitter = SSELineSplitter()
            var parser = SSEParser()
            for byte in "id: 1\nevent: task.created\ndata: \(payload)\(ending)".utf8 {
                if let line = splitter.consume(byte: byte) {
                    XCTAssertNil(try parser.consume(line: line))
                }
            }
            XCTAssertNil(try parser.finish())
        }
    }
}
