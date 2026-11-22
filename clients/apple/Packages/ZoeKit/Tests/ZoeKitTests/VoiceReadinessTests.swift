import XCTest
@testable import ZoeKit
@testable import ZoeUI

private final class ReadinessKeyStore: InstanceKeyStoring, @unchecked Sendable {
    var key: InstanceKey?
    func load() throws -> InstanceKey? { key }
    func replace(_ key: InstanceKey) throws { self.key = key }
    func reset() throws { key = nil }
    func bearerToken() throws -> String {
        guard let key else { throw ZoeError.missingCredential }
        return try key.bearerToken()
    }
}

@MainActor
final class VoiceReadinessTests: XCTestCase {
    private func fixture(version: String = "v1") throws -> (VoiceConfig, VoicePoolStatus) {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        let config = try decoder.decode(VoiceConfig.self, from: Data("""
        {"version":"\(version)","gateway_dial_origin":"https://gateway.example","defaults":{"voice":"default","voice_mode":"","language_code":"auto"},
         "voices":[],"voice_modes":[],"languages":[],
         "webrtc":{"data_channel":{"label":"zoe","negotiated":true,"id":0},
         "ice_servers":[],"receive_audio":true,"receive_video":false}}
        """.utf8))
        let pool = try decoder.decode(VoicePoolStatus.self, from: Data("""
        {"blocked":false,"reason_code":"","updated_at":"now","account_lease_identity":false,
         "account_scoped_termination":false,"trusted_termination_evidence":false}
        """.utf8))
        return (config, pool)
    }

    private func model() throws -> ZoeAppModel {
        let name = "VoiceReadinessTests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: name)!
        addTeardownBlock { defaults.removePersistentDomain(forName: name) }
        let model = ZoeAppModel(defaults: defaults)
        model.serverAddress = "https://readiness.example"
        let store = ReadinessKeyStore()
        try store.replace(InstanceKey(Data(repeating: 1, count: 32).base64EncodedString()
            .replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")))
        model.keyStoreOverrideForTesting = { _ in store }
        return model
    }

    func testSuccessAndFailureDoNotChangeActiveOrConnectingMedia() async throws {
        let model = try model()
        let next = try fixture()
        model.voiceReadinessOverrideForTesting = { next }
        await model.refreshVoiceReadiness() // Configure the connection before installing media.
        XCTAssertEqual(model.voiceConfig, next.0)
        let api = try APIClient(serverURL: URL(string: model.serverAddress)!)
        let session = try voiceDialFixture("active", generation: 1)
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        await model.refreshVoiceReadiness()
        XCTAssertEqual(model.voiceStatus, .active(sessionID: "active", mediaGeneration: 1))
        model.voiceReadinessOverrideForTesting = { throw ZoeError.invalidResponse }
        await model.refreshVoiceReadiness()
        XCTAssertEqual(model.voiceStatus, .active(sessionID: "active", mediaGeneration: 1))
        XCTAssertTrue(model.canContinueVoiceAttemptForTesting(epoch))
        XCTAssertTrue(model.canEndVoiceSession)
        let connecting = try self.model()
        connecting.voiceReadinessOverrideForTesting = { next }
        await connecting.refreshVoiceReadiness()
        _ = connecting.beginVoiceAttemptForTesting()
        await connecting.refreshVoiceReadiness()
        XCTAssertEqual(connecting.voiceStatus, .connecting)
        connecting.voiceReadinessOverrideForTesting = { throw ZoeError.invalidResponse }
        await connecting.refreshVoiceReadiness()
        XCTAssertEqual(connecting.voiceStatus, .connecting)
    }

    func testIdleFailureRetainsCacheAndLifecycle() async throws {
        let model = try model()
        let next = try fixture()
        model.voiceReadinessOverrideForTesting = { next }
        await model.refreshVoiceReadiness()
        // The first refresh configures the connection, which publishes .stopped.
        let before = model.voiceStatus
        model.voiceReadinessOverrideForTesting = { throw ZoeError.invalidResponse }
        await model.refreshVoiceReadiness()
        XCTAssertEqual(model.voiceStatus, before)
        XCTAssertEqual(model.voiceConfig, next.0)
        XCTAssertEqual(model.voicePoolStatus, next.1)
    }

    func testRefreshCannotApplyAcrossMediaTransitionOrCancellation() async throws {
        for cancel in [false, true] {
            let model = try model()
            let next = try fixture()
            let started = expectation(description: "readiness suspended")
            var continuation: CheckedContinuation<(VoiceConfig, VoicePoolStatus), Error>?
            model.voiceReadinessOverrideForTesting = {
                try await withCheckedThrowingContinuation {
                    continuation = $0
                    started.fulfill()
                }
            }
            let task = Task { await model.refreshVoiceReadiness() }
            await fulfillment(of: [started], timeout: 2)
            let before = model.voiceStatus
            if cancel { task.cancel() }
            else { _ = model.beginVoiceAttemptForTesting() }
            continuation?.resume(returning: next)
            await task.value
            XCTAssertNil(model.voiceConfig)
            XCTAssertNil(model.voicePoolStatus)
            XCTAssertEqual(model.voiceStatus, cancel ? before : .connecting)
        }
    }

    func testOlderRefreshCannotOverwriteNewerRefreshOrConnection() async throws {
        for switchConnection in [false, true] {
            let model = try model()
            let old = try fixture(version: "old")
            let next = try fixture(version: "new")
            let started = expectation(description: "old readiness suspended")
            var continuation: CheckedContinuation<(VoiceConfig, VoicePoolStatus), Error>?
            model.voiceReadinessOverrideForTesting = {
                try await withCheckedThrowingContinuation {
                    continuation = $0
                    started.fulfill()
                }
            }
            let task = Task { await model.refreshVoiceReadiness() }
            await fulfillment(of: [started], timeout: 2)
            if switchConnection { model.serverAddress = "https://other-readiness.example" }
            model.voiceReadinessOverrideForTesting = { next }
            await model.refreshVoiceReadiness()
            continuation?.resume(returning: old)
            await task.value
            XCTAssertEqual(model.voiceConfig, next.0)
            XCTAssertEqual(model.voicePoolStatus, next.1)
        }
    }

    func testRefreshCompletingAfterHangupCannotResetStoppedState() async throws {
        let model = try model()
        let initial = try fixture()
        model.voiceReadinessOverrideForTesting = { initial }
        await model.refreshVoiceReadiness()
        let api = try APIClient(serverURL: URL(string: model.serverAddress)!)
        _ = model.installVoiceSessionForTesting(
            try voiceDialFixture("ending", generation: 1), api: api)
        model.voiceHangupOverrideForTesting = { _ in }
        let started = expectation(description: "readiness suspended before hangup")
        var continuation: CheckedContinuation<(VoiceConfig, VoicePoolStatus), Error>?
        model.voiceReadinessOverrideForTesting = {
            try await withCheckedThrowingContinuation {
                continuation = $0
                started.fulfill()
            }
        }
        let task = Task { await model.refreshVoiceReadiness() }
        await fulfillment(of: [started], timeout: 2)
        await model.endVoiceSession()
        let endedStatus = model.voiceStatus
        continuation?.resume(returning: try fixture(version: "stale"))
        await task.value
        XCTAssertFalse(model.canEndVoiceSession)
        XCTAssertEqual(model.voiceStatus, endedStatus)
        XCTAssertEqual(model.voiceConfig, initial.0)
    }
}
