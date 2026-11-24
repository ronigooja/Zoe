import Foundation
import XCTest
@testable import ZoeKit

final class ClientMCPFrameTests: XCTestCase {
    func testStateContainsWholeCatalog() throws {
        let state = ClientMCPState(catalogs: [
            "second": [],
            "agent-desktop": [.object(["name": .string("click")])]
        ])
        let value = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(state))
        XCTAssertEqual(value, .object([
            "channel": .string("mcp"), "type": .string("state"),
            "enabled_mcp_ids": .array([.string("agent-desktop"), .string("second")]),
            "tools": .object([
                "agent-desktop": .object(["tools": .array([.object(["name": .string("click")])])]),
                "second": .object(["tools": .array([])])
            ])
        ]))
    }

    func testCallPreservesJSONRPC() throws {
        let data = Data(#"{"channel":"mcp","type":"call","request_id":"r","mcp_id":"agent-desktop","jsonrpc":{"jsonrpc":"2.0","id":"r","method":"tools/call","params":{"name":"click","arguments":{"x":3}}}}"#.utf8)
        let call = try JSONDecoder().decode(ClientMCPCall.self, from: data)
        XCTAssertEqual(call.requestID, "r")
        XCTAssertEqual(call.mcpID, "agent-desktop")
        XCTAssertEqual(call.jsonrpc, .object([
            "jsonrpc": .string("2.0"), "id": .string("r"), "method": .string("tools/call"),
            "params": .object(["name": .string("click"), "arguments": .object(["x": .integer(3)])])
        ]))
    }

    func testResultUsesServerKeys() throws {
        let result = ClientMCPResult(requestID: "r", ok: false, result: nil, error: .init(code: "mcp_disabled", message: nil))
        let value = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(result))
        XCTAssertEqual(value, .object([
            "channel": .string("mcp"), "type": .string("result"), "request_id": .string("r"),
            "ok": .bool(false), "error": .object(["code": .string("mcp_disabled")])
        ]))
    }
}
