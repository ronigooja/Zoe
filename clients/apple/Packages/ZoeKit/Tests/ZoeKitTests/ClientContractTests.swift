import XCTest
@testable import ZoeKit
@testable import ZoeUI
import Foundation
import CryptoKit
import WebRTC
#if os(macOS)
import AppKit
#endif

private final class ContractURLProtocol: URLProtocol {
    static var responseHandler: ((URLRequest) -> (Int, [String: String], Data))?
    static var failureHandler: ((URLRequest) -> Error?)?
    static var capturedRequest: URLRequest?
    static var responseGate: ((URLRequest) async -> Void)?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        Task {
            await Self.responseGate?(request)
            finishLoading()
        }
    }
    private func finishLoading() {
        Self.capturedRequest = request
        if let error = Self.failureHandler?(request) {
            client?.urlProtocol(self, didFailWithError: error)
            return
        }
        let (status, headers, body) = Self.responseHandler?(request) ?? (500, [:], Data())
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: headers)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

private struct StaticCredential: BearerTokenProviding {
    func bearerToken() throws -> String { UUID().uuidString }
}

private actor AsyncTestGate {
    private var continuation: CheckedContinuation<Void, Never>?
    private var startWaiter: CheckedContinuation<Void, Never>?
    private var hasStarted = false

    func pause() async {
        await withCheckedContinuation { continuation in
            self.continuation = continuation
            hasStarted = true
            startWaiter?.resume()
            startWaiter = nil
        }
    }

    func waitUntilStarted() async {
        guard !hasStarted else { return }
        await withCheckedContinuation { startWaiter = $0 }
    }

    func resume() {
        continuation?.resume()
        continuation = nil
    }
}

final class ClientContractTests: XCTestCase {
    private static func recoveryBody(cursor: String) -> Data {
        Data("""
        {"cursor":"\(cursor)","snapshot":{"workspace_id":"default","subject_id":"owner","server_version":"1","event_cursor":"\(cursor)","authority":{"instance_lineage_id":"lineage","generation":1,"deletion_sequence":"0"},"projects":[],"tasks":[{"id":"authoritative-task","status":"complete"}],"approvals":[],"runs":[],"execution_capacity":{}},"ui":{"cursor":\(cursor),"surfaces":[]},"voice":{"current_session":null,"pool":{"blocked":false,"reason_code":""}}}
        """.utf8)
    }

    private func session() -> URLSession {
        ContractURLProtocol.failureHandler = nil
        ContractURLProtocol.responseGate = nil
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [ContractURLProtocol.self]
        return URLSession(configuration: configuration)
    }

    func testConnectionGenerationRejectsResponsesAfterOriginOrCredentialSwitch() throws {
        let serverA = try XCTUnwrap(URL(string: "https://server-a.example"))
        let serverB = try XCTUnwrap(URL(string: "https://server-b.example"))
        var generation = ConnectionGeneration(origin: serverA)
        let originalRequest = generation.value

        XCTAssertTrue(generation.accepts(originalRequest, from: serverA))
        generation.switchOrigin(to: serverB)
        XCTAssertFalse(generation.accepts(originalRequest, from: serverA))
        XCTAssertFalse(generation.accepts(originalRequest, from: serverB))

        let reauthenticatedRequest = generation.value
        generation.advance() // Same server, new device identity.
        XCTAssertFalse(generation.accepts(reauthenticatedRequest, from: serverB))
    }

    func testUISnapshotUsesAuthenticatedEndpointAndExactInt64Cursor() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        ContractURLProtocol.responseHandler = { request in
            XCTAssertEqual(request.url?.path, "/v1/ui/snapshot")
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertNotNil(request.value(forHTTPHeaderField: "Authorization"))
            return (200, ["Content-Type": "application/json"], Data(#"{"cursor":9007199254740993,"surfaces":[]}"#.utf8))
        }
        let snapshot = try await api.uiSnapshot()
        XCTAssertEqual(snapshot.cursor, 9_007_199_254_740_993)
        XCTAssertTrue(snapshot.surfaces.isEmpty)
    }

    @MainActor func testReconnectTransportReplaysNonUIEventsDespiteNewerUISnapshotAndFetchFailure() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "1"))
        model.setAuthenticationForTesting(.connected)
        _ = model.installVoiceSessionForTesting(VoiceSessionResponse(voiceSessionId: "replayed-call", generation: 1, state: "active", answerSdp: ""), api: api)
        var mediaStops = 0
        var hangups = 0
        model.voiceLocalMediaStopObserverForTesting = { mediaStops += 1 }
        model.voiceHangupOverrideForTesting = { _ in
            hangups += 1
            return VoiceOperationResponse(ok: true, state: "ended", upstreamTermination: "verified", poolBlocked: false)
        }
        func envelope(_ cursor: Int64, _ type: String, payload: String = "{}", version: Int64 = 1) -> String {
            "{\"schema_version\":1,\"event_id\":\"e\(cursor)\",\"cursor\":\"\(cursor)\",\"type\":\"\(type)\",\"occurred_at\":\"\",\"workspace_id\":\"default\",\"aggregate_type\":\"ui_surface\",\"aggregate_id\":\"s1\",\"aggregate_version\":\(version),\"payload\":\(payload)}"
        }
        func frame(_ cursor: Int64, _ type: String, payload: String = "{}", version: Int64 = 1) -> String {
            "id: \(cursor)\nevent: \(type)\ndata: \(envelope(cursor, type, payload: payload, version: version))\n\n"
        }
        let surface = #"{"request_id":"r1","surface_id":"s1","surface_version":2,"container":"window","awaiting_input":true,"task_id":"task","question_id":"question","messages":[{"version":"v0.9","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}]}"#
        let snapshot = "{\"cursor\":12,\"surfaces\":[\(surface.dropLast()),\"render_status\":\"rendered\"}]}"
        var renderCount = 0
        var consumed: [String] = []
        model.onUISurface = { _ in renderCount += 1; return true }
        model.consumedEventObserverForTesting = { consumed.append($0) }
        var attempt = 0
        var streamCursors: [String?] = []
        ContractURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/recovery" {
                return (200, ["Content-Type": "application/json"], Self.recoveryBody(cursor: "0"))
            }
            if request.url?.path == "/v1/ui/snapshot" {
                attempt += 1
                if attempt == 2 { return (503, [:], Data()) }
                return (200, ["Content-Type": "application/json"], Data((attempt == 1 ? #"{"cursor":10,"surfaces":[]}"# : snapshot).utf8))
            }
            guard request.url?.path == "/v1/events" else { return (503, [:], Data()) }
            streamCursors.append(request.value(forHTTPHeaderField: "Last-Event-ID"))
            let body = attempt == 1 ? frame(10, "task.updated")
                : frame(11, "voice.end") + frame(12, "ui.surface", payload: surface, version: 2) + frame(13, "task.updated")
            return (200, ["Content-Type": "text/event-stream"], Data(body.utf8))
        }
        try await model.receiveEventConnectionForTesting(api: api)
        XCTAssertEqual(model.eventCursorForTesting, "10")
        do {
            try await model.receiveEventConnectionForTesting(api: api)
            XCTFail("Failed snapshot must not open SSE or advance cursor")
        } catch { }
        XCTAssertEqual(model.eventCursorForTesting, "10")
        XCTAssertEqual(streamCursors.count, 1)
        try await model.receiveEventConnectionForTesting(api: api)
        XCTAssertEqual(streamCursors, ["1", "10"])
        XCTAssertEqual(consumed, ["task.updated", "voice.end", "ui.surface", "task.updated"])
        XCTAssertEqual(mediaStops, 1, "Replayed voice.end must close actual local media")
        XCTAssertEqual(hangups, 1, "Replayed voice.end must run the voice shutdown handler")
        guard case .idle = model.voiceStatus else { return XCTFail("Confirmed voice shutdown must finish idle") }
        XCTAssertEqual(renderCount, 1, "Snapshot revision must suppress its obsolete SSE replay")
        XCTAssertEqual(model.eventCursorForTesting, "13", "Event racing snapshot12 remains consumed")
    }

    @MainActor func testExpiredGlobalHistoryStopsWithoutAdvancingToUISnapshotCursor() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        model.setAuthenticationForTesting(.connected)
        model.setSnapshotForTesting(Snapshot(serverVersion: "test", eventCursor: "10"))
        ContractURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/recovery" { return (503, [:], Data()) }
            if request.url?.path == "/v1/ui/snapshot" {
                return (200, ["Content-Type": "application/json"], Data(#"{"cursor":99,"surfaces":[]}"#.utf8))
            }
            XCTAssertEqual(request.value(forHTTPHeaderField: "Last-Event-ID"), "10")
            return (200, ["Content-Type": "text/event-stream"], Data("event: snapshot_required\ndata: {\"reason\":\"cursor_expired\"}\n\n".utf8))
        }
        try await model.receiveEventConnectionForTesting(api: api)
        do {
            try await model.receiveEventConnectionForTesting(api: api)
            XCTFail("Unavailable recovery must fail and retain the consumed cursor")
        } catch { }
        XCTAssertTrue(model.eventRecoveryRequired)
        XCTAssertEqual(model.authentication, .unavailable)
        XCTAssertEqual(model.eventCursorForTesting, "10")
    }

    @MainActor func testExpiredHistoryRecoveryThenPostCheckpointVoiceEnd() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        model.setAuthenticationForTesting(.connected)
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "10"))
        _ = model.installVoiceSessionForTesting(VoiceSessionResponse(voiceSessionId: "old", generation: 1, state: "active", answerSdp: ""), api: api)
        var stops = 0
        var hangups = 0
        model.voiceLocalMediaStopObserverForTesting = { stops += 1 }
        model.voiceHangupOverrideForTesting = { id in
            XCTAssertEqual(id, "old")
            hangups += 1
            return VoiceOperationResponse(ok: true, state: "ended", upstreamTermination: "verified", poolBlocked: false)
        }
        var paths: [String] = []
        var streams = 0
        ContractURLProtocol.responseHandler = { request in
            let path = request.url!.path
            paths.append(path)
            if path == "/v1/recovery" {
                let body = String(data: Self.recoveryBody(cursor: "20"), encoding: .utf8)!
                    .replacingOccurrences(of: "\"current_session\":null", with: "\"current_session\":{\"id\":\"old\",\"generation\":1,\"state\":\"active\"}")
                return (200, [:], Data(body.utf8))
            }
            if path == "/v1/ui/snapshot" { return (200, [:], Data(#"{"cursor":20,"surfaces":[]}"#.utf8)) }
            streams += 1
            if streams == 1 {
                return (200, ["Content-Type": "text/event-stream"], Data("event: snapshot_required\ndata: {\"reason\":\"cursor_expired\"}\n\n".utf8))
            }
            XCTAssertEqual(request.value(forHTTPHeaderField: "Last-Event-ID"), "20")
            XCTAssertEqual(stops, 0, "Exact active recovery session must survive until post-checkpoint SSE")
            XCTAssertEqual(hangups, 0)
            let event = #"{"schema_version":1,"event_id":"end","cursor":"21","type":"voice.end","occurred_at":"","workspace_id":"default","aggregate_type":"voice_session","aggregate_id":"old","aggregate_version":1,"payload":{}}"#
            return (200, ["Content-Type": "text/event-stream"], Data("id: 21\nevent: voice.end\ndata: \(event)\n\n".utf8))
        }
        try await model.receiveEventConnectionForTesting(api: api)
        XCTAssertEqual(model.eventCursorForTesting, "10")
        try await model.receiveEventConnectionForTesting(api: api)
        XCTAssertFalse(model.eventRecoveryRequired)
        XCTAssertEqual(model.eventCursorForTesting, "21")
        XCTAssertEqual(model.authoritativeControlSnapshot?.tasks.count, 1)
        XCTAssertEqual(stops, 1, "Post-checkpoint voice.end must close actual local media")
        XCTAssertEqual(hangups, 1, "Post-checkpoint voice.end must invoke server hangup")
        guard case .idle = model.voiceStatus else { return XCTFail("Confirmed post-checkpoint voice shutdown must finish idle") }
        XCTAssertTrue(paths.contains("/v1/recovery"))
    }

    @MainActor func testInitialZeroHistoryUsesAuthoritativeBaselineBeforeSSE() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        model.setAuthenticationForTesting(.connected)
        var paths: [String] = []
        ContractURLProtocol.responseHandler = { request in
            let path = request.url!.path
            paths.append(path)
            if path == "/v1/recovery" { return (200, [:], Self.recoveryBody(cursor: "99")) }
            if path == "/v1/ui/snapshot" { return (200, [:], Data(#"{"cursor":100,"surfaces":[]}"#.utf8)) }
            XCTAssertEqual(request.value(forHTTPHeaderField: "Last-Event-ID"), "99")
            return (200, ["Content-Type": "text/event-stream"], Data())
        }
        try await model.receiveEventConnectionForTesting(api: api)
        XCTAssertEqual(paths.first, "/v1/recovery")
        XCTAssertEqual(model.eventCursorForTesting, "99")
        XCTAssertEqual(model.authoritativeControlSnapshot?.tasks.count, 1)
    }

    @MainActor func testRecoveryResponseFromOldConnectionCannotAdoptCheckpoint() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        model.setAuthenticationForTesting(.connected)
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "0"))
        let gate = AsyncTestGate()
        model.recoveryApplicationGateForTesting = { await gate.pause() }
        ContractURLProtocol.responseHandler = { request in
            XCTAssertEqual(request.url?.path, "/v1/recovery")
            return (200, [:], Self.recoveryBody(cursor: "99"))
        }
        let pending = Task { try await model.receiveEventConnectionForTesting(api: api) }
        await gate.waitUntilStarted()
        model.advanceConnectionForTesting() // Same-origin credential rotation also advances this fence.
        await gate.resume()
        try await pending.value
        XCTAssertEqual(model.eventCursorForTesting, "0")
        XCTAssertNil(model.authoritativeControlSnapshot)
    }

    @MainActor func testRecoveryPreservesOnlyExactActiveLocalVoiceIdentity() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        model.setAuthenticationForTesting(.connected)
        _ = model.installVoiceSessionForTesting(VoiceSessionResponse(voiceSessionId: "local", generation: 3, state: "active", answerSdp: ""), api: api)
        var hangups = 0
        model.voiceHangupOverrideForTesting = { _ in
            hangups += 1
            return VoiceOperationResponse(ok: true, state: "ended", upstreamTermination: "verified", poolBlocked: false)
        }
        ContractURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/recovery" {
                let body = String(data: Self.recoveryBody(cursor: "20"), encoding: .utf8)!
                    .replacingOccurrences(of: "\"current_session\":null", with: "\"current_session\":{\"id\":\"local\",\"generation\":3,\"state\":\"active\"}")
                return (200, [:], Data(body.utf8))
            }
            if request.url?.path == "/v1/ui/snapshot" { return (200, [:], Data(#"{"cursor":20,"surfaces":[]}"#.utf8)) }
            return (200, ["Content-Type": "text/event-stream"], Data())
        }
        try await model.receiveEventConnectionForTesting(api: api)
        XCTAssertEqual(hangups, 0)
        guard case .active = model.voiceStatus else { return XCTFail("Exact active session must survive recovery") }
        XCTAssertEqual(model.eventCursorForTesting, "20")
    }

    @MainActor func testRecoveryFailedRenderRetriesSameRevisionWithoutModelCommit() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "10"))
        var renders = 0
        model.onUISurface = { _ in renders += 1; return renders > 1 }
        let surface = #"{"request_id":"pending","surface_id":"s1","surface_version":2,"container":"window","awaiting_input":true,"task_id":"task","question_id":"question","render_status":"rendered","messages":[{"version":"v0.9","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}]}"#
        let body = String(data: Self.recoveryBody(cursor: "20"), encoding: .utf8)!
            .replacingOccurrences(of: "\"surfaces\":[]", with: "\"surfaces\":[\(surface)]")
        ContractURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/recovery" { return (200, [:], Data(body.utf8)) }
            // Pending ACK lookup failure must not roll back or poison recovery.
            return (503, [:], Data())
        }
        do { try await model.recoverGlobalStateForTesting(api: api); XCTFail("First render must fail") } catch { }
        XCTAssertEqual(model.eventCursorForTesting, "10")
        XCTAssertNil(model.authoritativeControlSnapshot)
        XCTAssertNil(model.recoveredVoicePool)
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(renders, 2)
        XCTAssertEqual(model.eventCursorForTesting, "20")
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(renders, 2, "Successful presentation revision is not rendered twice")
    }

    @MainActor func testRecoverySuppressesHistoricalSurfaceButLiveRevisionStillDisplays() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        model.setAuthenticationForTesting(.connected)
        var renders = 0
        model.onUISurface = { _ in renders += 1; return true }
        let surface = #"{"request_id":"old","surface_id":"s1","surface_version":1,"container":"window","awaiting_input":false,"render_status":"rendered","messages":[{"version":"v0.9","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}]}"#
        let body = String(data: Self.recoveryBody(cursor: "20"), encoding: .utf8)!
            .replacingOccurrences(of: "\"surfaces\":[]", with: "\"surfaces\":[\(surface)]")
        ContractURLProtocol.responseHandler = { _ in (200, [:], Data(body.utf8)) }
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(renders, 0)
        XCTAssertEqual(model.eventCursorForTesting, "20")
        let live = surface.replacingOccurrences(of: "\"surface_version\":1", with: "\"surface_version\":2")
        let payload = try JSONDecoder().decode(JSONValue.self, from: Data(live.utf8))
        await model.handleEventForTesting(EventEnvelope(schemaVersion: 1, eventId: "live", cursor: "21", type: "ui.surface", occurredAt: "", workspaceId: "default", aggregateType: "ui_surface", aggregateId: "s1", aggregateVersion: 2, payload: payload))
        XCTAssertEqual(renders, 1)
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(renders, 1, "Reconnect must not hide or recreate the already-presented live surface")
    }

    @MainActor func testRecoveryWithoutDeletedPresentsPendingQuestionOnLaunch() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        var renders = 0
        model.onUISurface = { event in
            XCTAssertTrue(event.awaitingInput)
            XCTAssertEqual(event.taskID, "task")
            XCTAssertEqual(event.questionID, "question")
            renders += 1
            return true
        }
        let surface = #"{"request_id":"pending","surface_id":"s1","surface_version":1,"container":"popover","awaiting_input":true,"task_id":"task","question_id":"question","render_status":"rendered","messages":[{"version":"v0.9","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}]}"#
        let body = String(data: Self.recoveryBody(cursor: "20"), encoding: .utf8)!
            .replacingOccurrences(of: "\"surfaces\":[]", with: "\"surfaces\":[\(surface)]")
        ContractURLProtocol.responseHandler = { _ in (200, [:], Data(body.utf8)) }
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(renders, 1)
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(renders, 1)
    }

    #if os(macOS)
    @MainActor func testNativeSurfaceWithoutDeletedPresentsFromEventAndSnapshotAndACKReachesTransport() async throws {
        _ = NSApplication.shared
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("journal.json"))
        let model = ZoeAppModel()
        let manager = SurfaceManager(popoverAnchor: { nil })
        defer { manager.reset() }
        model.installRecoveryCoordinatorForTesting(CommandCoordinator(api: api, journal: journal))
        model.setAuthenticationForTesting(.connected)
        model.onUISurface = { manager.display($0) }
        model.isCurrentUIPresentation = { manager.isCurrentPresentation($0) }
        model.onUISurfacesClosed = { ids in ids.forEach { manager.close(id: $0) } }
        let live = #"{"request_id":"live","surface_id":"s1","surface_version":1,"container":"window","awaiting_input":false,"messages":[{"version":"v0.9","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}},{"version":"v0.9","updateComponents":{"surfaceId":"s1","components":[{"id":"root","component":"Text","text":"Hello"}]}}]}"#
        let payload = try JSONDecoder().decode(JSONValue.self, from: Data(live.utf8))
        await model.handleEventForTesting(EventEnvelope(schemaVersion: 1, eventId: "live", cursor: "10", type: "ui.surface", occurredAt: "", workspaceId: "default", aggregateType: "ui_surface", aggregateId: "s1", aggregateVersion: 1, payload: payload))
        XCTAssertNotNil(manager.viewModel(for: "s1"))
        let pending = #"{"request_id":"pending","surface_id":"s2","surface_version":1,"container":"window","awaiting_input":true,"task_id":"task","question_id":"question","render_status":"rendered","messages":[{"version":"v0.9","createSurface":{"surfaceId":"s2","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}},{"version":"v0.9","updateComponents":{"surfaceId":"s2","components":[{"id":"root","component":"Text","text":"Question"}]}}]}"#
        let body = String(data: Self.recoveryBody(cursor: "20"), encoding: .utf8)!.replacingOccurrences(of: "\"surfaces\":[]", with: "\"surfaces\":[\(pending)]")
        let ack = expectation(description: "Native presentation ACK reaches transport")
        ContractURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/recovery" { return (200, [:], Data(body.utf8)) }
            if request.httpMethod == "POST" {
                ack.fulfill()
            }
            return (503, [:], Data())
        }
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertNil(manager.viewModel(for: "s1"))
        XCTAssertNotNil(manager.viewModel(for: "s2"))
        XCTAssertEqual(manager.presentationVersionForTesting("s2"), 1)
        await fulfillment(of: [ack], timeout: 3)
        XCTAssertEqual(model.eventCursorForTesting, "20")
    }
    #endif

    @MainActor func testGenerationSwitchAfterRenderingCannotCommitStagedModel() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "10"))
        let gate = AsyncTestGate()
        model.recoveryCommitGateForTesting = { await gate.pause() }
        ContractURLProtocol.responseHandler = { _ in (200, [:], Self.recoveryBody(cursor: "20")) }
        let pending = Task { try await model.recoverGlobalStateForTesting(api: api) }
        await gate.waitUntilStarted()
        model.advanceConnectionForTesting()
        await gate.resume()
        do { try await pending.value; XCTFail("Stale commit must fail") } catch { }
        XCTAssertEqual(model.eventCursorForTesting, "10")
        XCTAssertNil(model.authoritativeControlSnapshot)
        XCTAssertNil(model.recoveredVoicePool)
    }

    @MainActor func testPendingACKLookupFailureDoesNotGateRecoveryOrNextRecovery() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let coordinator = CommandCoordinator(api: api, journal: journal)
        ContractURLProtocol.responseHandler = { _ in (503, [:], Data()) }
        do {
            _ = try await coordinator.submit(kind: "ui.rendered", payload: JSONValue.object([:]), workspaceID: "default", idempotencyKey: "pending-render")
        } catch { }
        let model = ZoeAppModel()
        model.installRecoveryCoordinatorForTesting(coordinator)
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "10"))
        ContractURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/recovery" { return (200, [:], Self.recoveryBody(cursor: "20")) }
            return (503, [:], Data())
        }
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(model.eventCursorForTesting, "20")
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(model.authoritativeControlSnapshot?.tasks.count, 1)
        let pending = try await journal.all()
        XCTAssertTrue(pending.contains { $0.idempotencyKey == "pending-render" })
    }

    @MainActor func testRecoveryStopsEndedReplacedAndGenerationMismatchedVoice() async throws {
        for current in ["null", #"{"id":"local","generation":3,"state":"ended"}"#, #"{"id":"replacement","generation":3,"state":"active"}"#, #"{"id":"local","generation":4,"state":"active"}"#] {
            let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
            let model = ZoeAppModel()
            model.setAuthenticationForTesting(.connected)
            _ = model.installVoiceSessionForTesting(VoiceSessionResponse(voiceSessionId: "local", generation: 3, state: "active", answerSdp: ""), api: api)
            var stops = 0
            model.voiceLocalMediaStopObserverForTesting = { stops += 1 }
            let body = String(data: Self.recoveryBody(cursor: "20"), encoding: .utf8)!
                .replacingOccurrences(of: "\"current_session\":null", with: "\"current_session\":\(current)")
            ContractURLProtocol.responseHandler = { _ in (200, [:], Data(body.utf8)) }
            try await model.recoverGlobalStateForTesting(api: api)
            XCTAssertEqual(stops, 1)
            guard case .stopped = model.voiceStatus else { return XCTFail("Obsolete local audio must stop") }
            XCTAssertEqual(model.eventCursorForTesting, "20")
        }
    }

    @MainActor func testRecoveryRejectsRotatedCredentialWithoutCursorAdvance() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = ZoeAppModel()
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "0"))
        ContractURLProtocol.responseHandler = { _ in
            (401, [:], Data(#"{"error":{"code":"unauthenticated","message":"rotated"}}"#.utf8))
        }
        do { try await model.receiveEventConnectionForTesting(api: api); XCTFail("Expected rejection") } catch { }
        XCTAssertEqual(model.eventCursorForTesting, "0")
        XCTAssertTrue(model.eventRecoveryRequired)
        XCTAssertEqual(model.authentication, .unavailable)
    }

    @MainActor func testConnectionSwitchDuringBlockedReceiptNeverResubmits() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory); ContractURLProtocol.responseGate = nil }
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("journal.json"))
        let coordinator = CommandCoordinator(api: api, journal: journal)
        ContractURLProtocol.responseHandler = { _ in (503, [:], Data()) }
        do { _ = try await coordinator.submit(kind: "ui.rendered", payload: .object([:]), workspaceID: "default", idempotencyKey: "old") } catch { }
        let gate = AsyncTestGate()
        var valid = true
        var posts = 0
        ContractURLProtocol.responseGate = { request in
            if request.httpMethod == "GET" { await gate.pause() }
        }
        ContractURLProtocol.responseHandler = { request in
            if request.httpMethod == "POST" { posts += 1 }
            return (404, [:], Data(#"{"error":{"code":"not_found","message":"missing"}}"#.utf8))
        }
        let recovery = Task {
            try await coordinator.recoverPending(currentWorkspaceID: "default", onOutcome: { _ in }, isValid: { valid })
        }
        await gate.waitUntilStarted()
        valid = false
        await gate.resume()
        do { _ = try await recovery.value; XCTFail("Expected cancellation") } catch { }
        XCTAssertEqual(posts, 0)
        let pending = try await journal.all()
        XCTAssertEqual(pending.first?.idempotencyKey, "old")
    }

    func testUIACKAndActionUnknownAcceptanceRecoverOriginalReceipt() async throws {
        for kind in ["ui.rendered", "ui.action"] {
            let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: directory) }
            let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
            let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
            let coordinator = CommandCoordinator(api: api, journal: journal)
            let key = UUID().uuidString
            let payload: JSONValue = .object(["request_id": .string("r1"), "surface_id": .string("s1"), "surface_version": .integer(2)])
            var posts = 0
            ContractURLProtocol.failureHandler = { request in
                guard request.httpMethod == "POST" else { return nil }
                posts += 1
                return URLError(.timedOut)
            }
            ContractURLProtocol.responseHandler = { request in
                XCTAssertEqual(request.httpMethod, "GET")
                return (200, ["Content-Type": "application/json"], Self.commandReceipt(key: key, kind: kind, workspaceID: "workspace-1", resourceID: "s1"))
            }
            do {
                _ = try await coordinator.submit(kind: kind, payload: payload, workspaceID: "workspace-1", idempotencyKey: key)
                XCTFail("Expected uncertain acceptance")
            } catch { }
            let pending = try await journal.all()
            XCTAssertEqual(pending.first?.payload, payload)
            let outcomes = try await coordinator.recoverPending(currentWorkspaceID: "workspace-1", onOutcome: { _ in })
            XCTAssertEqual(outcomes.count, 1)
            XCTAssertTrue(try XCTUnwrap(outcomes.first).recovered)
            XCTAssertEqual(posts, 1)
            let remaining = try await journal.all()
            XCTAssertTrue(remaining.isEmpty)
        }
        ContractURLProtocol.failureHandler = nil
    }

    func testConversationTextRecoveryResendsJournaledCommand() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory); ContractURLProtocol.failureHandler = nil }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let coordinator = CommandCoordinator(api: api, journal: journal)
        let key = UUID().uuidString.lowercased()
        let payload: JSONValue = .object(["text": .string("hello")])
        ContractURLProtocol.failureHandler = { request in request.httpMethod == "POST" ? URLError(.timedOut) : nil }
        ContractURLProtocol.responseHandler = { _ in (404, [:], Data(#"{"error":{"code":"not_found","message":"missing"}}"#.utf8)) }
        do {
            _ = try await coordinator.submit(kind: "voice.text_input", payload: payload, workspaceID: "workspace-1", idempotencyKey: key)
            XCTFail("Expected an uncertain submission")
        } catch { }
        let pending = try await journal.all()
        XCTAssertEqual(pending.first?.kind, "voice.text_input")
        XCTAssertEqual(pending.first?.payload, payload)
        ContractURLProtocol.failureHandler = nil
        ContractURLProtocol.responseHandler = { request in
            if request.httpMethod == "GET" { return (404, [:], Data(#"{"error":{"code":"not_found","message":"missing"}}"#.utf8)) }
            return (200, ["Content-Type": "application/json"], Self.commandReceipt(key: key, kind: "voice.text_input", workspaceID: "workspace-1", resourceID: ""))
        }
        let outcomes = try await coordinator.recoverPending(currentWorkspaceID: "workspace-1", onOutcome: { _ in })
        XCTAssertEqual(outcomes.count, 1)
        XCTAssertFalse(try XCTUnwrap(outcomes.first).recovered)
        let remaining = try await journal.all()
        XCTAssertTrue(remaining.isEmpty)
    }

    func testSSEParserCombinesDataLinesAndPreservesDecimalCursor() throws {
        let payload = #"{"schema_version":1,"event_id":"ev-7","cursor":"9007199254740993","type":"task.updated","occurred_at":"2026-10-20T12:00:00Z","workspace_id":"workspace","aggregate_type":"task","aggregate_id":"task-1","aggregate_version":5,"payload":{"status":"active"}}"#
        var parser = SSEParser()
        XCTAssertNil(try parser.consume(line: "id: 9007199254740993"))
        XCTAssertNil(try parser.consume(line: "event: task.updated"))
        XCTAssertNil(try parser.consume(line: "data: \(payload)"))
        guard case let .event(event)? = try parser.consume(line: "") else {
            return XCTFail("Expected one event after the SSE record boundary")
        }
        XCTAssertEqual(event.cursor, "9007199254740993")
        XCTAssertEqual(event.schemaVersion, 1)
    }

    func testExpiredSSECursorRequestsSnapshotRepair() throws {
        var parser = SSEParser()
        XCTAssertNil(try parser.consume(line: "event: snapshot_required"))
        XCTAssertNil(try parser.consume(line: #"data: {"reason":"cursor_expired","authority":{"instance_lineage_id":"lineage-2","generation":6,"deletion_sequence":"9"}}"#))
        guard case let .snapshotRequired(reason)? = try parser.consume(line: "") else {
            return XCTFail("Expected snapshot repair request")
        }
        XCTAssertEqual(reason, "cursor_expired")
    }

    func testSSEParserDropsUnterminatedRecordAtEOF() throws {
        let payload = #"{"schema_version":1,"event_id":"ev-8","cursor":"20","type":"task.updated","occurred_at":"2026-10-20T12:00:00Z","workspace_id":"workspace","aggregate_type":"task","aggregate_id":"task-1","aggregate_version":6,"payload":{}}"#
        var parser = SSEParser()
        _ = try parser.consume(line: "id: 20")
        _ = try parser.consume(line: "event: task.updated")
        _ = try parser.consume(line: "data: \(payload)")
        XCTAssertNil(try parser.finish(), "An SSE frame without its record boundary must not advance the event cursor.")
    }

    func testCommandReceiptDecodesOmittedResourceMetadata() throws {
        let data = Data(#"{"command_id":"cmd-1","idempotency_key":"key-1","workspace_id":"ws-1","kind":"voice.turn.handoff","status":"accepted","accepted_at":"2026-10-20T12:00:00Z","event_cursor":"21"}"#.utf8)
        let receipt = try ZoeJSON.decoder.decode(CommandReceipt.self, from: data)
        XCTAssertEqual(receipt.idempotencyKey, "key-1")
        XCTAssertNil(receipt.resourceType)
        XCTAssertNil(receipt.resourceId)
    }

    func testJournalRejectsSameKeyBoundToDifferentPayload() async throws {
        let root = URL(fileURLWithPath: ProcessInfo.processInfo.environment["ZOE_BUILD_ROOT"] ?? "/Users/zhuwei/terminal/zoe-apple-build/test-data", isDirectory: true)
        let directory = root.appendingPathComponent("swift-journal-tests", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let file = directory.appendingPathComponent("\(UUID().uuidString).json")
        let journal = try CommandJournal(fileURL: file)
        let first = PendingCommand(idempotencyKey: "stable-key", kind: "voice.turn.handoff", expectedVersion: 2, payload: .object(["content": .string("first")]), createdAt: "now", workspaceID: "ws", expectedResourceType: nil, expectedResourceID: nil)
        let changed = PendingCommand(idempotencyKey: "stable-key", kind: "voice.turn.handoff", expectedVersion: 2, payload: .object(["content": .string("second")]), createdAt: "later", workspaceID: "ws", expectedResourceType: nil, expectedResourceID: nil)
        try await journal.append(first)
        do {
            try await journal.append(changed)
            XCTFail("Expected a local idempotency conflict")
        } catch let error as ZoeError {
            guard case let .http(status, code, _, _) = error else { return XCTFail("Unexpected error: \(error)") }
            XCTAssertEqual(status, 409)
            XCTAssertEqual(code, "local_idempotency_conflict")
        }
        try? FileManager.default.removeItem(at: file)
    }

    func testUncertainSubmissionChecksReceiptBeforeRetryAndDoesNotPostTwice() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let coordinator = CommandCoordinator(api: api, journal: journal)
        let key = "stable-retry-\(UUID().uuidString.lowercased())"
        var postCount = 0
        var receiptCount = 0
        var acceptedReceipt = Data()
        ContractURLProtocol.failureHandler = { request in
            guard request.url?.path == "/v1/commands", request.httpMethod == "POST" else { return nil }
            postCount += 1
            let submittedKey = request.value(forHTTPHeaderField: "Idempotency-Key") ?? ""
            acceptedReceipt = Self.commandReceipt(key: submittedKey, kind: "voice.turn.commit", workspaceID: "workspace-1", resourceID: "task-created")
            return URLError(.timedOut)
        }
        ContractURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/commands", request.httpMethod == "GET" {
                receiptCount += 1
                return (200, ["Content-Type": "application/json"], acceptedReceipt)
            }
            return (503, ["Content-Type": "application/json"], Data())
        }

        do {
            _ = try await coordinator.submit(kind: "voice.turn.commit", payload: .object(["title": .string("Work")]), workspaceID: "workspace-1", idempotencyKey: key)
            XCTFail("The first response should be lost after the server accepts the command")
        } catch { }
        let pendingAfterTimeout = try await journal.all()
        XCTAssertEqual(pendingAfterTimeout.map(\.idempotencyKey), [key])

        let recovered = try await coordinator.retryPending(idempotencyKey: key, currentWorkspaceID: "workspace-1")

        XCTAssertTrue(recovered.recovered)
        XCTAssertEqual(recovered.receipt.idempotencyKey, key)
        XCTAssertEqual(postCount, 1, "a receipt found after a lost response must prevent another command POST")
        XCTAssertEqual(receiptCount, 1)
        let remaining = try await journal.all()
        XCTAssertTrue(remaining.isEmpty)
    }

    func testPendingRetryPostsOriginalPayloadAndKeyOnlyAfterDefinitiveReceipt404() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let key = "definitive-404-\(UUID().uuidString.lowercased())"
        let payload: JSONValue = .object(["title": .string("Original title"), "goal": .string("Original payload")])
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "voice.turn.commit", expectedVersion: nil, payload: payload, createdAt: "2026-10-22T12:00:00Z", workspaceID: "workspace-1", expectedResourceType: nil, expectedResourceID: nil))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let coordinator = CommandCoordinator(api: api, journal: journal)
        var receiptCount = 0
        var postCount = 0
        var postKey: String?
        var postBody: Data?
        ContractURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/commands", request.httpMethod == "GET" {
                receiptCount += 1
                return (404, ["Content-Type": "application/json"], Data())
            }
            if request.url?.path == "/v1/commands", request.httpMethod == "POST" {
                postCount += 1
                postKey = request.value(forHTTPHeaderField: "Idempotency-Key")
                postBody = Self.requestBody(request)
                return (200, ["Content-Type": "application/json"], Self.commandReceipt(key: key, kind: "voice.turn.commit", workspaceID: "workspace-1", resourceID: "task-created"))
            }
            return (503, ["Content-Type": "application/json"], Data())
        }

        let outcome = try await coordinator.retryPending(idempotencyKey: key, currentWorkspaceID: "workspace-1")

        XCTAssertEqual(receiptCount, 1, "receipt lookup must precede the safe retry")
        XCTAssertEqual(postCount, 1)
        XCTAssertEqual(postKey, key)
        let postedEnvelope = try XCTUnwrap(postBody.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] })
        XCTAssertEqual(postedEnvelope["kind"] as? String, "voice.turn.commit")
        XCTAssertEqual(postedEnvelope["payload"] as? [String: String], ["title": "Original title", "goal": "Original payload"])
        XCTAssertEqual(outcome.receipt.idempotencyKey, key)
        let remaining = try await journal.all()
        XCTAssertTrue(remaining.isEmpty)
    }

    func testPendingRetryRefusesIncompatibleWorkspaceWithoutNetworkRequest() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let key = "workspace-guard-\(UUID().uuidString.lowercased())"
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "voice.turn.commit", expectedVersion: nil, payload: .object([:]), createdAt: "now", workspaceID: "old-workspace", expectedResourceType: nil, expectedResourceID: nil))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let coordinator = CommandCoordinator(api: api, journal: journal)
        ContractURLProtocol.responseHandler = { _ in XCTFail("workspace mismatch must not make a network request"); return (500, [:], Data()) }

        do {
            _ = try await coordinator.retryPending(idempotencyKey: key, currentWorkspaceID: "new-workspace")
            XCTFail("Expected a workspace guard")
        } catch let error as ZoeError {
            guard case .pendingCommandWorkspaceMismatch = error else { return XCTFail("Unexpected error: \(error)") }
        }
        let remaining = try await journal.all()
        XCTAssertEqual(remaining.map(\.idempotencyKey), [key])
    }

    func testPendingReceiptLookupNetworkFailureDoesNotResubmitOrDropJournalRow() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let key = "receipt-timeout-\(UUID().uuidString.lowercased())"
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "voice.turn.commit", expectedVersion: nil, payload: .object(["title": .string("Work")]), createdAt: "now", workspaceID: "workspace-1", expectedResourceType: nil, expectedResourceID: nil))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let coordinator = CommandCoordinator(api: api, journal: journal)
        var postCount = 0
        ContractURLProtocol.failureHandler = { request in
            if request.url?.path == "/v1/commands", request.httpMethod == "GET" { return URLError(.timedOut) }
            if request.url?.path == "/v1/commands", request.httpMethod == "POST" { postCount += 1 }
            return nil
        }
        ContractURLProtocol.responseHandler = { _ in XCTFail("a failed receipt lookup must not fall through to POST"); return (500, [:], Data()) }

        do {
            _ = try await coordinator.retryPending(idempotencyKey: key, currentWorkspaceID: "workspace-1")
            XCTFail("Expected receipt lookup to remain uncertain")
        } catch { }

        XCTAssertEqual(postCount, 0)
        let remaining = try await journal.all()
        XCTAssertEqual(remaining.map(\.idempotencyKey), [key])
    }

    @MainActor
    func testVoiceMediaEpochFenceRejectsDelayedAndDuplicateLossEvents() throws {
        var fence = VoiceMediaEpochFence()
        let first = fence.beginAttempt()
        XCTAssertTrue(fence.accepts(first))
        XCTAssertTrue(fence.stop(first), "the first terminal media event claims teardown")
        XCTAssertFalse(fence.accepts(first), "callbacks after teardown cannot revive the stopped attempt")
        XCTAssertFalse(fence.stop(first), "a duplicate connection/audio callback cannot repeat teardown or release")

        let second = fence.beginAttempt()
        XCTAssertGreaterThan(second, first)
        XCTAssertFalse(fence.stop(first), "a delayed event from the old connection cannot stop the new attempt")
        XCTAssertTrue(fence.accepts(second))
    }

    @MainActor
    func testVoiceAudioInterruptionDuringSetupFencesLateHandshakeAndDuplicateEvents() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        var localStopCount = 0
        model.voiceLocalMediaStopObserverForTesting = { localStopCount += 1 }
        let startingEpoch = model.beginVoiceAttemptForTesting()

        await model.handleVoiceMediaLossForTesting(.audio(.interrupted), mediaEpoch: startingEpoch)
        await model.handleVoiceMediaLossForTesting(.audio(.interrupted), mediaEpoch: startingEpoch)

        XCTAssertFalse(model.canContinueVoiceAttemptForTesting(startingEpoch), "a late SDP/connected response cannot reactivate the interrupted attempt")
        XCTAssertEqual(localStopCount, 1, "the startup interruption claims local teardown once")
        XCTAssertFalse(model.canEndVoiceSession, "the event does not invent an active server session")
        guard case .interrupted = model.voiceStatus else { return XCTFail("expected an interrupted voice setup") }

        let nextEpoch = model.beginVoiceAttemptForTesting()
        XCTAssertTrue(model.canContinueVoiceAttemptForTesting(nextEpoch))
        XCTAssertFalse(model.canContinueVoiceAttemptForTesting(startingEpoch), "old callbacks remain fenced after a later attempt begins")
    }

    @MainActor
    func testVoiceMediaLossStopsHeartbeatAndPreservesTaskWhileHangupQuarantinesPool() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        var hangupCount = 0
        var localStopCount = 0
        ContractURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/voice/sessions/session-1/hangup" {
                hangupCount += 1
                XCTAssertEqual(localStopCount, 1, "the hangup receipt is sent only after local media teardown")
                let body = (try? JSONSerialization.jsonObject(with: Self.requestBody(request) ?? Data())) as? [String: Any]
                XCTAssertEqual(body?["media_closed"] as? Bool, true)
                XCTAssertEqual(body?["generation"] as? Int, 4)
                XCTAssertEqual(body?["account_lease_id"] as? String, "opaque-lease-1")
                XCTAssertEqual(body?["account_lease_generation"] as? Int, 11)
                if hangupCount == 1 {
                    return (202, ["Content-Type": "application/json"], Data(#"{"ok":true,"state":"ended","upstream_termination":"unverified","pool_blocked":true}"#.utf8))
                }
                return (200, ["Content-Type": "application/json"], Data(#"{"ok":true,"state":"ended","upstream_termination":"verified","pool_blocked":false}"#.utf8))
            }
            if request.url?.path == "/v1/voice/pool/status" {
                return (200, ["Content-Type": "application/json"], Data(#"{"blocked":true,"reason_code":"upstream_termination_unverified","updated_at":"2026-10-22T12:00:00Z","account_lease_identity":false,"account_scoped_termination":false,"trusted_termination_evidence":false}"#.utf8))
            }
            return (404, [:], Data())
        }
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let session = VoiceSessionResponse(voiceSessionId: "session-1", generation: 4, state: "active", answerSdp: "", accountLeaseId: "opaque-lease-1", accountLeaseGeneration: 11)
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        let heartbeatStarted = expectation(description: "heartbeat task starts")
        let heartbeatCancelled = expectation(description: "heartbeat task is canceled on media loss")
        model.installVoiceHeartbeatForTesting(Task {
            heartbeatStarted.fulfill()
            await withTaskCancellationHandler {
                try? await Task.sleep(for: .seconds(30))
            } onCancel: {
                heartbeatCancelled.fulfill()
            }
        })
        model.voiceLocalMediaStopObserverForTesting = { localStopCount += 1 }
        await fulfillment(of: [heartbeatStarted], timeout: 1)

        await model.handleVoiceMediaLossForTesting(.connection(.disconnected), mediaEpoch: epoch)
        await model.handleVoiceMediaLossForTesting(.connection(.failed), mediaEpoch: epoch)
        await fulfillment(of: [heartbeatCancelled], timeout: 1)

        XCTAssertFalse(model.voiceHeartbeatActiveForTesting)
        XCTAssertEqual(localStopCount, 1, "local media teardown is claimed once")
        XCTAssertEqual(hangupCount, 1, "duplicate terminal signals issue at most one hangup")
        guard case let .interrupted(reason) = model.voiceStatus else { return XCTFail("expected an interrupted voice status") }
        XCTAssertEqual(reason, uiFormat("%@ Local media is closed. Upstream termination is unverified; the account pool remains quarantined. Retry server hangup to repeat the idempotent lease release.", uiText("The WebRTC connection was lost.")))
        XCTAssertTrue(model.voicePoolStatus?.blocked == true)
        XCTAssertTrue(model.canEndVoiceSession, "the identity remains available for an idempotent server release retry")

        await model.endVoiceSession()

        XCTAssertEqual(hangupCount, 2)
        XCTAssertFalse(model.canEndVoiceSession)
    }

    @MainActor
    func testDelayedOldHeartbeatFailureCannotTearDownNewVoiceSession() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let old = VoiceSessionResponse(voiceSessionId: "session-old", generation: 2, state: "active", answerSdp: "")
        let oldEpoch = model.installVoiceSessionForTesting(old, api: api)
        let current = VoiceSessionResponse(voiceSessionId: "session-current", generation: 3, state: "active", answerSdp: "")
        let currentEpoch = model.installVoiceSessionForTesting(current, api: api)
        var localStopCount = 0
        model.voiceLocalMediaStopObserverForTesting = { localStopCount += 1 }

        await model.handleVoiceHeartbeatFailureForTesting(sessionID: old.voiceSessionId, api: api, mediaEpoch: oldEpoch, generation: 0, message: "late timeout")

        XCTAssertEqual(currentEpoch, oldEpoch + 1)
        XCTAssertTrue(model.canEndVoiceSession)
        XCTAssertEqual(localStopCount, 0)
        guard case let .active(sessionID, generation) = model.voiceStatus else { return XCTFail("the newer voice session must remain active") }
        XCTAssertEqual(sessionID, current.voiceSessionId)
        XCTAssertEqual(generation, current.generation)
    }

    @MainActor
    func testConcurrentRetryCannotSendSecondVoiceHangup() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let session = VoiceSessionResponse(voiceSessionId: "session-retry", generation: 5, state: "active", answerSdp: "")
        _ = model.installVoiceSessionForTesting(session, api: api)
        let gate = AsyncTestGate()
        var hangupCount = 0
        model.voiceHangupOverrideForTesting = { _ in
            hangupCount += 1
            await gate.pause()
            return VoiceOperationResponse(ok: true, state: "ended", upstreamTermination: "verified", poolBlocked: false)
        }

        let first = Task { await model.endVoiceSession() }
        await gate.waitUntilStarted()
        await model.endVoiceSession()
        XCTAssertEqual(hangupCount, 1, "a retry while the first hangup is pending cannot issue a second POST")
        await gate.resume()
        await first.value
        XCTAssertEqual(hangupCount, 1)
        XCTAssertFalse(model.canEndVoiceSession)
    }

    private static func commandReceipt(key: String, kind: String, workspaceID: String, resourceID: String, resourceType: String = "task") -> Data {
        Data("{\"command_id\":\"command-1\",\"idempotency_key\":\"\(key)\",\"workspace_id\":\"\(workspaceID)\",\"kind\":\"\(kind)\",\"status\":\"accepted\",\"resource_type\":\"\(resourceType)\",\"resource_id\":\"\(resourceID)\",\"resource_version\":1,\"accepted_at\":\"2026-10-22T12:00:00Z\",\"event_cursor\":\"8\"}".utf8)
    }

    private static func requestBody(_ request: URLRequest) -> Data? {
        if let body = request.httpBody { return body }
        guard let stream = request.httpBodyStream else { return nil }
        stream.open()
        defer { stream.close() }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while stream.hasBytesAvailable {
            let count = stream.read(&buffer, maxLength: buffer.count)
            if count <= 0 { break }
            data.append(buffer, count: count)
        }
        return data.isEmpty ? nil : data
}
}
