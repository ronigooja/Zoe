#if os(macOS)
import Combine
import XCTest
import ZoeKit
@testable import ZoeUI

actor FakeRemoteControlRelay: RemoteControlRelaying {
    private var settings: [ClientMCPEntry]
    private var state: [String: ClientMCPStatus] = [:]
    private let updates = AsyncStream.makeStream(of: [String: ClientMCPStatus].self, bufferingPolicy: .bufferingNewest(1))
    private var nextFailure: MCPChildError?
    private(set) var credentialChanges = 0
    private(set) var shutdowns = 0

    init(entries: [ClientMCPEntry] = [.agentDesktop]) { settings = entries }

    func entries() -> [ClientMCPEntry] { settings }
    func statuses() -> [String: ClientMCPStatus] { state }
    func statusUpdates() -> AsyncStream<[String: ClientMCPStatus]> {
        updates.continuation.yield(state)
        return updates.stream
    }

    func emit(_ status: ClientMCPStatus, id: String) {
        state[id] = status
        updates.continuation.yield(state)
    }

    func failNextAction(_ error: MCPChildError) { nextFailure = error }

    func setEnabled(_ enabled: Bool, id: String) throws {
        if let error = nextFailure {
            nextFailure = nil
            emit(.error(.exited), id: id)
            throw error
        }
        emit(enabled ? .on : .off, id: id)
    }

    func setIdleTimeout(_ seconds: TimeInterval, id: String) {
        if let index = settings.firstIndex(where: { $0.id == id }) { settings[index].idleTimeoutSeconds = seconds }
    }

    func setLaunchOption(_ key: String, value: Bool, id: String) {
        if let index = settings.firstIndex(where: { $0.id == id }) { settings[index].launchOptions[key] = value }
    }

    func credentialsChanged() { credentialChanges += 1 }

    func shutdown() {
        shutdowns += 1
        for entry in settings { emit(.off, id: entry.id) }
        updates.continuation.finish()
    }
}

@MainActor
final class RemoteControlModelTests: XCTestCase {
    private let id = ClientMCPEntry.agentDesktop.id

    func testRowsAndSettingsRefreshWithoutStatusChange() async throws {
        let relay = FakeRemoteControlRelay(entries: [.agentDesktop, .init(id: "other", command: "other")])
        let model = RemoteControlModel(relay: relay)
        let loaded = expectation(description: "the stream's initial event loads both rows")
        let subscription = model.$rows.first(where: { $0.count == 2 }).sink { _ in loaded.fulfill() }
        await fulfillment(of: [loaded], timeout: 2)
        withExtendedLifetime(subscription) {}
        XCTAssertEqual(model.rows.map(\.id), [id, "other"])
        let desktop = try XCTUnwrap(model.rows.first)
        XCTAssertEqual(desktop.title, id)
        XCTAssertEqual(desktop.status, .off)
        XCTAssertEqual(desktop.idleTimeoutSeconds, 1800)
        XCTAssertEqual(desktop.launchOptions, ["headed": false])
        XCTAssertFalse(model.anyEnabled)
        XCTAssertEqual(model.rows[1].launchOptions, [:])
        await model.setIdleTimeout(60, id: id)
        await model.setLaunchOption("headed", value: true, id: id)
        XCTAssertEqual(model.rows.first?.idleTimeoutSeconds, 60)
        XCTAssertEqual(model.rows.first?.headed, true)
    }

    func testStreamPropagatesStartingPermissionAndSwitchState() async {
        let relay = FakeRemoteControlRelay()
        let model = RemoteControlModel(relay: relay)
        let starts = expectation(description: "starting counts as enabled")
        let starting = model.$anyEnabled.first(where: { $0 }).sink { _ in starts.fulfill() }
        await relay.emit(.starting, id: id)
        await fulfillment(of: [starts], timeout: 2)
        withExtendedLifetime(starting) {}
        XCTAssertEqual(model.rows.first?.status, .starting)
        XCTAssertEqual(model.rows.first?.enabled, true)

        let stops = expectation(description: "missing permission leaves the switch off")
        let stopped = model.$anyEnabled.first(where: { !$0 }).sink { _ in stops.fulfill() }
        await relay.emit(.permissionNeeded(.screenRecording), id: id)
        await fulfillment(of: [stops], timeout: 2)
        withExtendedLifetime(stopped) {}
        XCTAssertEqual(model.rows.first?.permissionNeeded, .screenRecording)
        XCTAssertEqual(model.rows.first?.enabled, false)
        XCTAssertNil(model.rows.first?.lastError)
    }

    func testActionErrorIsLocalizedAndClearsOnRetry() async {
        let relay = FakeRemoteControlRelay()
        let model = RemoteControlModel(relay: relay)
        await relay.failNextAction(.executableNotFound)
        await model.setEnabled(true, id: id)
        XCTAssertEqual(model.rows.first?.lastError, "agent-desktop not found. Install it and try again.")
        XCTAssertEqual(model.rows.first?.status, .error(.exited))
        XCTAssertFalse(model.anyEnabled)
        await model.setEnabled(true, id: id)
        XCTAssertNil(model.rows.first?.lastError)
        XCTAssertTrue(model.anyEnabled)
        await model.setEnabled(false, id: id)
        XCTAssertFalse(model.anyEnabled)
    }
}
#endif
