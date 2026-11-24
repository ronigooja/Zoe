import Foundation
import XCTest
@testable import ZoeKit

final class MCPChildProcessTests: XCTestCase {
    private func child(_ script: String, args: [String] = []) -> FoundationMCPChildProcess {
        FoundationMCPChildProcess(executable: URL(fileURLWithPath: "/usr/bin/python3"), arguments: ["-u", "-c", script] + args)
    }

    func testLineFramerHandlesSplitCoalescedAndOversizeLines() {
        var framer = MCPStdioLines()
        var lines: [Data] = []
        framer.receive(Data("sp".utf8)) { lines.append($0) }
        XCTAssertTrue(lines.isEmpty)
        framer.receive(Data("lit\none\ntwo\nne".utf8)) { lines.append($0) }
        framer.receive(Data("xt\n".utf8)) { lines.append($0) }
        XCTAssertEqual(lines.map { String(decoding: $0, as: UTF8.self) }, ["split", "one", "two", "next"])
        XCTAssertTrue(framer.receive(Data(repeating: 120, count: MCPStdioLines.maxBytes + 1)) { _ in XCTFail("oversize line emitted") })
        framer.receive(Data("discarded\nrecovered\n".utf8)) { lines.append($0) }
        XCTAssertEqual(lines.last, Data("recovered".utf8))
    }

    func testInitializeAndToolsListErrorsFromRealChild() async throws {
        let script = #"""
import json, sys
for line in sys.stdin:
    req = json.loads(line)
    if 'id' not in req: continue
    value = {'jsonrpc':'2.0', 'id':req['id']}
    if req['method'] == sys.argv[1]:
        value['error'] = {'code':-32603, 'message':'fixture failure'}
    else:
        value['result'] = {}
    print(json.dumps(value))
"""#
        for method in ["initialize", "tools/list"] {
            let process = child(script, args: [method])
            do { _ = try await process.start(); XCTFail("Expected \(method) failure") }
            catch let error as MCPChildError { XCTAssertEqual(error, .remoteError) }
            await process.terminate()
        }
    }

    func testRealChildReceivesCancellationNotificationAndLateResponseIsIgnored() async throws {
        let script = #"""
import json, sys
original = None
cancelled = None
for line in sys.stdin:
    req = json.loads(line)
    method = req['method']
    if method == 'initialize': result = {}
    elif method == 'tools/list': result = {'tools':[{'name':'tool'}]}
    elif method == 'notifications/cancelled':
        cancelled = req['params']['requestId']
        if original is not None:
            print(json.dumps({'jsonrpc':'2.0','id':original,'result':{'late':True}}))
        continue
    elif req.get('params', {}).get('name') == 'one':
        original = req['id']
        continue
    elif req.get('params', {}).get('name') == 'two': result = {'cancelled':cancelled is not None and cancelled == original, 'different': req['id'] != original}
    else: continue
    print(json.dumps({'jsonrpc':'2.0','id':req['id'],'result':result}))
"""#
        let process = child(script)
        _ = try await process.start()
        func call(_ name: String) -> JSONValue {
            .object(["jsonrpc": .string("2.0"), "id": .string("reused"), "method": .string("tools/call"), "params": .object(["name": .string(name)])])
        }
        let first = Task { try await process.call(call("one")) }
        try await mcpEventually { await process.hasPendingRequest(.string("reused")) }
        await process.cancel(id: .string("reused"))
        do { _ = try await first.value; XCTFail("expected cancellation") } catch is CancellationError {}
        let result = try await process.call(call("two"))
        XCTAssertEqual(result, .object(["cancelled": .bool(true), "different": .bool(true)]))
        await process.terminate()
    }

    func testRealStdioInitializationPaginationAndCall() async throws {
        let script = #"""
import json, sys
for line in sys.stdin:
    req = json.loads(line)
    method = req.get('method')
    if method == 'initialize':
        assert req['params']['protocolVersion'] == '2025-11-25'
        assert req['params']['clientInfo'] == {'name':'zoe-client','version':'1.0.0'}
        result = {}
    elif method == 'notifications/initialized':
        continue
    elif method == 'tools/list':
        result = {'tools':[{'name':'second'}]} if req['params'].get('cursor') else {'tools':[{'name':'first'}], 'nextCursor':'next'}
    elif method == 'tools/call':
        result = {'content':[{'type':'text','text':'x' * (3 * 1024 * 1024)}]}
    else:
        continue
    payload = json.dumps({'jsonrpc':'2.0','id':req['id'],'result':result}).encode() + b'\n'
    if method == 'tools/call':
        sys.stdout.buffer.write(b'not json\n{"jsonrpc":"2.0","id":"wrong","result":{}}\n' + payload[:19])
        sys.stdout.buffer.flush()
        payload = payload[19:]
    sys.stdout.buffer.write(payload)
    sys.stdout.buffer.flush()
"""#
        let child = child(script)
        let tools = try await child.start()
        XCTAssertEqual(tools, [.object(["name": .string("first")]), .object(["name": .string("second")])])
        let result = try await child.call(.object([
            "jsonrpc": .string("2.0"), "id": .string("test"), "method": .string("tools/call"),
            "params": .object(["name": .string("first")])
        ]))
        await child.terminate()
        guard case let .array(content)? = result[mcp: "content"], case let .string(text)? = content.first?[mcp: "text"] else {
            return XCTFail("missing large payload")
        }
        XCTAssertEqual(text.count, 3 * 1024 * 1024)
    }
}
