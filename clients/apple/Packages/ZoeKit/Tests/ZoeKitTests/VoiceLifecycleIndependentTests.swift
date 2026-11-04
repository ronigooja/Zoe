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

    private func recordedDiagnostics(for installationID: String) async throws -> [DiagnosticEvent] {
        let queue = try XCTUnwrap(EncryptedDiagnosticQueue(serverURL: URL(string: "https://voice-independent.example")!, installationID: installationID))
        for _ in 0..<20 {
            let events = await queue.take(limit: 20)
            if !events.isEmpty { return events }
            await Task.yield()
        }
        return await queue.take(limit: 20)
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
        await model.receiveDataChannelStateForTesting(.closed, mediaEpoch: epoch)
        await model.receiveDataChannelStateForTesting(.closed, mediaEpoch: epoch)
        let events = try await recordedDiagnostics(for: installationID)
        XCTAssertEqual(events.filter { $0.phase == "voice_media_loss" }.count, 1)
        XCTAssertEqual(events.first?.errorType, "data_channel")
        XCTAssertEqual(events.first?.errorSummary, "closed")
        guard case .interrupted = model.voiceStatus else { return XCTFail("closed data channel must stop voice") }
    }

    @MainActor
    func testOpenAndConnectingDataChannelStatesDoNothing() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let epoch = model.beginVoiceAttemptForTesting()
        await model.receiveDataChannelStateForTesting(.connecting, mediaEpoch: epoch)
        await model.receiveDataChannelStateForTesting(.open, mediaEpoch: epoch)
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
        await model.receiveDataChannelStateForTesting(.closed, mediaEpoch: staleEpoch)
        XCTAssertEqual(model.voiceStatus, .connecting)
        let events = try await recordedDiagnostics(for: installationID)
        XCTAssertTrue(events.isEmpty)
    }

    @MainActor
    func testPeerConnectionFailedStateRecordsMediaLossDiagnostic() async throws {
        let installationID = "voice-connection-" + UUID().uuidString
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try voiceAPI(installationID: installationID)
        let session = VoiceSessionResponse(voiceSessionId: "failed-session", generation: 1, state: "active", answerSdp: "")
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        await model.receiveConnectionStateForTesting(.failed, mediaEpoch: epoch)
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
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        model.setContinuousVoiceEnabled(true)
        model.voiceHangupOverrideForTesting = { _ in
            try await Task.sleep(for: .milliseconds(100))
            return VoiceOperationResponse(ok: true, state: "ended", upstreamTermination: "verified", poolBlocked: false)
        }
        let loss = Task { @MainActor in
            await model.receiveDataChannelStateForTesting(.closing, mediaEpoch: epoch)
        }
        for _ in 0..<20 {
            if case .reconnecting = model.voiceStatus { break }
            await Task.yield()
        }
        XCTAssertTrue({ if case .reconnecting = model.voiceStatus { return true }; return false }())
        await loss.value
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
