import XCTest
@testable import ZoeKit
@testable import ZoeUI
import Foundation
import CryptoKit
import WebRTC

private final class ContractURLProtocol: URLProtocol {
    static var responseHandler: ((URLRequest) -> (Int, [String: String], Data))?
    static var failureHandler: ((URLRequest) -> Error?)?
    static var capturedRequest: URLRequest?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
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
    private func session() -> URLSession {
        ContractURLProtocol.failureHandler = nil
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
