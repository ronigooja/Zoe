import XCTest
@testable import ZoeKit

private struct RelayPermissions: MCPPermissionChecking {
    func missingPermission(for entry: ClientMCPEntry) -> MCPPermissionReason? { nil }
}

private actor RelayChild: MCPChildProcess {
    func start() async throws -> [JSONValue] { [.object(["name": .string("tool")])] }
    func call(_ request: JSONValue) async throws -> JSONValue { .object([:]) }
    func cancel(id: JSONValue) async {}
    func terminate() async {}
}

final class ClientMCPRelayTests: XCTestCase {
    func testConstructionObservableSwitchesAndOnlySettingsPersist() async throws {
        let second = ClientMCPEntry(id: "other", command: "other", enabled: true)
        var desktop = ClientMCPEntry.agentDesktop
        desktop.enabled = true
        let store = InMemoryMCPRegistryStore(entries: [desktop, second])
        let firstSocket = FixtureMCPSocket(), clocklessOther = FixtureMCPSocket()
        let factoryCalls = expectation(description: "only the enabled relay requests a socket")
        factoryCalls.assertForOverFulfill = true
        let relay = try await ClientMCPRelay.make(api: mcpTestAPI(), store: store, permissions: RelayPermissions(), childFactory: { _, _ in RelayChild() }, socketFactory: { _ in factoryCalls.fulfill(); return firstSocket })
        let reopened = expectation(description: "launching with saved settings must not open a socket")
        reopened.isInverted = true
        var statuses = await relay.statusUpdates().makeAsyncIterator()
        let initial = await statuses.next()
        XCTAssertEqual(initial, ["agent-desktop": .off, "other": .off])
        try await relay.setIdleTimeout(42, id: "agent-desktop")
        try await relay.setLaunchOption("headed", value: true, id: "agent-desktop")
        try await relay.setEnabled(true, id: "agent-desktop")
        let on = await statuses.next()
        XCTAssertEqual(on, ["agent-desktop": .on, "other": .off])
        await fulfillment(of: [factoryCalls], timeout: 1)
        let persisted = try XCTUnwrap(store.load())
        XCTAssertFalse(persisted.contains(where: \.enabled))
        let relaunched = try await ClientMCPRelay.make(api: mcpTestAPI(), store: store, permissions: RelayPermissions(), childFactory: { _, _ in XCTFail("must not launch a child on construction"); return RelayChild() }, socketFactory: { _ in reopened.fulfill(); return clocklessOther })
        let reloaded = try await XCTUnwrapAsyncEntry(relaunched)
        XCTAssertFalse(reloaded.enabled)
        XCTAssertEqual(reloaded.idleTimeoutSeconds, 42)
        XCTAssertEqual(reloaded.effectiveArguments, ["mcp", "--headed"])
        try await relay.setEnabled(false, id: "agent-desktop")
        let off = await statuses.next()
        XCTAssertEqual(off, ["agent-desktop": .off, "other": .off])
        try await mcpEventually { await firstSocket.closeCount > 0 }
        await fulfillment(of: [reopened], timeout: 0.02)
    }

    private func XCTUnwrapAsyncEntry(_ relay: ClientMCPRelay) async throws -> ClientMCPEntry {
        let entries = await relay.entries()
        return try XCTUnwrap(entries.first { $0.id == "agent-desktop" })
    }
}
