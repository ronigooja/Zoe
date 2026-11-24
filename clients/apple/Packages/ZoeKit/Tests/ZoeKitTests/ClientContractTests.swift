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

private struct VoiceFixtureCredential: BearerTokenProviding {
    func bearerToken() throws -> String { "synthetic-instance-key" }
}

private final class VoiceContractSilenceClock: SilenceClock {
    var date = Date(timeIntervalSince1970: 0)
    func now() -> Date { date }
}

private struct VoiceContractKeyStore: InstanceKeyStoring {
    func load() throws -> InstanceKey? { try InstanceKey(String(repeating: "A", count: 43)) }
    func replace(_ key: InstanceKey) throws {}
    func reset() throws {}
    func bearerToken() throws -> String { try XCTUnwrap(load()).bearerToken() }
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
    @MainActor private func isolatedJournal(_ model: ZoeAppModel) -> ZoeAppModel {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        addTeardownBlock {
            await model.waitForRecoveryACKForTesting()
            try? FileManager.default.removeItem(at: directory)
        }
        model.journalFactoryForTesting = { identity in
            let name = identity.utf8.map { String(format: "%02x", $0) }.joined()
            return try CommandJournal(fileURL: directory.appendingPathComponent(name + ".json"))
        }
        return model
    }

    @MainActor func testSilenceTimeoutRequestsGatewayHangupBeforeClosingLocalMedia() async throws {
        let clock = VoiceContractSilenceClock()
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!, silenceClock: clock)
        let api = try voiceContractAPI()
        let call = try voiceDialFixture("zoe-id", generation: 4, handle: "gateway-handle")
        model.installVoiceSessionForTesting(call, api: api, origin: URL(string: "https://gateway.example:8443")!)
        var events: [String] = []
        model.voiceLocalMediaStopObserverForTesting = { events.append("local_close") }
        ContractURLProtocol.responseHandler = { request in
            XCTAssertEqual(request.url?.absoluteString, "https://gateway.example:8443/client/v1/voice/calls/gateway-handle/hangup")
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer synthetic-instance-key")
            XCTAssertTrue((Self.requestBody(request) ?? Data()).isEmpty)
            events.append("gateway_hangup")
            return (204, [:], Data())
        }
        clock.date = Date(timeIntervalSince1970: 59)
        await model.tickVoiceSilenceForTesting()
        XCTAssertTrue(events.isEmpty)
        clock.date = Date(timeIntervalSince1970: 60)
        await model.tickVoiceSilenceForTesting()
        await model.tickVoiceSilenceForTesting()
        XCTAssertEqual(events, ["gateway_hangup", "local_close"])
        XCTAssertEqual(model.voiceStatus, .idle)
    }

    private static let voiceConfigBody = #"{"version":"v1","gateway_dial_origin":"https://gateway.example:8443","defaults":{"voice":"cove","voice_mode":"wingman","language_code":"auto"},"voices":[{"id":"cove","name":"Cove"}],"voice_modes":[],"languages":[],"webrtc":{"data_channel":{"label":"zoe","negotiated":true,"id":0},"receive_audio":true,"receive_video":false}}"#
    private static let voicePoolBody = #"{"blocked":true,"reason_code":"voice_pool_quarantined","updated_at":"now","account_lease_identity":false,"account_scoped_termination":false,"trusted_termination_evidence":false}"#
    private static let dialAnswer = Data(#"{"voice_session_id":"zoe-call","gateway_session_id":"gateway-call","generation":4,"answer_sdp":"answer"}"#.utf8)

    private func voiceContractAPI() throws -> APIClient {
        try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: VoiceFixtureCredential(), session: session())
    }

    @MainActor private func voiceContractModel(api: APIClient, clock: SilenceClock = SystemSilenceClock()) -> ZoeAppModel {
        let defaults = UserDefaults(suiteName: UUID().uuidString)!
        defaults.set(api.serverURL.absoluteString, forKey: "zoe.instance.serverAddress")
        let model = ZoeAppModel(defaults: defaults, silenceClock: clock)
        model.keyStoreOverrideForTesting = { _ in VoiceContractKeyStore() }
        model.connectionAPIOverrideForTesting = { _, _, _ in api }
        model.voiceOfferOverrideForTesting = { "offer" }
        model.voiceAnswerOverrideForTesting = { XCTAssertEqual($0, "answer") }
        return model
    }

    func testVoiceRequestsDisableCookiesAndCachingWithInjectedProtocol() async throws {
        let api = try voiceContractAPI()
        let origin = URL(string: "https://gateway.example:8443")!
        ContractURLProtocol.responseHandler = { request in
            XCTAssertFalse(request.httpShouldHandleCookies)
            XCTAssertEqual(request.cachePolicy, .reloadIgnoringLocalCacheData)
            XCTAssertEqual(request.value(forHTTPHeaderField: "Cache-Control"), "no-store")
            XCTAssertNil(request.value(forHTTPHeaderField: "Cookie"))
            let headers = ["Set-Cookie": "voice-fixture=ignored; Path=/"]
            switch request.url?.path {
            case "/v1/voice/config": return (200, headers, Data(Self.voiceConfigBody.utf8))
            case "/v1/voice/pool/status": return (200, headers, Data(Self.voicePoolBody.utf8))
            case "/client/v1/voice/calls": return (200, headers, Self.dialAnswer)
            case "/client/v1/voice/calls/gateway-call/hangup": return (204, [:], Data())
            default: XCTFail("Unexpected request"); return (404, [:], Data())
            }
        }
        _ = try await api.voiceConfig()
        _ = try await api.voicePoolStatus()
        let call = try await api.dialVoiceCall(origin: origin, payload: VoiceDialRequest(requestID: "call", offerSDP: "offer"))
        try await api.hangupVoiceCall(origin: origin, gatewaySessionID: call.gatewaySessionId)
    }

    @MainActor func testEarlyVoiceEndClosesMediaBeforeHandshakeCanPublishActiveCall() async throws {
        let model = voiceContractModel(api: try voiceContractAPI())
        var stops = 0
        model.voiceLocalMediaStopObserverForTesting = { stops += 1 }
        ContractURLProtocol.responseHandler = Self.negotiationResponse
        model.voiceAnswerOverrideForTesting = { _ in
            await model.receiveVoiceDataForTesting(Data(#"{"type":"voice.end"}"#.utf8), mediaEpoch: 0)
            XCTAssertEqual(stops, 0, "stale media cannot end the call in progress")
            await model.receiveVoiceDataForTesting(Data(#"{"type":"voice.end"}"#.utf8))
            XCTAssertEqual(stops, 1, "end must close even before the handshake resumes")
        }
        defer { model.voiceAnswerOverrideForTesting = nil }
        await model.startVoice()
        XCTAssertEqual(model.voiceStatus, .idle)
        XCTAssertFalse(model.canEndVoiceSession)
        XCTAssertFalse(model.hasPendingVoiceHangupForTesting)
    }

    @MainActor func testEarlyActivityIsRetainedAndNotCountedAgainAfterConnect() async throws {
        let clock = VoiceContractSilenceClock()
        let model = voiceContractModel(api: try voiceContractAPI(), clock: clock)
        ContractURLProtocol.responseHandler = Self.negotiationResponse
        let activity = Data(#"{"type":"input_transcript.added","turn_id":"early","event_id":"first","item":{"text":"Hello"}}"#.utf8)
        model.voiceAnswerOverrideForTesting = { _ in
            clock.date = Date(timeIntervalSince1970: 5)
            await model.receiveVoiceDataForTesting(activity)
            XCTAssertEqual(model.silence.lastActivityAge, 0)
            clock.date = Date(timeIntervalSince1970: 8)
        }
        defer { model.voiceAnswerOverrideForTesting = nil }
        await model.startVoice()
        XCTAssertEqual(model.voiceStatus, .active(sessionID: "zoe-call", mediaGeneration: 4))
        XCTAssertEqual(model.silence.lastActivityAge, 3)
        await model.receiveVoiceDataForTesting(activity)
        XCTAssertEqual(model.silence.lastActivityAge, 3)
    }

    private static func negotiationResponse(_ request: URLRequest) -> (Int, [String: String], Data) {
        switch request.url?.path {
        case "/v1/voice/config": return (200, [:], Data(voiceConfigBody.utf8))
        case "/v1/voice/pool/status": return (200, [:], Data(voicePoolBody.utf8))
        case "/client/v1/voice/calls": return (200, [:], dialAnswer)
        default: XCTFail("Unexpected negotiation request: \(request.url?.path ?? "")"); return (404, [:], Data())
        }
    }

    func testVoiceConfigRejectsMissingOrInvalidOriginWithoutFallback() async throws {
        let api = try voiceContractAPI()
        for body in [Self.voiceConfigBody.replacingOccurrences(of: #""gateway_dial_origin":"https://gateway.example:8443","#, with: ""),
                     Self.voiceConfigBody.replacingOccurrences(of: "https://gateway.example:8443", with: "http://gateway.example"),
                     Self.voiceConfigBody.replacingOccurrences(of: #""https://gateway.example:8443""#, with: "123")] {
            ContractURLProtocol.responseHandler = { request in
                XCTAssertEqual(request.url?.absoluteString, "https://zoe.example/v1/voice/config")
                XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer synthetic-instance-key")
                return (200, [:], Data(body.utf8))
            }
            do {
                _ = try await api.voiceConfig()
                XCTFail("A missing or invalid origin must fail closed")
            } catch let error as ZoeError {
                guard case let .http(status, code, _, _) = error else { return XCTFail("Unexpected config error: \(error)") }
                XCTAssertEqual(status, 503)
                XCTAssertEqual(code, "voice_unavailable")
            }
        }
    }

    func testDialMapsWireFieldsAndRetriesExactRequestAfterTransportLoss() async throws {
        let api = try voiceContractAPI()
        let origin = URL(string: "https://gateway.example:8443")!
        var requests: [Data] = []
        ContractURLProtocol.failureHandler = { request in
            XCTAssertEqual(request.url?.absoluteString, "https://gateway.example:8443/client/v1/voice/calls")
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer synthetic-instance-key")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Content-Type"), "application/json")
            requests.append(Self.requestBody(request) ?? Data())
            return requests.count == 1 ? URLError(.timedOut) : nil
        }
        defer { ContractURLProtocol.failureHandler = nil }
        ContractURLProtocol.responseHandler = { _ in (200, [:], Self.dialAnswer) }
        let response = try await api.dialVoiceCall(origin: origin, payload: VoiceDialRequest(requestID: "stable", offerSDP: "offer", voice: "cove", voiceMode: "wingman", languageCode: "auto"))
        XCTAssertEqual(response.voiceSessionId, "zoe-call")
        XCTAssertEqual(response.gatewaySessionId, "gateway-call")
        XCTAssertEqual(response.generation, 4)
        XCTAssertEqual(response.answerSdp, "answer")
        XCTAssertEqual(requests.count, 2)
        XCTAssertEqual(requests.first, requests.last)
        let first = try XCTUnwrap(requests.first)
        let payload = try JSONSerialization.jsonObject(with: first) as? [String: String]
        XCTAssertEqual(payload, ["request_id": "stable", "offer_sdp": "offer", "voice": "cove", "voice_mode": "wingman", "language_code": "auto"])
    }

    func testHangupEncodesOpaqueHandleAsOnePathSegmentAndRequires204() async throws {
        let api = try voiceContractAPI()
        let origin = URL(string: "https://gateway.example")!
        for status in [200, 204, 401, 404, 503] {
            let code: String = switch status {
            case 401: "unauthenticated"
            case 404: "not_found"
            default: "voice_unavailable"
            }
            ContractURLProtocol.responseHandler = { request in
                XCTAssertEqual(request.url?.absoluteString, "https://gateway.example/client/v1/voice/calls/id%2Fwith%3Freserved%23chars/hangup")
                XCTAssertEqual(request.httpMethod, "POST")
                XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer synthetic-instance-key")
                XCTAssertNil(request.value(forHTTPHeaderField: "Content-Type"))
                XCTAssertTrue((Self.requestBody(request) ?? Data()).isEmpty)
                let data = status == 204 ? Data() : Data("{\"error\":{\"code\":\"\(code)\",\"message\":\"private\"}}".utf8)
                return (status, [:], data)
            }
            do {
                try await api.hangupVoiceCall(origin: origin, gatewaySessionID: "id/with?reserved#chars")
                XCTAssertEqual(status, 204)
            } catch let error as ZoeError {
                switch error {
                case .invalidResponse: XCTAssertEqual(status, 200)
                case let .http(actualStatus, actualCode, _, _):
                    XCTAssertEqual(actualStatus, status)
                    XCTAssertEqual(actualCode, code)
                default: XCTFail("Unexpected hangup error: \(error)")
                }
            }
        }
    }

    @MainActor func testDialMapsDocumentedErrorsWithoutReplayingHTTPFailures() async throws {
        let api = try voiceContractAPI()
        let origin = URL(string: "https://gateway.example")!
        for (status, code, visible) in [
            (400, "invalid_request", "The voice settings or call request were not accepted."),
            (401, "unauthenticated", "The instance key was not accepted for voice calls."),
            (409, "voice_lease_busy", "Another voice call is active or still closing."),
            (409, "idempotency_conflict", "This voice call could not be retried. Start a new call."),
            (503, "voice_pool_quarantined", "The voice account pool is quarantined."),
            (503, "voice_unavailable", "Voice is unavailable right now.")
        ] {
            var count = 0
            ContractURLProtocol.responseHandler = { request in
                count += 1
                let payload = (try? JSONSerialization.jsonObject(with: Self.requestBody(request) ?? Data())) as? [String: String]
                XCTAssertEqual(payload, ["request_id": "call", "offer_sdp": "offer"])
                return (status, [:], Data("{\"error\":{\"code\":\"\(code)\",\"message\":\"private diagnostic\"}}".utf8))
            }
            do {
                _ = try await api.dialVoiceCall(origin: origin, payload: VoiceDialRequest(requestID: "call", offerSDP: "offer"))
                XCTFail("Expected dial failure")
            } catch let error as ZoeError {
                guard case let .http(actualStatus, actualCode, message, _) = error else { return XCTFail("Unexpected error: \(error)") }
                XCTAssertEqual(actualStatus, status)
                XCTAssertEqual(actualCode, code)
                XCTAssertFalse(message.contains("private diagnostic"))
                XCTAssertEqual(ZoeAppModel.voiceDialFailureMessage(error), visible)
            }
            XCTAssertEqual(count, 1)
        }
    }

    @MainActor func testLateGatewayDialOnlyHangsUpItsOwnHandle() async throws {
        let api = try voiceContractAPI()
        let model = voiceContractModel(api: api)
        let gate = AsyncTestGate()
        ContractURLProtocol.responseGate = { request in
            if request.url?.path == "/client/v1/voice/calls" { await gate.pause() }
        }
        defer { ContractURLProtocol.responseGate = nil }
        var hangups = 0
        ContractURLProtocol.responseHandler = { request in
            switch request.url?.path {
            case "/v1/voice/config": return (200, [:], Data(Self.voiceConfigBody.utf8))
            case "/v1/voice/pool/status": return (200, [:], Data(Self.voicePoolBody.utf8))
            case "/client/v1/voice/calls": return (200, [:], Self.dialAnswer)
            case "/client/v1/voice/calls/gateway-call/hangup":
                XCTAssertEqual(request.url?.host, "gateway.example")
                XCTAssertEqual(request.url?.port, 8443)
                XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer synthetic-instance-key")
                hangups += 1
                return (204, [:], Data())
            default: XCTFail("Unexpected request: \(request.url?.path ?? "")"); return (404, [:], Data())
            }
        }
        model.voiceAnswerOverrideForTesting = { _ in XCTFail("Stale answer must not be applied") }
        let dial = Task { await model.startVoice() }
        await gate.waitUntilStarted()
        await model.endVoiceSession()
        let replacement = try voiceDialFixture("new-call", generation: 5)
        model.installVoiceSessionForTesting(replacement, api: api)
        await gate.resume()
        await dial.value
        XCTAssertEqual(hangups, 1)
        XCTAssertEqual(model.voiceStatus, .active(sessionID: "new-call", mediaGeneration: 5))
        XCTAssertFalse(model.hasPendingVoiceHangupForTesting)
    }

    @MainActor func testGatewayLifecycleNeverPostsLegacyRoutesOrRelaysVoiceTurns() async throws {
        let api = try voiceContractAPI()
        let model = voiceContractModel(api: api)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        for kind in ["voice.turn.commit", "voice.turn.handoff"] {
            try await journal.append(PendingCommand(idempotencyKey: kind, kind: kind, expectedVersion: nil, payload: .object([:]), createdAt: "now", instanceLineageID: "default", expectedResourceType: nil, expectedResourceID: nil))
        }
        let coordinator = CommandCoordinator(api: api, journal: journal)
        var requestIDs: [String] = []
        ContractURLProtocol.responseHandler = { request in
            switch request.url?.path {
            case "/v1/voice/config":
                XCTAssertEqual(request.url?.host, "zoe.example")
                return (200, [:], Data(Self.voiceConfigBody.utf8))
            case "/v1/voice/pool/status":
                XCTAssertEqual(request.url?.host, "zoe.example")
                return (200, [:], Data(Self.voicePoolBody.utf8))
            case "/client/v1/voice/calls":
                XCTAssertEqual(request.url?.host, "gateway.example")
                let payload = (try? JSONSerialization.jsonObject(with: Self.requestBody(request) ?? Data())) as? [String: String]
                requestIDs.append(payload?["request_id"] ?? "")
                return (200, [:], Self.dialAnswer)
            case "/client/v1/voice/calls/gateway-call/hangup":
                XCTAssertEqual(request.url?.host, "gateway.example")
                return (204, [:], Data())
            default: XCTFail("Unexpected request: \(request.url?.path ?? "")"); return (404, [:], Data())
            }
        }
        // Legacy journal rows must not be looked up or replayed after the upgrade.
        let outcomes = try await coordinator.recoverPending(currentInstanceLineageID: "default", onOutcome: { _ in })
        XCTAssertTrue(outcomes.isEmpty)
        do {
            _ = try await coordinator.retryPending(idempotencyKey: "voice.turn.handoff", currentInstanceLineageID: "default")
            XCTFail("Explicit retry must not replay a retired command")
        } catch ZoeError.persistence(_) {}
        for _ in 0..<2 {
            await model.startVoice()
            XCTAssertEqual(model.voiceStatus, .active(sessionID: "zoe-call", mediaGeneration: 4))
            model.configureVoiceCoordinatorForTesting(api: api, journal: journal)
            await model.receiveVoiceDataForTesting(Data(#"{"type":"user_audio_turn.final","turn_id":"obsolete","revision":1,"transcript":"ignored"}"#.utf8))
            await model.endVoiceSession()
            XCTAssertEqual(model.voiceStatus, .idle)
            XCTAssertTrue(model.voicePoolStatus?.blocked == true, "204 alone says nothing about pool reuse")
        }
        XCTAssertEqual(requestIDs.count, 2)
        XCTAssertEqual(Set(requestIDs).count, 2, "each new call gets a fresh request ID")
        XCTAssertFalse(requestIDs.contains(""))
    }
    private static func recoveryBody(cursor: String) -> Data {
        Data("""
        {"cursor":"\(cursor)","snapshot":{"subject_id":"owner","server_version":"1","event_cursor":"\(cursor)","authority":{"instance_lineage_id":"default","generation":1,"deletion_sequence":"0"},"projects":[],"tasks":[{"id":"authoritative-task","status":"complete"}],"approvals":[],"runs":[],"execution_capacity":{}},"ui":{"cursor":\(cursor),"surfaces":[]},"voice":{"current_session":null,"pool":{"blocked":false,"reason_code":""}}}
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
        let model = isolatedJournal(ZoeAppModel())
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "1", authority: InstanceAuthority(instanceLineageId: "default")))
        model.setAuthenticationForTesting(.connected)
        _ = model.installVoiceSessionForTesting(try voiceDialFixture("replayed-call", generation: 1), api: api)
        var mediaStops = 0
        var hangups = 0
        model.voiceLocalMediaStopObserverForTesting = { mediaStops += 1 }
        model.voiceHangupOverrideForTesting = { _ in
            hangups += 1
        }
        func envelope(_ cursor: Int64, _ type: String, payload: String = "{}", version: Int64 = 1) -> String {
            "{\"schema_version\":1,\"event_id\":\"e\(cursor)\",\"cursor\":\"\(cursor)\",\"type\":\"\(type)\",\"occurred_at\":\"\",\"aggregate_type\":\"\(type == "voice.end" ? "voice_session" : "ui_surface")\",\"aggregate_id\":\"\(type == "voice.end" ? "replayed-call" : "s1")\",\"aggregate_version\":\(version),\"payload\":\(payload)}"
        }
        func frame(_ cursor: Int64, _ type: String, payload: String = "{}", version: Int64 = 1) -> String {
            "id: \(cursor)\nevent: \(type)\ndata: \(envelope(cursor, type, payload: payload, version: version))\n\n"
        }
        let surface = #"{"request_id":"r1","surface_id":"s1","surface_version":2,"container":"window","chat_input":false,"task_question":true,"task_id":"task","question_id":"question","messages":[{"version":"v0.9","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}]}"#
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
        XCTAssertEqual(hangups, 0, "Replayed voice.end must not request another gateway close")
        guard case .idle = model.voiceStatus else { return XCTFail("Confirmed voice shutdown must finish idle") }
        XCTAssertEqual(renderCount, 1, "Snapshot revision must suppress its obsolete SSE replay")
        XCTAssertEqual(model.eventCursorForTesting, "13", "Event racing snapshot12 remains consumed")
    }

    @MainActor func testExpiredGlobalHistoryStopsWithoutAdvancingToUISnapshotCursor() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = isolatedJournal(ZoeAppModel())
        model.setAuthenticationForTesting(.connected)
        model.setSnapshotForTesting(Snapshot(serverVersion: "test", eventCursor: "10", authority: InstanceAuthority(instanceLineageId: "default")))
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
        let model = isolatedJournal(ZoeAppModel())
        model.setAuthenticationForTesting(.connected)
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "10", authority: InstanceAuthority(instanceLineageId: "default")))
        _ = model.installVoiceSessionForTesting(try voiceDialFixture("old", generation: 1), api: api)
        var stops = 0
        var hangups = 0
        model.voiceLocalMediaStopObserverForTesting = { stops += 1 }
        model.voiceHangupOverrideForTesting = { _ in
            hangups += 1
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
            let event = #"{"schema_version":1,"event_id":"end","cursor":"21","type":"voice.end","occurred_at":"","aggregate_type":"voice_session","aggregate_id":"old","aggregate_version":1,"payload":{}}"#
            return (200, ["Content-Type": "text/event-stream"], Data("id: 21\nevent: voice.end\ndata: \(event)\n\n".utf8))
        }
        try await model.receiveEventConnectionForTesting(api: api)
        XCTAssertEqual(model.eventCursorForTesting, "10")
        try await model.receiveEventConnectionForTesting(api: api)
        XCTAssertFalse(model.eventRecoveryRequired)
        XCTAssertEqual(model.eventCursorForTesting, "21")
        XCTAssertEqual(model.authoritativeControlSnapshot?.tasks.count, 1)
        XCTAssertEqual(stops, 1, "Post-checkpoint voice.end must close actual local media")
        XCTAssertEqual(hangups, 0, "Post-checkpoint voice.end must not request another gateway close")
        guard case .idle = model.voiceStatus else { return XCTFail("Confirmed post-checkpoint voice shutdown must finish idle") }
        XCTAssertTrue(paths.contains("/v1/recovery"))
    }

    @MainActor func testInitialZeroHistoryUsesAuthoritativeBaselineBeforeSSE() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = isolatedJournal(ZoeAppModel())
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
        let model = isolatedJournal(ZoeAppModel())
        model.setAuthenticationForTesting(.connected)
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "0", authority: InstanceAuthority(instanceLineageId: "default")))
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
        let model = isolatedJournal(ZoeAppModel())
        model.setAuthenticationForTesting(.connected)
        _ = model.installVoiceSessionForTesting(try voiceDialFixture("local", generation: 3), api: api)
        var hangups = 0
        model.voiceHangupOverrideForTesting = { _ in
            hangups += 1
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
        let model = isolatedJournal(ZoeAppModel())
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "10", authority: InstanceAuthority(instanceLineageId: "default")))
        var renders = 0
        model.onUISurface = { _ in renders += 1; return renders > 1 }
        let surface = #"{"request_id":"pending","surface_id":"s1","surface_version":2,"container":"window","chat_input":false,"task_question":true,"task_id":"task","question_id":"question","render_status":"rendered","messages":[{"version":"v0.9","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}]}"#
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
        let model = isolatedJournal(ZoeAppModel())
        model.setAuthenticationForTesting(.connected)
        var renders = 0
        model.onUISurface = { _ in renders += 1; return true }
        let surface = #"{"request_id":"old","surface_id":"s1","surface_version":1,"container":"window","chat_input":false,"task_question":false,"render_status":"rendered","messages":[{"version":"v0.9","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}]}"#
        let body = String(data: Self.recoveryBody(cursor: "20"), encoding: .utf8)!
            .replacingOccurrences(of: "\"surfaces\":[]", with: "\"surfaces\":[\(surface)]")
        ContractURLProtocol.responseHandler = { _ in (200, [:], Data(body.utf8)) }
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(renders, 0)
        XCTAssertEqual(model.eventCursorForTesting, "20")
        let live = surface.replacingOccurrences(of: "\"surface_version\":1", with: "\"surface_version\":2")
        let payload = try JSONDecoder().decode(JSONValue.self, from: Data(live.utf8))
        await model.handleEventForTesting(EventEnvelope(schemaVersion: 1, eventId: "live", cursor: "21", type: "ui.surface", occurredAt: "", aggregateType: "ui_surface", aggregateId: "s1", aggregateVersion: 2, payload: payload))
        XCTAssertEqual(renders, 1)
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(renders, 1, "Reconnect must not hide or recreate the already-presented live surface")
    }

    @MainActor func testRecoveryWithoutDeletedPresentsPendingQuestionOnLaunch() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let model = isolatedJournal(ZoeAppModel())
        var renders = 0
        model.onUISurface = { event in
            XCTAssertTrue(event.taskQuestion)
            XCTAssertEqual(event.taskID, "task")
            XCTAssertEqual(event.questionID, "question")
            renders += 1
            return true
        }
        let surface = #"{"request_id":"pending","surface_id":"s1","surface_version":1,"container":"popover","chat_input":false,"task_question":true,"task_id":"task","question_id":"question","render_status":"rendered","messages":[{"version":"v0.9","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}}]}"#
        let body = String(data: Self.recoveryBody(cursor: "20"), encoding: .utf8)!
            .replacingOccurrences(of: "\"surfaces\":[]", with: "\"surfaces\":[\(surface)]")
        ContractURLProtocol.responseHandler = { _ in (200, [:], Data(body.utf8)) }
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(renders, 1)
        try await model.recoverGlobalStateForTesting(api: api)
        XCTAssertEqual(renders, 1)
        await model.waitForRecoveryACKForTesting()
    }

    #if os(macOS)
    @MainActor func testNativeSurfaceWithoutDeletedPresentsFromEventAndSnapshotAndACKReachesTransport() async throws {
        _ = NSApplication.shared
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("journal.json"))
        let model = isolatedJournal(ZoeAppModel())
        let manager = SurfaceManager(popoverAnchor: { nil })
        defer { manager.reset() }
        model.installRecoveryCoordinatorForTesting(CommandCoordinator(api: api, journal: journal))
        model.setAuthenticationForTesting(.connected)
        model.onUISurface = { manager.display($0) }
        model.isCurrentUIPresentation = { manager.isCurrentPresentation($0) }
        model.onUISurfacesClosed = { ids in ids.forEach { manager.close(id: $0) } }
        let live = #"{"request_id":"live","surface_id":"s1","surface_version":1,"container":"window","chat_input":false,"task_question":false,"messages":[{"version":"v0.9","createSurface":{"surfaceId":"s1","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}},{"version":"v0.9","updateComponents":{"surfaceId":"s1","components":[{"id":"root","component":"Text","text":"Hello"}]}}]}"#
        let payload = try JSONDecoder().decode(JSONValue.self, from: Data(live.utf8))
        await model.handleEventForTesting(EventEnvelope(schemaVersion: 1, eventId: "live", cursor: "10", type: "ui.surface", occurredAt: "", aggregateType: "ui_surface", aggregateId: "s1", aggregateVersion: 1, payload: payload))
        XCTAssertNotNil(manager.viewModel(for: "s1"))
        let pending = #"{"request_id":"pending","surface_id":"s2","surface_version":1,"container":"window","chat_input":false,"task_question":true,"task_id":"task","question_id":"question","render_status":"rendered","messages":[{"version":"v0.9","createSurface":{"surfaceId":"s2","catalogId":"https://a2ui.org/specification/v0_9/basic_catalog.json"}},{"version":"v0.9","updateComponents":{"surfaceId":"s2","components":[{"id":"root","component":"Text","text":"Question"}]}}]}"#
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
        let model = isolatedJournal(ZoeAppModel())
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "10", authority: InstanceAuthority(instanceLineageId: "default")))
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
            _ = try await coordinator.submit(kind: "ui.rendered", payload: JSONValue.object([:]), instanceLineageID: "default", idempotencyKey: "pending-render")
        } catch { }
        let model = isolatedJournal(ZoeAppModel())
        model.installRecoveryCoordinatorForTesting(coordinator)
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "10", authority: InstanceAuthority(instanceLineageId: "default")))
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
            let model = isolatedJournal(ZoeAppModel())
            model.setAuthenticationForTesting(.connected)
            _ = model.installVoiceSessionForTesting(try voiceDialFixture("local", generation: 3), api: api)
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
        let model = isolatedJournal(ZoeAppModel())
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "0", authority: InstanceAuthority(instanceLineageId: "default")))
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
        do { _ = try await coordinator.submit(kind: "ui.rendered", payload: .object([:]), instanceLineageID: "default", idempotencyKey: "old") } catch { }
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
            try await coordinator.recoverPending(currentInstanceLineageID: "default", onOutcome: { _ in }, isValid: { valid })
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
                return (200, ["Content-Type": "application/json"], Self.commandReceipt(key: key, kind: kind, resourceID: "s1"))
            }
            do {
                _ = try await coordinator.submit(kind: kind, payload: payload, instanceLineageID: "instance-1", idempotencyKey: key)
                XCTFail("Expected uncertain acceptance")
            } catch { }
            let pending = try await journal.all()
            XCTAssertEqual(pending.first?.payload, payload)
            let outcomes = try await coordinator.recoverPending(currentInstanceLineageID: "instance-1", onOutcome: { _ in })
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
            _ = try await coordinator.submit(kind: "voice.text_input", payload: payload, instanceLineageID: "instance-1", idempotencyKey: key)
            XCTFail("Expected an uncertain submission")
        } catch { }
        let pending = try await journal.all()
        XCTAssertEqual(pending.first?.kind, "voice.text_input")
        XCTAssertEqual(pending.first?.payload, payload)
        ContractURLProtocol.failureHandler = nil
        ContractURLProtocol.responseHandler = { request in
            if request.httpMethod == "GET" { return (404, [:], Data(#"{"error":{"code":"not_found","message":"missing"}}"#.utf8)) }
            return (200, ["Content-Type": "application/json"], Self.commandReceipt(key: key, kind: "voice.text_input", resourceID: ""))
        }
        let outcomes = try await coordinator.recoverPending(currentInstanceLineageID: "instance-1", onOutcome: { _ in })
        XCTAssertEqual(outcomes.count, 1)
        XCTAssertFalse(try XCTUnwrap(outcomes.first).recovered)
        let remaining = try await journal.all()
        XCTAssertTrue(remaining.isEmpty)
    }

    func testSSEParserCombinesDataLinesAndPreservesDecimalCursor() throws {
        let payload = #"{"schema_version":1,"event_id":"ev-7","cursor":"9007199254740993","type":"task.updated","occurred_at":"2026-10-20T12:00:00Z","aggregate_type":"task","aggregate_id":"task-1","aggregate_version":5,"payload":{"status":"active"}}"#
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
        let payload = #"{"schema_version":1,"event_id":"ev-8","cursor":"20","type":"task.updated","occurred_at":"2026-10-20T12:00:00Z","aggregate_type":"task","aggregate_id":"task-1","aggregate_version":6,"payload":{}}"#
        var parser = SSEParser()
        _ = try parser.consume(line: "id: 20")
        _ = try parser.consume(line: "event: task.updated")
        _ = try parser.consume(line: "data: \(payload)")
        XCTAssertNil(try parser.finish(), "An SSE frame without its record boundary must not advance the event cursor.")
    }

    func testCommandReceiptDecodesOmittedResourceMetadata() throws {
        let data = Data(#"{"command_id":"cmd-1","idempotency_key":"key-1","kind":"voice.text_input","status":"accepted","accepted_at":"2026-10-20T12:00:00Z","event_cursor":"21"}"#.utf8)
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
        let first = PendingCommand(idempotencyKey: "stable-key", kind: "voice.text_input", expectedVersion: 2, payload: .object(["text": .string("first")]), createdAt: "now", instanceLineageID: "instance-1", expectedResourceType: nil, expectedResourceID: nil)
        let changed = PendingCommand(idempotencyKey: "stable-key", kind: "voice.text_input", expectedVersion: 2, payload: .object(["text": .string("second")]), createdAt: "later", instanceLineageID: "instance-1", expectedResourceType: nil, expectedResourceID: nil)
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
            acceptedReceipt = Self.commandReceipt(key: submittedKey, kind: "ui.action", resourceID: "surface", resourceType: "ui_surface")
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
            _ = try await coordinator.submit(kind: "ui.action", payload: .object(["surface_id": .string("surface")]), instanceLineageID: "instance-1", idempotencyKey: key)
            XCTFail("The first response should be lost after the server accepts the command")
        } catch { }
        let pendingAfterTimeout = try await journal.all()
        XCTAssertEqual(pendingAfterTimeout.map(\.idempotencyKey), [key])

        let recovered = try await coordinator.retryPending(idempotencyKey: key, currentInstanceLineageID: "instance-1")

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
        let payload: JSONValue = .object(["text": .string("Original payload")])
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "voice.text_input", expectedVersion: nil, payload: payload, createdAt: "2026-10-22T12:00:00Z", instanceLineageID: "instance-1", expectedResourceType: nil, expectedResourceID: nil))
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
                return (200, ["Content-Type": "application/json"], Self.commandReceipt(key: key, kind: "voice.text_input", resourceID: "task-created"))
            }
            return (503, ["Content-Type": "application/json"], Data())
        }

        let outcome = try await coordinator.retryPending(idempotencyKey: key, currentInstanceLineageID: "instance-1")

        XCTAssertEqual(receiptCount, 1, "receipt lookup must precede the safe retry")
        XCTAssertEqual(postCount, 1)
        XCTAssertEqual(postKey, key)
        let postedEnvelope = try XCTUnwrap(postBody.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] })
        XCTAssertEqual(postedEnvelope["kind"] as? String, "voice.text_input")
        XCTAssertEqual(postedEnvelope["payload"] as? [String: String], ["text": "Original payload"])
        XCTAssertEqual(outcome.receipt.idempotencyKey, key)
        let remaining = try await journal.all()
        XCTAssertTrue(remaining.isEmpty)
    }

    func testPendingRetryRefusesIncompatibleInstanceWithoutNetworkRequest() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let key = "instance-guard-\(UUID().uuidString.lowercased())"
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "voice.text_input", expectedVersion: nil, payload: .object([:]), createdAt: "now", instanceLineageID: "old-instance", expectedResourceType: nil, expectedResourceID: nil))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let coordinator = CommandCoordinator(api: api, journal: journal)
        ContractURLProtocol.responseHandler = { _ in XCTFail("instance mismatch must not make a network request"); return (500, [:], Data()) }

        do {
            _ = try await coordinator.retryPending(idempotencyKey: key, currentInstanceLineageID: "new-instance")
            XCTFail("Expected an instance guard")
        } catch let error as ZoeError {
            guard case .pendingCommandInstanceMismatch = error else { return XCTFail("Unexpected error: \(error)") }
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
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "voice.text_input", expectedVersion: nil, payload: .object(["text": .string("Work")]), createdAt: "now", instanceLineageID: "instance-1", expectedResourceType: nil, expectedResourceID: nil))
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
            _ = try await coordinator.retryPending(idempotencyKey: key, currentInstanceLineageID: "instance-1")
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

        XCTAssertFalse(model.canContinueVoiceAttemptForTesting(startingEpoch), "a late SDP answer cannot reactivate the interrupted attempt")
        XCTAssertEqual(localStopCount, 1, "the startup interruption claims local teardown once")
        XCTAssertFalse(model.canEndVoiceSession, "the event does not invent an active server session")
        guard case .interrupted = model.voiceStatus else { return XCTFail("expected an interrupted voice setup") }

        let nextEpoch = model.beginVoiceAttemptForTesting()
        XCTAssertTrue(model.canContinueVoiceAttemptForTesting(nextEpoch))
        XCTAssertFalse(model.canContinueVoiceAttemptForTesting(startingEpoch), "old callbacks remain fenced after a later attempt begins")
    }

    @MainActor
    func testMediaLossClosesLocalMediaBeforeGatewayHangupAndRetriesUnconfirmedClose() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        var hangupCount = 0
        var localStopCount = 0
        ContractURLProtocol.responseHandler = { request in
            if request.url?.path == "/client/v1/voice/calls/gateway-session-1/hangup" {
                XCTAssertEqual(request.url?.host, "gateway.example")
                XCTAssertEqual(request.httpMethod, "POST")
                hangupCount += 1
                XCTAssertEqual(localStopCount, 1, "close local media before calling gateway hangup")
                XCTAssertTrue((Self.requestBody(request) ?? Data()).isEmpty)
                if hangupCount == 1 {
                    return (503, [:], Data(#"{"error":{"code":"voice_unavailable","message":"temporary"}}"#.utf8))
                }
                return (204, [:], Data())
            }
            XCTFail("Unexpected media lifecycle request: \(request.url?.path ?? "")")
            return (404, [:], Data())
        }
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let session = try voiceDialFixture("session-1", generation: 4)
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        model.voiceLocalMediaStopObserverForTesting = { localStopCount += 1 }

        await model.handleVoiceMediaLossForTesting(.connection(.disconnected), mediaEpoch: epoch)
        await model.handleVoiceMediaLossForTesting(.connection(.failed), mediaEpoch: epoch)
        XCTAssertEqual(localStopCount, 1, "local media teardown is claimed once")
        XCTAssertEqual(hangupCount, 1, "duplicate terminal signals issue at most one hangup")
        guard case let .interrupted(reason) = model.voiceStatus else { return XCTFail("expected an interrupted voice status") }
        XCTAssertTrue(reason.contains("gateway hangup is unconfirmed"))
        XCTAssertTrue(model.canEndVoiceSession, "the handle remains available for idempotent retry")
        XCTAssertTrue(model.hasPendingVoiceHangupForTesting)

        await model.endVoiceSession()

        XCTAssertEqual(hangupCount, 2)
        XCTAssertFalse(model.canEndVoiceSession)
        XCTAssertFalse(model.hasPendingVoiceHangupForTesting)
    }

    @MainActor
    func testDelayedHangupAcknowledgementCannotTearDownNewVoiceSession() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let old = try voiceDialFixture("session-old", generation: 2)
        let oldEpoch = model.installVoiceSessionForTesting(old, api: api)
        var localStopCount = 0
        model.voiceLocalMediaStopObserverForTesting = { localStopCount += 1 }
        let gate = AsyncTestGate()
        model.voiceHangupOverrideForTesting = { handle in
            XCTAssertEqual(handle, old.gatewaySessionId)
            await gate.pause()
        }
        let hangup = Task { await model.endVoiceSession() }
        await gate.waitUntilStarted()
        let current = try voiceDialFixture("session-current", generation: 3)
        let currentEpoch = model.installVoiceSessionForTesting(current, api: api)
        await gate.resume()
        await hangup.value

        XCTAssertGreaterThan(currentEpoch, oldEpoch)
        XCTAssertTrue(model.canEndVoiceSession)
        XCTAssertEqual(localStopCount, 0, "the old acknowledgement must not close the replacement media")
        guard case let .active(sessionID, generation) = model.voiceStatus else { return XCTFail("the newer voice session must remain active") }
        XCTAssertEqual(sessionID, current.voiceSessionId)
        XCTAssertEqual(generation, current.generation)
    }

    @MainActor
    func testConcurrentRetryCannotSendSecondVoiceHangup() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticCredential(), session: session())
        let session = try voiceDialFixture("session-retry", generation: 5)
        _ = model.installVoiceSessionForTesting(session, api: api)
        let gate = AsyncTestGate()
        var hangupCount = 0
        model.voiceHangupOverrideForTesting = { _ in
            hangupCount += 1
            await gate.pause()
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

    @MainActor
    func testUserHangupWaitsForGatewayAndIgnoresCallbacks() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try voiceContractAPI()
        let call = try voiceDialFixture("user-hangup", generation: 1)
        let epoch = model.installVoiceSessionForTesting(call, api: api)
        let gate = AsyncTestGate()
        var events: [String] = []
        model.voiceLocalMediaStopObserverForTesting = { events.append("local_close") }
        model.voiceHangupOverrideForTesting = { _ in
            events.append("gateway_hangup")
            await gate.pause()
        }
        let end = Task { await model.endVoiceSession() }
        await gate.waitUntilStarted()
        XCTAssertEqual(events, ["gateway_hangup"])
        XCTAssertFalse(model.canEndVoiceSession)
        guard case .interrupted = model.voiceStatus else { return XCTFail("call must show ending") }
        await model.handleVoiceMediaLossForTesting(.dataChannel(.closed), mediaEpoch: epoch)
        await model.handleVoiceMediaLossForTesting(.connection(.failed), mediaEpoch: epoch)
        await model.receiveVoiceDataForTesting(Data(#"{"type":"voice.end"}"#.utf8), mediaEpoch: epoch)
        await model.endVoiceSession()
        await model.startVoice()
        XCTAssertEqual(events, ["gateway_hangup"])
        guard case .interrupted = model.voiceStatus else { return XCTFail("callbacks must not change ending status") }
        await gate.resume()
        await end.value
        XCTAssertEqual(events, ["gateway_hangup", "local_close"])
        XCTAssertEqual(model.voiceStatus, .idle)
    }

    @MainActor
    func testHungUserHangupClosesMediaAtDeadline() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try voiceContractAPI()
        _ = model.installVoiceSessionForTesting(try voiceDialFixture("hung", generation: 1), api: api)
        model.voiceHangupTimeoutForTesting = .milliseconds(50)
        let gate = AsyncTestGate()
        var events: [String] = []
        model.voiceLocalMediaStopObserverForTesting = { events.append("local_close") }
        model.voiceHangupOverrideForTesting = { _ in
            events.append("gateway_hangup")
            await gate.pause()
        }
        let end = Task { await model.endVoiceSession() }
        await gate.waitUntilStarted()
        await end.value
        XCTAssertEqual(events, ["gateway_hangup", "local_close"])
        XCTAssertTrue(model.hasPendingVoiceHangupForTesting)
        guard case let .interrupted(reason) = model.voiceStatus else { return XCTFail("expected unconfirmed hangup") }
        XCTAssertTrue(reason.contains("gateway hangup is unconfirmed"))
        await gate.resume()
    }

    @MainActor
    func testFailedUserHangupStillClosesMedia() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let api = try voiceContractAPI()
        _ = model.installVoiceSessionForTesting(try voiceDialFixture("failed", generation: 1), api: api)
        var events: [String] = []
        model.voiceLocalMediaStopObserverForTesting = { events.append("local_close") }
        model.voiceHangupOverrideForTesting = { _ in
            events.append("gateway_hangup")
            throw ZoeError.invalidResponse
        }
        await model.endVoiceSession()
        XCTAssertEqual(events, ["gateway_hangup", "local_close"])
        XCTAssertTrue(model.hasPendingVoiceHangupForTesting)
        guard case let .interrupted(reason) = model.voiceStatus else { return XCTFail("expected unconfirmed hangup") }
        XCTAssertTrue(reason.contains("gateway hangup is unconfirmed"))
    }

    private static func commandReceipt(key: String, kind: String, resourceID: String, resourceType: String = "task") -> Data {
        Data("{\"command_id\":\"command-1\",\"idempotency_key\":\"\(key)\",\"kind\":\"\(kind)\",\"status\":\"accepted\",\"resource_type\":\"\(resourceType)\",\"resource_id\":\"\(resourceID)\",\"resource_version\":1,\"accepted_at\":\"2026-10-22T12:00:00Z\",\"event_cursor\":\"8\"}".utf8)
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
