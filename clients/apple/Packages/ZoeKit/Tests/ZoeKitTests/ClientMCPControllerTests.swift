import Foundation
import XCTest
@testable import ZoeKit

private struct AllowedPermissions: MCPPermissionChecking {
    func missingPermission(for entry: ClientMCPEntry) -> MCPPermissionReason? { nil }
}
private struct DeniedPermissions: MCPPermissionChecking {
    let reason: MCPPermissionReason
    func missingPermission(for entry: ClientMCPEntry) -> MCPPermissionReason? { reason }
}
private final class RequestPermissions: MCPPermissionChecking, @unchecked Sendable {
    private let lock = NSLock()
    private var granted = false
    private var requests: [MCPPermissionReason] = []
    let reason: MCPPermissionReason
    init(_ reason: MCPPermissionReason) { self.reason = reason }
    func missingPermission(for entry: ClientMCPEntry) -> MCPPermissionReason? {
        lock.lock(); defer { lock.unlock() }
        return granted ? nil : reason
    }
    func requestPermission(_ reason: MCPPermissionReason) {
        lock.lock(); defer { lock.unlock() }
        requests.append(reason)
    }
    func grant() { lock.lock(); defer { lock.unlock() }; granted = true }
    var requested: [MCPPermissionReason] { lock.lock(); defer { lock.unlock() }; return requests }
}

private final class ChildHarness: @unchecked Sendable {
    private let lock = NSLock()
    private var callbacks: [String: @Sendable () -> Void] = [:]
    private var args: [String: [[String]]] = [:]
    private var children: [String: [FakeMCPChild]] = [:]
    func make(_ entry: ClientMCPEntry, exit: @escaping @Sendable () -> Void) -> FakeMCPChild {
        lock.lock(); defer { lock.unlock() }
        let child = FakeMCPChild(processID: String(children[entry.id, default: []].count))
        callbacks[entry.id] = exit
        args[entry.id, default: []].append(entry.effectiveArguments)
        children[entry.id, default: []].append(child)
        return child
    }
    func exit(_ id: String) { lock.lock(); let callback = callbacks[id]; lock.unlock(); callback?() }
    func arguments(_ id: String) -> [[String]] { lock.lock(); defer { lock.unlock() }; return args[id] ?? [] }
    func child(_ id: String, _ index: Int = 0) -> FakeMCPChild { lock.lock(); defer { lock.unlock() }; return children[id]![index] }
}

private actor RecordingTransport: ClientMCPTransport {
    var messages: [JSONValue] = []
    var needed = false
    func setNeeded(_ needed: Bool) async { self.needed = needed }
    func send(_ data: Data) async throws { messages.append(try JSONDecoder().decode(JSONValue.self, from: data)) }
}

private actor FakeMCPChild: MCPChildProcess {
    let processID: String
    let blocksStartup: Bool
    let returnsLate: Bool
    var starting: CheckedContinuation<[JSONValue], Error>?
    var calls: [JSONValue] = []
    var continuations: [CheckedContinuation<JSONValue, Error>] = []
    var terminated = false
    var cancelled: [JSONValue] = []
    init(processID: String = "0", blocksStartup: Bool = false, returnsLate: Bool = false) {
        self.processID = processID
        self.blocksStartup = blocksStartup
        self.returnsLate = returnsLate
    }
    func start() async throws -> [JSONValue] {
        if blocksStartup { return try await withCheckedThrowingContinuation { starting = $0 } }
        return [.object(["name": .string("tool"), "description": .string(processID)])]
    }
    func failStartup() { starting?.resume(throwing: MCPChildError.remoteError); starting = nil }
    func call(_ request: JSONValue) async throws -> JSONValue {
        calls.append(request)
        if let args = request[mcp: "params"]?[mcp: "arguments"],
           args[mcp: "ref"] != .string("ref-\(processID)") || args[mcp: "session"] != .string("session-\(processID)") {
            throw MCPChildError.remoteError
        }
        return try await withCheckedThrowingContinuation { continuations.append($0) }
    }
    func cancel(id: JSONValue) async {
        cancelled.append(id)
        if !returnsLate && !continuations.isEmpty { continuations.removeFirst().resume(throwing: CancellationError()) }
    }
    func terminate() async {
        terminated = true
        starting?.resume(throwing: MCPChildError.exited)
        starting = nil
        let pending = continuations
        continuations.removeAll()
        for continuation in pending { continuation.resume(throwing: MCPChildError.exited) }
    }
    func complete(result: JSONValue? = nil) {
        if !continuations.isEmpty {
            continuations.removeFirst().resume(returning: result ?? .object(["ref": .string("ref-\(processID)"), "session": .string("session-\(processID)")]))
        }
    }
    func completeLarge() { if !continuations.isEmpty { continuations.removeFirst().resume(returning: .object(["payload": .string(String(repeating: "x", count: ClientWebSocketLimits.frameBytes))])) } }
}

final class ClientMCPControllerTests: XCTestCase {
    private func call(_ id: String, mcp: String = "agent-desktop", arguments: JSONValue? = nil) -> Data {
        var params: [String: JSONValue] = ["name": .string("tool")]
        params["arguments"] = arguments
        return try! JSONEncoder().encode(JSONValue.object([
            "channel": .string("mcp"), "type": .string("call"), "request_id": .string(id), "mcp_id": .string(mcp),
            "jsonrpc": .object(["jsonrpc": .string("2.0"), "id": .string(id), "method": .string("tools/call"), "params": .object(params)])
        ]))
    }

    private func waitFor(_ predicate: @escaping () async -> Bool) async throws {
        try await mcpEventually(predicate)
    }

    func testSerialDuplicateAndDisconnectDropsPending() async throws {
        let child = FakeMCPChild(), transport = RecordingTransport()
        let registry = try ClientMCPRegistry(store: InMemoryMCPRegistryStore())
        let controller = ClientMCPController(registry: registry, permissions: AllowedPermissions(), transport: transport) { _, _ in child }
        try await controller.enable("agent-desktop")
        await controller.connectionOpened()
        await controller.receive(call("one"))
        await controller.receive(call("one"))
        await controller.receive(call("two"))
        try await waitFor { await child.calls.count == 1 }
        await child.complete()
        try await waitFor { await child.calls.count == 2 }
        await controller.receive(call("one"))
        try await waitFor { await transport.messages.filter { value in
            if case let .object(object) = value { return object["request_id"] == .string("one") }
            return false
        }.count == 2 }
        await controller.connectionClosed()
        let count = await transport.messages.count
        await controller.connectionOpened()
        let messages = await transport.messages
        XCTAssertEqual(messages.count, count + 1)
        await controller.disable("agent-desktop")
    }

    func testDisableFailsPending() async throws {
        let child = FakeMCPChild(), transport = RecordingTransport()
        let registry = try ClientMCPRegistry(store: InMemoryMCPRegistryStore())
        let controller = ClientMCPController(registry: registry, permissions: AllowedPermissions(), transport: transport) { _, _ in child }
        try await controller.enable("agent-desktop")
        await controller.connectionOpened()
        await controller.receive(call("one"))
        try await waitFor { await child.calls.count == 1 }
        await controller.disable("agent-desktop")
        let messages = await transport.messages
        XCTAssertTrue(messages.contains(.object(["channel": .string("mcp"), "type": .string("result"), "request_id": .string("one"), "ok": .bool(false), "error": .object(["code": .string("mcp_disabled")])])))
        let terminated = await child.terminated
        XCTAssertTrue(terminated)
    }

    func testCallJustBeforeIdleExpiryResetsTimerAndExpiryFailsPending() async throws {
        let child = FakeMCPChild(), transport = RecordingTransport(), clock = ManualMCPClock()
        let registry = try ClientMCPRegistry(store: InMemoryMCPRegistryStore())
        let controller = ClientMCPController(registry: registry, permissions: AllowedPermissions(), clock: clock, transport: transport) { _, _ in child }
        try await controller.enable("agent-desktop")
        await controller.connectionOpened()
        try await waitFor { await clock.sleepers == [1800] }
        await clock.advance(1799)
        await controller.receive(call("idle"))
        try await waitFor { await clock.requested.filter { $0 == 1800 }.count == 2 }
        await clock.advance(1)
        let status = await controller.statuses["agent-desktop"]
        XCTAssertEqual(status, .on)
        await clock.advance(1799)
        try await waitFor { await transport.messages.contains { $0[mcp: "request_id"] == .string("idle") && $0[mcp: "error"]?[mcp: "code"] == .string("mcp_disabled") } }
        let terminated = await child.terminated
        XCTAssertTrue(terminated)
    }

    func testUnexpectedExitDoesNotAffectOtherMCP() async throws {
        let harness = ChildHarness(), transport = RecordingTransport()
        let registry = try ClientMCPRegistry(store: InMemoryMCPRegistryStore(), defaults: [.agentDesktop, .init(id: "other", command: "other")])
        let controller = ClientMCPController(registry: registry, permissions: AllowedPermissions(), transport: transport) { harness.make($0, exit: $1) }
        try await controller.enable("agent-desktop")
        try await controller.enable("other")
        await controller.connectionOpened()
        await controller.receive(call("a"))
        await controller.receive(call("b", mcp: "other"))
        try await waitFor { let a = await harness.child("agent-desktop").calls.count; let b = await harness.child("other").calls.count; return a == 1 && b == 1 }
        harness.exit("agent-desktop")
        try await waitFor { await transport.messages.contains { $0[mcp: "request_id"] == .string("a") } }
        let statuses = await controller.statuses
        XCTAssertEqual(statuses["other"], .on)
        let messages = await transport.messages
        XCTAssertTrue(messages.contains(.object(["channel": .string("mcp"), "type": .string("result"), "request_id": .string("a"), "ok": .bool(false), "error": .object(["code": .string("mcp_exited")])])))
        await controller.disable("other")
    }

    func testRestartOptionFailsPendingAndPublishesNewCatalogAndHandles() async throws {
        let harness = ChildHarness(), transport = RecordingTransport()
        let registry = try ClientMCPRegistry(store: InMemoryMCPRegistryStore())
        let controller = ClientMCPController(registry: registry, permissions: AllowedPermissions(), transport: transport) { harness.make($0, exit: $1) }
        try await controller.enable("agent-desktop")
        await controller.connectionOpened()
        await controller.receive(call("before"))
        try await waitFor { await harness.child("agent-desktop").calls.count == 1 }
        await harness.child("agent-desktop").complete()
        try await waitFor { await transport.messages.contains { $0[mcp: "request_id"] == .string("before") } }
        await controller.receive(call("a"))
        try await waitFor { await harness.child("agent-desktop").calls.count == 2 }
        try await controller.setLaunchOption("headed", value: true, for: "agent-desktop")
        await controller.receive(call("after"))
        try await waitFor { await harness.child("agent-desktop", 1).calls.count == 1 }
        await harness.child("agent-desktop", 1).complete()
        try await waitFor { await transport.messages.contains { $0[mcp: "request_id"] == .string("after") } }
        XCTAssertEqual(harness.arguments("agent-desktop"), [["mcp"], ["mcp", "--headed"]])
        let messages = await transport.messages
        let catalogs = messages.compactMap { $0[mcp: "tools"]?[mcp: "agent-desktop"]?[mcp: "tools"] }
        XCTAssertTrue(catalogs.contains(.array([.object(["name": .string("tool"), "description": .string("0")])])))
        XCTAssertEqual(catalogs.last, .array([.object(["name": .string("tool"), "description": .string("1")])]))
        let before = messages.first { $0[mcp: "request_id"] == .string("before") }?[mcp: "result"]
        let after = messages.first { $0[mcp: "request_id"] == .string("after") }?[mcp: "result"]
        for key in ["ref", "session"] {
            XCTAssertNotNil(before?[mcp: key]); XCTAssertNotNil(after?[mcp: key]); XCTAssertNotEqual(before?[mcp: key], after?[mcp: key])
        }
        await controller.receive(call("stale", arguments: try XCTUnwrap(before)))
        try await waitFor { await transport.messages.contains { $0[mcp: "request_id"] == .string("stale") && $0[mcp: "ok"] == .bool(false) } }
        XCTAssertTrue(messages.contains(.object(["channel": .string("mcp"), "type": .string("result"), "request_id": .string("a"), "ok": .bool(false), "error": .object(["code": .string("mcp_restarted")])])))
        let terminated = await harness.child("agent-desktop").terminated
        XCTAssertTrue(terminated)
        await controller.disable("agent-desktop")
    }

    func testAccessibilityDenialPreventsSpawn() async throws { try await assertPermissionBlocksFactory(.accessibility) }
    func testScreenRecordingNotePreventsSpawn() async throws { try await assertPermissionBlocksFactory(.screenRecording) }

    func testPermissionRequestedOnceThenEnableAfterGrant() async throws {
        for reason in [MCPPermissionReason.accessibility, .screenRecording] {
            let permissions = RequestPermissions(reason), harness = ChildHarness()
            let controller = ClientMCPController(registry: try ClientMCPRegistry(store: InMemoryMCPRegistryStore()), permissions: permissions, transport: RecordingTransport()) { harness.make($0, exit: $1) }
            try await controller.enable("agent-desktop")
            let denied = await controller.statuses
            XCTAssertEqual(denied["agent-desktop"], .permissionNeeded(reason))
            XCTAssertEqual(permissions.requested, [reason])
            XCTAssertTrue(harness.arguments("agent-desktop").isEmpty)
            permissions.grant()
            try await controller.enable("agent-desktop")
            let granted = await controller.statuses
            XCTAssertEqual(granted["agent-desktop"], .on)
            XCTAssertEqual(permissions.requested, [reason])
            await controller.disable("agent-desktop")
        }
    }

    private func assertPermissionBlocksFactory(_ reason: MCPPermissionReason) async throws {
        let harness = ChildHarness(), transport = RecordingTransport()
        let registry = try ClientMCPRegistry(store: InMemoryMCPRegistryStore())
        let controller = ClientMCPController(registry: registry, permissions: DeniedPermissions(reason: reason), transport: transport) { harness.make($0, exit: $1) }
        try await controller.enable("agent-desktop")
        let statuses = await controller.statuses
        XCTAssertEqual(statuses["agent-desktop"], .permissionNeeded(reason))
        XCTAssertTrue(harness.arguments("agent-desktop").isEmpty)
    }

    func testMissingExecutableProducesErrorStatus() async throws {
        let transport = RecordingTransport()
        let registry = try ClientMCPRegistry(store: InMemoryMCPRegistryStore(), defaults: [.init(id: "missing", command: "zoe-missing-test-executable")])
        let controller = ClientMCPController(registry: registry, permissions: AllowedPermissions(), transport: transport, factory: ClientMCPController.processFactory())
        do { try await controller.enable("missing"); XCTFail("expected failure") } catch {}
        let statuses = await controller.statuses
        let entries = await controller.entries()
        XCTAssertEqual(statuses["missing"], .error(.exited))
        XCTAssertFalse(entries[0].enabled)
    }

    func testStartupErrorFailsQueuedCallAndTurnsOff() async throws {
        try await assertStartupFailure(timeout: false)
    }

    func testStartupTimeoutFailsQueuedCallAndTurnsOff() async throws {
        try await assertStartupFailure(timeout: true)
    }

    private func assertStartupFailure(timeout: Bool) async throws {
        let child = FakeMCPChild(blocksStartup: true), transport = RecordingTransport(), clock = ManualMCPClock()
        let controller = ClientMCPController(registry: try ClientMCPRegistry(store: InMemoryMCPRegistryStore()), permissions: AllowedPermissions(), clock: clock, transport: transport) { _, _ in child }
        let enabling = Task { try await controller.enable("agent-desktop") }
        try await waitFor { await child.starting != nil }
        await controller.connectionOpened()
        await controller.receive(call("pending"))
        if timeout {
            try await waitFor { await clock.sleepers.contains(ClientMCPController.startupTimeoutSeconds) }
            await clock.advance(ClientMCPController.startupTimeoutSeconds)
        } else { await child.failStartup() }
        do { try await enabling.value; XCTFail("Expected startup failure") }
        catch let error as MCPChildError { XCTAssertEqual(error, timeout ? .startupTimedOut : .remoteError) }
        let statuses = await controller.statuses
        let entries = await controller.entries()
        let messages = await transport.messages
        let terminated = await child.terminated
        let forwarded = await child.calls
        XCTAssertEqual(statuses["agent-desktop"], .error(timeout ? .startupTimedOut : .exited))
        XCTAssertFalse(entries[0].enabled)
        XCTAssertTrue(terminated)
        XCTAssertTrue(forwarded.isEmpty)
        XCTAssertTrue(messages.contains { $0[mcp: "request_id"] == .string("pending") && $0[mcp: "error"]?[mcp: "code"] == .string("mcp_exited") })
        try await waitFor { await clock.sleepers.isEmpty }
    }

    func testSlowMCPDoesNotBlockOtherMCPAndEachRunsInArrivalOrder() async throws {
        let harness = ChildHarness(), transport = RecordingTransport()
        let registry = try ClientMCPRegistry(store: InMemoryMCPRegistryStore(), defaults: [.agentDesktop, .init(id: "other", command: "other")])
        let controller = ClientMCPController(registry: registry, permissions: AllowedPermissions(), transport: transport) { harness.make($0, exit: $1) }
        try await controller.enable("agent-desktop")
        try await controller.enable("other")
        await controller.connectionOpened()
        for id in ["first", "second", "third"] { await controller.receive(call(id)) }
        await controller.receive(call("independent", mcp: "other"))
        let slow = harness.child("agent-desktop"), fast = harness.child("other")
        try await waitFor { await fast.calls.count == 1 }
        await fast.complete()
        try await waitFor { await transport.messages.contains { $0[mcp: "request_id"] == .string("independent") } }
        let initial = await slow.calls.map { $0[mcp: "id"] }
        XCTAssertEqual(initial, [.string("first")])
        await slow.complete()
        try await waitFor { await slow.calls.count == 2 }
        await slow.complete()
        try await waitFor { await slow.calls.count == 3 }
        let order = await slow.calls.map { $0[mcp: "id"] }
        XCTAssertEqual(order, [.string("first"), .string("second"), .string("third")])
        await controller.disable("agent-desktop")
        await controller.disable("other")
    }

    func testReconnectResendsFullCatalogAndDropsLateResultEvenWithReusedID() async throws {
        let active = FakeMCPChild(processID: "fresh", returnsLate: true), other = FakeMCPChild(), transport = RecordingTransport()
        let registry = try ClientMCPRegistry(store: InMemoryMCPRegistryStore(), defaults: [.agentDesktop, .init(id: "other", command: "other")])
        let controller = ClientMCPController(registry: registry, permissions: AllowedPermissions(), transport: transport) { entry, _ in entry.id == "other" ? other : active }
        try await controller.enable("agent-desktop")
        try await controller.enable("other")
        await controller.connectionOpened()
        await controller.receive(call("reused"))
        try await waitFor { await active.calls.count == 1 }
        await controller.connectionClosed()
        await controller.disable("other")
        await controller.connectionOpened()
        let catalog = await transport.messages.last
        XCTAssertEqual(catalog?[mcp: "enabled_mcp_ids"], .array([.string("agent-desktop")]))
        XCTAssertEqual(catalog?[mcp: "tools"], .object(["agent-desktop": .object(["tools": .array([.object(["name": .string("tool"), "description": .string("fresh")])])])]))
        await controller.receive(call("reused"))
        try await waitFor { await active.calls.count == 2 }
        await active.complete(result: .object(["sentinel": .string("old")]))
        await active.complete(result: .object(["sentinel": .string("new")]))
        try await waitFor { await transport.messages.contains { $0[mcp: "request_id"] == .string("reused") } }
        let results = await transport.messages.filter { $0[mcp: "request_id"] == .string("reused") }.map { $0[mcp: "result"]?[mcp: "sentinel"] }
        XCTAssertEqual(results, [.string("new")])
        await controller.disable("agent-desktop")
    }

    func testCancellationDropsResponseAndStartsNextCall() async throws {
        let child = FakeMCPChild(returnsLate: true), transport = RecordingTransport()
        let controller = ClientMCPController(registry: try ClientMCPRegistry(store: InMemoryMCPRegistryStore()), permissions: AllowedPermissions(), transport: transport) { _, _ in child }
        try await controller.enable("agent-desktop")
        await controller.connectionOpened()
        await controller.receive(call("one"))
        await controller.receive(call("two"))
        try await waitFor { await child.calls.count == 1 }
        await controller.receive(Data(#"{"channel":"mcp","type":"cancel","request_id":"one"}"#.utf8))
        try await waitFor { await child.calls.count == 2 }
        let cancelled = await child.cancelled
        XCTAssertEqual(cancelled, [.string("one")])
        await child.complete() // Deliberately deliver a result even after cancellation.
        await child.complete()
        try await waitFor { await transport.messages.contains { $0[mcp: "request_id"] == .string("two") } }
        let messages = await transport.messages
        XCTAssertFalse(messages.contains { value in if case let .object(object) = value { return object["request_id"] == .string("one") }; return false })
        await controller.disable("agent-desktop")
    }

    func testOversizeResultBecomesFrameTooLarge() async throws {
        let child = FakeMCPChild(), transport = RecordingTransport()
        let controller = ClientMCPController(registry: try ClientMCPRegistry(store: InMemoryMCPRegistryStore()), permissions: AllowedPermissions(), transport: transport) { _, _ in child }
        try await controller.enable("agent-desktop")
        await controller.connectionOpened()
        await controller.receive(call("big"))
        try await waitFor { await child.calls.count == 1 }
        await child.completeLarge()
        try await waitFor { await transport.messages.contains(.object(["channel": .string("mcp"), "type": .string("result"), "request_id": .string("big"), "ok": .bool(false), "error": .object(["code": .string("frame_too_large")])])) }
        await controller.disable("agent-desktop")
    }
}
