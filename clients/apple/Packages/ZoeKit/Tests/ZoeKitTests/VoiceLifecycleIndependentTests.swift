import XCTest
import Foundation
@testable import ZoeKit
@testable import ZoeUI

private struct IndependentVoiceCredential: BearerTokenProviding {
    func bearerToken() throws -> String { "synthetic-independent-voice-token" }
}

private final class IndependentVoiceURLProtocol: URLProtocol {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let body = Data(#"{"ok":true,"state":"ended","upstream_termination":"verified","pool_blocked":false}"#.utf8)
        let response = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: nil, headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

private final class VoiceFinalURLProtocol: URLProtocol {
    static var responseHandler: ((URLRequest) -> (Int, Data))?
    static var requests: [URLRequest] = []

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        Self.requests.append(request)
        let (status, body) = Self.responseHandler?(request) ?? (500, Data())
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}

    static func body(_ request: URLRequest) -> Data? {
        if let body = request.httpBody { return body }
        guard let stream = request.httpBodyStream else { return nil }
        stream.open()
        defer { stream.close() }
        var result = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while stream.hasBytesAvailable {
            let count = stream.read(&buffer, maxLength: buffer.count)
            if count <= 0 { break }
            result.append(buffer, count: count)
        }
        return result
    }
}

final class VoiceLifecycleIndependentTests: XCTestCase {
    private func voiceAPI(installationID: String) throws -> APIClient {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [IndependentVoiceURLProtocol.self]
        return try APIClient(
            serverURL: URL(string: "https://voice-independent.example")!,
            credentials: IndependentVoiceCredential(),
            session: URLSession(configuration: configuration),
            installationID: installationID
        )
    }

    @MainActor
    private func finalModel() throws -> (ZoeAppModel, APIClient, UInt64) {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [VoiceFinalURLProtocol.self]
        let api = try APIClient(serverURL: URL(string: "https://voice-final.example")!, credentials: IndependentVoiceCredential(), session: URLSession(configuration: configuration))
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let session = VoiceSessionResponse(voiceSessionId: "final-session", generation: 4, state: "active", answerSdp: "")
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "1", projects: [], tasks: [], approvals: [], runs: [], workspaceId: "workspace-1"))
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        model.configureVoiceCoordinatorForTesting(api: api, journal: journal)
        return (model, api, epoch)
    }

    private static func receipt(for request: URLRequest, kind: String, resourceType: String, resourceID: String) -> Data {
        let key = request.value(forHTTPHeaderField: "Idempotency-Key") ?? "missing"
        return Data("{\"command_id\":\"command-1\",\"idempotency_key\":\"\(key)\",\"workspace_id\":\"workspace-1\",\"kind\":\"\(kind)\",\"status\":\"accepted\",\"resource_type\":\"\(resourceType)\",\"resource_id\":\"\(resourceID)\",\"resource_version\":1,\"accepted_at\":\"2026-11-04T12:00:00Z\",\"event_cursor\":\"8\"}".utf8)
    }

    @MainActor
    private func waitForVoiceFinalRequests(_ count: Int) async {
        for _ in 0..<100 where VoiceFinalURLProtocol.requests.count < count {
            try? await Task.sleep(nanoseconds: 1_000_000)
        }
    }

    @MainActor
    func testAuthoritativeUserAudioTurnFinalCommitsAndHandsOffExactlyOnce() async throws {
        VoiceFinalURLProtocol.requests = []
        VoiceFinalURLProtocol.responseHandler = { request in
            let body = try! JSONSerialization.jsonObject(with: VoiceFinalURLProtocol.body(request)!) as! [String: Any]
            let kind = body["kind"] as! String
            return (200, Self.receipt(for: request, kind: kind, resourceType: kind == "voice.turn.commit" ? "voice_turn" : "task", resourceID: kind == "voice.turn.commit" ? "turn-1" : "task-1"))
        }
        let (model, _, epoch) = try finalModel()
        let event = Data(#"{"type":"user_audio_turn.final","turn_id":"turn-1","revision":1,"transcript":"  research Zoe  "}"#.utf8)
        await model.receiveVoiceDataForTesting(event, mediaEpoch: epoch)
        await waitForVoiceFinalRequests(2)
        await model.receiveVoiceDataForTesting(event, mediaEpoch: epoch)
        await Task.yield()
        XCTAssertEqual(VoiceFinalURLProtocol.requests.filter { $0.httpMethod == "POST" }.count, 2)
        XCTAssertTrue(VoiceFinalURLProtocol.requests.compactMap { VoiceFinalURLProtocol.body($0) }.allSatisfy { body in
            let envelope = try! JSONSerialization.jsonObject(with: body) as! [String: Any]
            return envelope["kind"] as? String != "task.create"
        })
        XCTAssertEqual(model.voiceDraft, "")
    }

    @MainActor
    func testNonFinalAndAssistantEventsAreIgnored() async throws {
        VoiceFinalURLProtocol.requests = []
        let (model, _, epoch) = try finalModel()
        for event in [
            #"{"type":"chat_message_delta","transcript":"ignore"}"#,
            #"{"type":"input_audio_transcription.completed","transcript":"ignore"}"#,
            #"{"type":"assistant.output","turn_id":"turn-1","revision":1,"transcript":"ignore"}"#,
            #"{"type":"user_audio_turn.final","turn_id":"turn-1","revision":0,"transcript":"ignore"}"#
        ] {
            await model.receiveVoiceDataForTesting(Data(event.utf8), mediaEpoch: epoch)
        }
        await Task.yield()
        XCTAssertTrue(VoiceFinalURLProtocol.requests.isEmpty)
        XCTAssertEqual(model.voiceDraft, "ignore")
    }

    @MainActor
    func testDuplicateFinalEventDoesNotDispatchDuplicateCommands() async throws {
        VoiceFinalURLProtocol.requests = []
        VoiceFinalURLProtocol.responseHandler = { request in
            let body = try! JSONSerialization.jsonObject(with: VoiceFinalURLProtocol.body(request)!) as! [String: Any]
            let kind = body["kind"] as! String
            return (200, Self.receipt(for: request, kind: kind, resourceType: kind == "voice.turn.commit" ? "voice_turn" : "task", resourceID: "resource-1"))
        }
        let (model, _, epoch) = try finalModel()
        let event = Data(#"{"type":"user_audio_turn.final","turn_id":"turn-dup","revision":1,"transcript":"same"}"#.utf8)
        await model.receiveVoiceDataForTesting(event, mediaEpoch: epoch)
        await model.receiveVoiceDataForTesting(event, mediaEpoch: epoch)
        await waitForVoiceFinalRequests(2)
        XCTAssertEqual(VoiceFinalURLProtocol.requests.filter { $0.httpMethod == "POST" }.count, 2)
    }

    @MainActor
    func testStaleVoiceMediaEpochIgnoresFinalEvent() async throws {
        VoiceFinalURLProtocol.requests = []
        let (model, _, staleEpoch) = try finalModel()
        _ = model.beginVoiceAttemptForTesting()
        let event = Data(#"{"type":"user_audio_turn.final","turn_id":"stale","revision":1,"transcript":"ignore"}"#.utf8)
        await model.receiveVoiceDataForTesting(event, mediaEpoch: staleEpoch)
        await Task.yield()
        XCTAssertTrue(VoiceFinalURLProtocol.requests.isEmpty)
    }

    @MainActor
    func testFailedFinalHandoffPreservesDraft() async throws {
        VoiceFinalURLProtocol.requests = []
        VoiceFinalURLProtocol.responseHandler = { request in
            let body = try! JSONSerialization.jsonObject(with: VoiceFinalURLProtocol.body(request)!) as! [String: Any]
            let kind = body["kind"] as! String
            if kind == "voice.turn.commit" { return (200, Self.receipt(for: request, kind: kind, resourceType: "voice_turn", resourceID: "turn-1")) }
            return (503, Data(#"{"error":{"code":"unavailable","message":"temporary"}}"#.utf8))
        }
        let (model, _, epoch) = try finalModel()
        await model.receiveVoiceDataForTesting(Data(#"{"type":"user_audio_turn.final","turn_id":"turn-fail","revision":1,"transcript":"preserve me"}"#.utf8), mediaEpoch: epoch)
        await waitForVoiceFinalRequests(2)
        XCTAssertEqual(model.voiceDraft, "preserve me")
        XCTAssertEqual(model.voiceDraftRevision, 1)
    }

    private func recordedDiagnostics(for installationID: String) async throws -> [DiagnosticEvent] {
        let queue = try XCTUnwrap(EncryptedDiagnosticQueue(serverURL: try voiceAPI(installationID: installationID).serverURL, installationID: installationID))
        for _ in 0..<100 {
            let events = await queue.take(limit: 20)
            if !events.isEmpty { return events }
            await Task.yield()
        }
        return await queue.take(limit: 20)
    }

    private func recordedMediaLossDiagnostics(for installationID: String) async throws -> [DiagnosticEvent] {
        let queue = try XCTUnwrap(EncryptedDiagnosticQueue(serverURL: try voiceAPI(installationID: installationID).serverURL, installationID: installationID))
        for _ in 0..<100 {
            let events = await queue.take(limit: 20)
            if events.contains(where: { $0.phase == "voice_media_loss" }) { return events }
            await Task.yield()
        }
        return await queue.take(limit: 20)
    }

    @MainActor
    private func wiredTransport(for model: ZoeAppModel, mediaEpoch: UInt64) throws -> LibWebRTCTransport {
        let transport = try LibWebRTCTransport()
        model.wireVoiceTransportForTesting(transport, mediaEpoch: mediaEpoch)
        return transport
    }

    @MainActor
    func testContinuousVoicePreferenceDefaultsOffAndPersists() throws {
        let suite = UserDefaults(suiteName: UUID().uuidString)!
        let first = ZoeAppModel(defaults: suite)
        XCTAssertFalse(first.continuousVoiceEnabled)
        first.setContinuousVoiceEnabled(true)
        let second = ZoeAppModel(defaults: suite)
        XCTAssertTrue(second.continuousVoiceEnabled)
    }

    @MainActor
    func testReconnectAndStoppedStatusesAreRepresentable() {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setContinuousVoiceEnabled(false)
        XCTAssertEqual(model.voiceStatus, .idle)
        XCTAssertNotEqual(model.voiceStatus, .reconnecting(reason: "temporary"))
        XCTAssertNotEqual(model.voiceStatus, .stopped)
    }

    @MainActor
    func testHeartbeatFailureRequiresCurrentEpochAPIAndUncancelledTask() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: IndependentVoiceCredential())
        let otherAPI = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: IndependentVoiceCredential())
        let session = VoiceSessionResponse(voiceSessionId: "same-session", generation: 4, state: "active", answerSdp: "")
        let oldEpoch = model.installVoiceSessionForTesting(session, api: api)
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        var localStops = 0
        var hangups = 0
        model.voiceLocalMediaStopObserverForTesting = { localStops += 1 }
        model.voiceHangupOverrideForTesting = { _ in
            hangups += 1
            return VoiceOperationResponse(ok: true, state: "ended", upstreamTermination: "verified", poolBlocked: false)
        }
        await model.handleVoiceHeartbeatFailureForTesting(sessionID: session.voiceSessionId, api: api, mediaEpoch: oldEpoch, generation: model.connectionEpoch, message: "old epoch")
        await model.handleVoiceHeartbeatFailureForTesting(sessionID: session.voiceSessionId, api: otherAPI, mediaEpoch: epoch, generation: model.connectionEpoch, message: "old API")
        let cancelled = Task { @MainActor in
            withUnsafeCurrentTask { $0?.cancel() }
            await model.handleVoiceHeartbeatFailureForTesting(sessionID: session.voiceSessionId, api: api, mediaEpoch: epoch, generation: model.connectionEpoch, message: "cancelled")
        }
        await cancelled.value
        XCTAssertEqual(localStops, 0)
        XCTAssertEqual(hangups, 0)
        XCTAssertTrue(model.canContinueVoiceAttemptForTesting(epoch))
        guard case .active = model.voiceStatus else { return XCTFail("stale/cancelled failure must leave media active") }

        await model.handleVoiceHeartbeatFailureForTesting(sessionID: session.voiceSessionId, api: api, mediaEpoch: epoch, generation: model.connectionEpoch, message: "current failure")
        XCTAssertEqual(localStops, 1, "positive control must execute the same guarded handler")
        XCTAssertEqual(hangups, 1)
        XCTAssertFalse(model.canContinueVoiceAttemptForTesting(epoch))
        XCTAssertFalse(model.canEndVoiceSession)
        guard case .interrupted = model.voiceStatus else { return XCTFail("current failure must stop local media") }
    }

    @MainActor
    func testFailedHangupIsPersistedForTheNextLaunch() async throws {
        let suite = UserDefaults(suiteName: UUID().uuidString)!
        let first = ZoeAppModel(defaults: suite)
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: IndependentVoiceCredential())
        let session = VoiceSessionResponse(voiceSessionId: "pending-session", generation: 4, state: "active", answerSdp: "", accountLeaseId: "lease", accountLeaseGeneration: 7)
        _ = first.installVoiceSessionForTesting(session, api: api)
        first.voiceHangupOverrideForTesting = { _ in throw ZoeError.http(status: 503, code: "unavailable", message: "temporary", details: nil) }

        await first.endVoiceSession()

        XCTAssertTrue(first.hasPendingVoiceHangupForTesting)
        let second = ZoeAppModel(defaults: suite)
        XCTAssertTrue(second.hasPendingVoiceHangupForTesting, "the unresolved release survives relaunch")
    }

    @MainActor
    func testClientErrorPresentationHidesServerDetails() async {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        await model.refreshVoiceReadiness()
        let visible = String(describing: model.voiceStatus)
        XCTAssertFalse(visible.contains("Server error"))
        XCTAssertFalse(visible.contains("quarantined until an operator"))
    }

    @MainActor
    func testClosedDataChannelEntersMediaLossPathOnceAndRecordsDiagnostic() async throws {
        let installationID = "voice-closed-" + UUID().uuidString
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try voiceAPI(installationID: installationID)
        let session = VoiceSessionResponse(voiceSessionId: "closed-session", generation: 1, state: "active", answerSdp: "")
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        let transport = try wiredTransport(for: model, mediaEpoch: epoch)
        transport.driveDataChannelStateForTesting(.closed)
        for _ in 0..<20 {
            if case .interrupted = model.voiceStatus { break }
            await Task.yield()
        }
        let events = try await recordedMediaLossDiagnostics(for: installationID)
        XCTAssertEqual(events.filter { $0.phase == "voice_media_loss" }.count, 1)
        XCTAssertEqual(events.first?.errorType, "data_channel")
        XCTAssertEqual(events.first?.errorSummary, "closed")
        XCTAssertEqual(events.first?.voiceSessionID, "closed-session")
        XCTAssertEqual(events.first?.voiceGeneration, 1)
        XCTAssertEqual(events.first?.voiceMediaEpoch, epoch)
        XCTAssertEqual(events.first?.voiceDataChannelState, "closed")
        guard case .interrupted = model.voiceStatus else { return XCTFail("closed data channel must stop voice") }
    }

    @MainActor
    func testOpenAndConnectingDataChannelStatesDoNothing() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let epoch = model.beginVoiceAttemptForTesting()
        let transport = try wiredTransport(for: model, mediaEpoch: epoch)
        transport.driveDataChannelStateForTesting(.connecting)
        transport.driveDataChannelStateForTesting(.open)
        await Task.yield()
        XCTAssertEqual(model.voiceStatus, .connecting)
        XCTAssertTrue(model.canContinueVoiceAttemptForTesting(epoch))
    }

    @MainActor
    func testStaleDataChannelStateIsIgnoredWithoutDiagnosticOrStatusChange() async throws {
        let installationID = "voice-stale-" + UUID().uuidString
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try voiceAPI(installationID: installationID)
        let session = VoiceSessionResponse(voiceSessionId: "stale-session", generation: 1, state: "active", answerSdp: "")
        let staleEpoch = model.installVoiceSessionForTesting(session, api: api)
        _ = model.beginVoiceAttemptForTesting()
        let transport = try wiredTransport(for: model, mediaEpoch: staleEpoch)
        transport.driveDataChannelStateForTesting(.closed)
        await Task.yield()
        XCTAssertEqual(model.voiceStatus, .connecting)
        let events = try await recordedMediaLossDiagnostics(for: installationID)
        XCTAssertTrue(events.isEmpty)
    }

    @MainActor
    func testPeerConnectionFailedStateRecordsMediaLossDiagnostic() async throws {
        let installationID = "voice-connection-" + UUID().uuidString
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try voiceAPI(installationID: installationID)
        let session = VoiceSessionResponse(voiceSessionId: "failed-session", generation: 1, state: "active", answerSdp: "")
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        let transport = try wiredTransport(for: model, mediaEpoch: epoch)
        transport.driveConnectionStateForTesting(.failed)
        for _ in 0..<20 {
            if case .interrupted = model.voiceStatus { break }
            await Task.yield()
        }
        let events = try await recordedDiagnostics(for: installationID)
        XCTAssertEqual(events.filter { $0.phase == "voice_media_loss" }.count, 1)
        XCTAssertEqual(events.first?.errorType, "connection")
        XCTAssertEqual(events.first?.errorSummary, "failed")
    }

    @MainActor
    func testContinuousVoiceDataChannelLossSetsReconnectingStatus() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try voiceAPI(installationID: "voice-reconnect-" + UUID().uuidString)
        let session = VoiceSessionResponse(voiceSessionId: "reconnect-session", generation: 1, state: "active", answerSdp: "")
        model.setContinuousVoiceEnabled(true)
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        var observedReconnecting = false
        model.voiceStatusObserverForTesting = { status in
            if case .reconnecting = status { observedReconnecting = true }
        }
        model.voiceHangupOverrideForTesting = { _ in
            return VoiceOperationResponse(ok: true, state: "ended", upstreamTermination: "verified", poolBlocked: false)
        }
        let transport = try wiredTransport(for: model, mediaEpoch: epoch)
        transport.driveDataChannelStateForTesting(.closing)
        await Task.yield()
        XCTAssertTrue(observedReconnecting)
    }

    func testRedactedDiagnosticKeepsOnlyStatusAndCode() {
        let http = RedactedDiagnostic(ZoeError.http(status: 409, code: "upstream_termination_unverified", message: "secret detail", details: nil))
        XCTAssertEqual(http.httpStatus, 409)
        XCTAssertEqual(http.errorSummary, "upstream_termination_unverified")
        XCTAssertFalse(http.errorSummary.contains("secret detail"))
        let local = RedactedDiagnostic(ZoeError.persistence("/Users/someone/private/path"))
        XCTAssertEqual(local.errorSummary, "persistence")
        let other = RedactedDiagnostic(NSError(domain: "NSCocoaErrorDomain", code: 4, userInfo: [NSLocalizedDescriptionKey: "file /private/x"]))
        XCTAssertEqual(other.errorSummary, "NSCocoaErrorDomain#4")
    }

}
