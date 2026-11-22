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
        // Retain diagnostic events for the queue assertions; hangup still succeeds.
        let status = request.url!.path == "/v1/diagnostics/client" ? 503 : 204
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

final class VoiceLifecycleIndependentTests: XCTestCase {
    @MainActor
    func testVoiceEndClosesOnlyMatchingLocalMedia() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: IndependentVoiceCredential())
        let call = try voiceDialFixture("current-call", generation: 4)
        let epoch = model.installVoiceSessionForTesting(call, api: api)
        var stops = 0
        var hangups = 0
        model.voiceLocalMediaStopObserverForTesting = { stops += 1 }
        model.voiceHangupOverrideForTesting = { _ in hangups += 1 }
        let stale = EventEnvelope(schemaVersion: 1, eventId: "end", cursor: "2", type: "voice.end", occurredAt: "now", subjectId: nil, aggregateType: "voice_session", aggregateId: "old-call", aggregateVersion: 3, payload: .object([:]))
        await model.handleEventForTesting(stale)
        let end = Data(#"{"type":"voice.end"}"#.utf8)
        await model.receiveVoiceDataForTesting(end, mediaEpoch: epoch - 1)
        XCTAssertEqual(stops, 0)
        await model.receiveVoiceDataForTesting(end, mediaEpoch: epoch)
        for _ in 0..<20 where model.canEndVoiceSession { await Task.yield() }
        XCTAssertEqual(stops, 1)
        XCTAssertEqual(hangups, 0)
        XCTAssertFalse(model.canEndVoiceSession)
    }

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
    func testFailedHangupIsPersistedForTheNextLaunch() async throws {
        let suite = UserDefaults(suiteName: UUID().uuidString)!
        let first = ZoeAppModel(defaults: suite)
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: IndependentVoiceCredential())
        let session = try voiceDialFixture("pending-session", generation: 4)
        _ = first.installVoiceSessionForTesting(session, api: api)
        first.voiceHangupOverrideForTesting = { _ in throw ZoeError.http(status: 503, code: "unavailable", message: "temporary", details: nil) }

        await first.endVoiceSession()

        XCTAssertTrue(first.hasPendingVoiceHangupForTesting)
        let second = ZoeAppModel(defaults: suite)
        XCTAssertTrue(second.hasPendingVoiceHangupForTesting, "the unresolved release survives relaunch")
        var handles: [String] = []
        second.voiceHangupOverrideForTesting = { handles.append($0) }
        await second.retryPendingVoiceHangupForTesting(api: api, origin: URL(string: "https://other.example")!)
        XCTAssertTrue(handles.isEmpty, "do not replay an old handle to a different origin")
        await second.retryPendingVoiceHangupForTesting(api: api)
        XCTAssertEqual(handles, [session.gatewaySessionId])
        XCTAssertFalse(second.hasPendingVoiceHangupForTesting)
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
        let session = try voiceDialFixture("closed-session", generation: 1)
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
        let session = try voiceDialFixture("stale-session", generation: 1)
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
        let session = try voiceDialFixture("failed-session", generation: 1)
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
        let session = try voiceDialFixture("reconnect-session", generation: 1)
        model.setContinuousVoiceEnabled(true)
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        var observedReconnecting = false
        model.voiceStatusObserverForTesting = { status in
            if case .reconnecting = status { observedReconnecting = true }
        }
        model.voiceHangupOverrideForTesting = { _ in }
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
