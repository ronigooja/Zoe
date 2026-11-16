import Foundation
import CryptoKit

#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

public protocol BearerTokenProviding: Sendable {
    func bearerToken() throws -> String
}

public final class APIClient: @unchecked Sendable {
    public let serverURL: URL
    private let session: URLSession
    private let credentials: BearerTokenProviding?
    private let installationID: String?
    private let encoder: JSONEncoder
    private let decoder: JSONDecoder
    private let diagnosticQueue: EncryptedDiagnosticQueue?
    private let diagnosticsEnabled: Bool
    private let diagnosticDeliveryFailureKey: String

    public var hasCredentials: Bool { credentials != nil }

    public init(serverURL: URL, credentials: BearerTokenProviding? = nil, session: URLSession = .shared, installationID: String? = nil) throws {
        guard let scheme = serverURL.scheme?.lowercased(), ["http", "https"].contains(scheme), serverURL.host != nil else {
            throw ZoeError.invalidServerURL
        }
        self.serverURL = try ServerAddress.normalize(serverURL.absoluteString)
        self.installationID = installationID
        self.credentials = credentials
        self.session = session
        self.diagnosticQueue = EncryptedDiagnosticQueue(serverURL: self.serverURL, installationID: installationID)
        self.diagnosticsEnabled = installationID != nil
        self.diagnosticDeliveryFailureKey = "zoe.diagnostics.delivery_failures." + self.serverURL.absoluteString
        encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
    }

    public func recovery() async throws -> RecoverySnapshot {
        let value: JSONValue = try await request(path: ["v1", "recovery"], method: "GET", authenticated: true)
        let result = try JSONDecoder().decode(RecoverySnapshot.self, from: JSONEncoder().encode(value))
        guard let cursor = Int64(result.cursor), cursor >= 0,
              result.snapshot.eventCursor == result.cursor, result.ui.cursor == cursor,
              result.snapshot.workspaceID == "default", result.snapshot.subjectID == "owner" else {
            throw ZoeError.invalidResponse
        }
        return result
    }

    public func uiSnapshot() async throws -> UISnapshot {
        // A2UI message keys and the strict UI contract must not undergo snake-case conversion.
        let value: JSONValue = try await request(path: ["v1", "ui", "snapshot"], method: "GET", authenticated: true)
        return try JSONDecoder().decode(UISnapshot.self, from: JSONEncoder().encode(value))
    }

    public func snapshot() async throws -> Snapshot {
        let snapshot: Snapshot = try await request(path: ["v1", "snapshot"], method: "GET", authenticated: true)
        guard snapshot.workspaceId == "default", snapshot.subjectId == "owner" else { throw ZoeError.invalidResponse }
        return snapshot
    }

    public func voiceConfig() async throws -> VoiceConfig {
        try await request(path: ["v1", "voice", "config"], method: "GET", authenticated: true)
    }

    public func recordDiagnostic(phase: String, requestPath: String, clientRequestID: String = UUID().uuidString.lowercased(), httpStatus: Int = 0, responseBytes: Int64 = 0, errorType: String = "", errorSummary: String = "", level: DiagnosticLevel? = nil, voiceSessionID: String? = nil, voiceGeneration: Int64? = nil, voiceMediaEpoch: UInt64? = nil, voicePeerState: String? = nil, voiceDataChannelState: String? = nil) {
        guard diagnosticsEnabled, let queue = diagnosticQueue else { return }
        let effectiveLevel = level ?? ((200..<300).contains(httpStatus) && errorType.isEmpty ? .info : .error)
        let event = DiagnosticEvent(clientRequestID: clientRequestID, phase: phase, requestPath: requestPath, httpStatus: httpStatus, responseBytes: responseBytes, errorType: errorType, errorSummary: errorSummary, installationID: installationID, level: effectiveLevel, voiceSessionID: voiceSessionID, voiceGeneration: voiceGeneration, voiceMediaEpoch: voiceMediaEpoch, voicePeerState: voicePeerState, voiceDataChannelState: voiceDataChannelState)
        Task { if !(await queue.append(event)) { self.noteDiagnosticDeliveryFailure("enqueue") } }
    }

    public func recordDiagnosticAndWait(phase: String, requestPath: String, httpStatus: Int = 0, responseBytes: Int64 = 0, errorType: String = "", errorSummary: String = "", level: DiagnosticLevel? = nil, voiceSessionID: String? = nil, voiceGeneration: Int64? = nil, voiceMediaEpoch: UInt64? = nil, voicePeerState: String? = nil, voiceDataChannelState: String? = nil) async {
        guard diagnosticsEnabled, let queue = diagnosticQueue else { return }
        let effectiveLevel = level ?? ((200..<300).contains(httpStatus) && errorType.isEmpty ? .info : .error)
        let event = DiagnosticEvent(clientRequestID: UUID().uuidString.lowercased(), phase: phase, requestPath: requestPath, httpStatus: httpStatus, responseBytes: responseBytes, errorType: errorType, errorSummary: errorSummary, installationID: installationID, level: effectiveLevel, voiceSessionID: voiceSessionID, voiceGeneration: voiceGeneration, voiceMediaEpoch: voiceMediaEpoch, voicePeerState: voicePeerState, voiceDataChannelState: voiceDataChannelState)
        if !(await queue.append(event)) { noteDiagnosticDeliveryFailure("enqueue") }
    }

    public func applyDiagnosticLevel(_ level: DiagnosticLevel) async {
        await diagnosticQueue?.setLevel(level)
    }

    public func fetchClientPolicy() async throws -> ClientPolicy {
        var request = URLRequest(url: try makeURL(path: ["v1", "client", "policy"]))
        request.httpMethod = "GET"
        try authorize(&request)
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw ZoeError.invalidResponse }
        guard (200..<300).contains(http.statusCode) else { throw decodeFailure(status: http.statusCode, data: data) }
        // ClientPolicy declares wire-format keys; do not transform them a second time.
        return try JSONDecoder().decode(ClientPolicy.self, from: data)
    }

    public func uploadDiagnosticsIfPossible() async {
        guard let credentials, let queue = diagnosticQueue, let token = try? credentials.bearerToken() else { return }
        guard await queue.canAttempt() else { return }
        guard await queue.beginUpload() else { return }
        defer { Task { await queue.endUpload() } }
        let events = await queue.take(limit: 50)
        guard !events.isEmpty, let url = try? makeURL(path: ["v1", "diagnostics", "client"]), let body = try? encoder.encode(["events": events]) else { noteDiagnosticDeliveryFailure("prepare"); return }
        var request = URLRequest(url: url)
        request.timeoutInterval = 2
        request.httpMethod = "POST"
        request.httpBody = body
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if let installationID { request.setValue(installationID, forHTTPHeaderField: "X-Zoe-Installation-ID") }
        do {
            let (_, response) = try await session.data(for: request)
            guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else { await queue.markFailure(); noteDiagnosticDeliveryFailure("upload"); return }
            await queue.removeFirst(events.count)
            await queue.markSuccess()
        } catch { await queue.markFailure(); noteDiagnosticDeliveryFailure("upload") }
    }

    /// Independent cancellation bounds diagnostic work without gating lifecycle operations.
    public func triggerDiagnosticUpload(timeout: TimeInterval = 2) {
        let upload = Task { await self.uploadDiagnosticsIfPossible() }
        Task {
            try? await Task.sleep(for: .seconds(timeout))
            upload.cancel()
        }
    }

    private func noteDiagnosticDeliveryFailure(_ kind: String) {
        let key = diagnosticDeliveryFailureKey + "." + kind
        UserDefaults.standard.set(min(UserDefaults.standard.integer(forKey: key) + 1, 1000), forKey: key)
    }

    public func voicePoolStatus() async throws -> VoicePoolStatus {
        try await request(path: ["v1", "voice", "pool", "status"], method: "GET", authenticated: true)
    }

    public func createVoiceSession(requestID: String, offerSDP: String, voice: String, voiceMode: String, languageCode: String) async throws -> VoiceSessionResponse {
        let body: [String: JSONValue] = [
            "request_id": .string(requestID),
            "offer_sdp": .string(offerSDP),
            "voice": .string(voice),
            "voice_mode": .string(voiceMode),
            "language_code": .string(languageCode)
        ]
        return try await request(path: ["v1", "voice", "sessions"], method: "POST", body: body, authenticated: true)
    }

    public func confirmVoiceConnected(sessionID: String, generation: Int64) async throws -> VoiceOperationResponse {
        let body = VoiceGenerationRequest(generation: generation)
        return try await request(path: ["v1", "voice", "sessions", sessionID, "connected"], method: "POST", body: body, authenticated: true)
    }

    public func heartbeatVoiceSession(sessionID: String) async throws -> VoiceOperationResponse {
        try await request(path: ["v1", "voice", "sessions", sessionID, "heartbeat"], method: "POST", body: EmptyObject(), authenticated: true)
    }

    public func hangupVoiceSession(_ session: VoiceSessionResponse) async throws -> VoiceOperationResponse {
        let body = VoiceHangupRequest(mediaClosed: true, generation: session.generation, accountLeaseId: session.accountLeaseId ?? "", accountLeaseGeneration: session.accountLeaseGeneration ?? 0)
        return try await request(path: ["v1", "voice", "sessions", session.voiceSessionId, "hangup"], method: "POST", body: body, authenticated: true)
    }

    public func receipt(idempotencyKey: String) async throws -> CommandReceipt {
        var components = URLComponents(url: try makeURL(path: ["v1", "commands"]), resolvingAgainstBaseURL: false)
        components?.queryItems = [URLQueryItem(name: "key", value: idempotencyKey)]
        guard let url = components?.url else { throw ZoeError.invalidServerURL }
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        try authorize(&request)
        return try await perform(request)
    }

    public func submit<Payload: Encodable & Sendable>(kind: String, expectedVersion: Int64? = nil, payload: Payload, idempotencyKey: String) async throws -> CommandReceipt {
        let envelope = CommandEnvelope(kind: kind, expectedVersion: expectedVersion, payload: payload)
        var request = URLRequest(url: try makeURL(path: ["v1", "commands"]))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue(idempotencyKey, forHTTPHeaderField: "Idempotency-Key")
        try authorize(&request)
        request.httpBody = try encoder.encode(envelope)
        return try await perform(request, successDiagnosticPhase: kind == "ui.rendered" ? "ui_rendered_ack" : nil)
    }

    public func events(after cursor: String?) async throws -> URLSession.AsyncBytes {
        var request = URLRequest(url: try makeURL(path: ["v1", "events"]))
        request.httpMethod = "GET"
        request.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        if let cursor, !cursor.isEmpty {
            request.setValue(cursor, forHTTPHeaderField: "Last-Event-ID")
        }
        try authorize(&request)
        let (bytes, response) = try await session.bytes(for: request)
        guard let response = response as? HTTPURLResponse else { throw ZoeError.invalidResponse }
        guard (200..<300).contains(response.statusCode) else {
            let data = try await collect(bytes)
            throw decodeFailure(status: response.statusCode, data: data)
        }
        return bytes
    }

    public func decode<T: Decodable>(_ type: T.Type, from data: Data) throws -> T {
        try decoder.decode(type, from: data)
    }

    private func request<Response: Decodable, Body: Encodable>(path: [String], method: String, body: Body? = Optional<NoBody>.none, authenticated: Bool) async throws -> Response {
        var request = URLRequest(url: try makeURL(path: path))
        request.httpMethod = method
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try encoder.encode(body)
        }
        if authenticated { try authorize(&request) }
        let clientRequestID = UUID().uuidString.lowercased()
        request.setValue(clientRequestID, forHTTPHeaderField: "X-Zoe-Client-Request-ID")
        return try await perform(request, clientRequestID: clientRequestID)
    }

    private func request<Response: Decodable>(path: [String], method: String, authenticated: Bool) async throws -> Response {
        try await request(path: path, method: method, body: Optional<NoBody>.none, authenticated: authenticated)
    }

    private func perform<Response: Decodable>(_ request: URLRequest, clientRequestID: String? = nil, successDiagnosticPhase: String? = nil) async throws -> Response {
        let path = request.url?.path ?? ""
        let requestID = clientRequestID ?? request.value(forHTTPHeaderField: "X-Zoe-Client-Request-ID") ?? UUID().uuidString.lowercased()
        let phase = diagnosticPhase(path)
        do {
            let (data, response) = try await session.data(for: request)
            guard let response = response as? HTTPURLResponse else {
                recordDiagnostic(phase: phase, requestPath: path, clientRequestID: requestID, errorType: "invalid_response", errorSummary: "non_http_response")
                throw ZoeError.invalidResponse
            }
            guard (200..<300).contains(response.statusCode) else {
                let failure = decodeFailure(status: response.statusCode, data: data)
                recordDiagnostic(phase: phase, requestPath: path, clientRequestID: requestID, httpStatus: response.statusCode, responseBytes: Int64(data.count), errorType: "http_error", errorSummary: diagnosticSummary(failure))
                throw failure
            }
            do {
                let decoded = try decoder.decode(Response.self, from: data)
                if let successDiagnosticPhase {
                    recordDiagnostic(phase: successDiagnosticPhase, requestPath: path, clientRequestID: requestID, httpStatus: response.statusCode, level: .info)
                }
                if phase != "request" {
                    recordDiagnostic(phase: phase, requestPath: path, clientRequestID: requestID, httpStatus: response.statusCode, responseBytes: Int64(data.count), level: .info)
                }
                return decoded
            }
            catch {
                recordDiagnostic(phase: phase == "config_request" ? "config_decode" : phase, requestPath: path, clientRequestID: requestID, httpStatus: response.statusCode, responseBytes: Int64(data.count), errorType: "decoding_error", errorSummary: RedactedDiagnostic(error).errorSummary)
                throw ZoeError.invalidResponse
            }
        } catch let error as ZoeError { throw error }
        catch {
            recordDiagnostic(phase: phase, requestPath: path, clientRequestID: requestID, errorType: "transport_error", errorSummary: RedactedDiagnostic(error).errorSummary)
            throw error
        }
    }

    private func diagnosticPhase(_ path: String) -> String {
        if path.hasSuffix("/voice/config") { return "config_request" }
        if path.hasSuffix("/voice/pool/status") { return "pool_status" }
        if path.hasSuffix("/voice/sessions") { return "session_create" }
        if path.contains("/connected") { return "connected" }
        return "request"
    }

    private func diagnosticSummary(_ error: ZoeError) -> String {
        if case let .http(_, code, _, _) = error { return String(code.prefix(128)) }
        return "http_error"
    }

    private func authorize(_ request: inout URLRequest) throws {
        guard let credentials else { throw ZoeError.missingCredential }
        request.setValue("Bearer \(try credentials.bearerToken())", forHTTPHeaderField: "Authorization")
        if let installationID { request.setValue(installationID, forHTTPHeaderField: "X-Zoe-Installation-ID") }
    }

    private func makeURL(path: [String]) throws -> URL {
        var url = serverURL
        for component in path {
            url.appendPathComponent(component)
        }
        guard url.scheme != nil, url.host != nil else { throw ZoeError.invalidServerURL }
        return url
    }

    private func decodeFailure(status: Int, data: Data) -> ZoeError {
        if let body = try? decoder.decode(APIFailureBody.self, from: data) {
            return .http(status: status, code: body.error.code, message: body.error.message, details: body.error.details)
        }
        return .http(status: status, code: "http_error", message: "The server returned HTTP \(status).", details: nil)
    }

    private func collect(_ bytes: URLSession.AsyncBytes) async throws -> Data {
        try await collect(bytes, limit: Int.max)
    }

    private func collect(_ bytes: URLSession.AsyncBytes, limit: Int) async throws -> Data {
        var data = Data()
        for try await byte in bytes {
            data.append(byte)
        }
        return data
    }
}

private struct CommandEnvelope<Payload: Encodable & Sendable>: Encodable {
    var kind: String
    var expectedVersion: Int64?
    var payload: Payload
}

private struct NoBody: Encodable {}

private struct EmptyObject: Encodable {}

private struct VoiceGenerationRequest: Encodable {
    var generation: Int64
}

private struct VoiceHangupRequest: Encodable {
    var mediaClosed: Bool
    var generation: Int64
    var accountLeaseId: String
    var accountLeaseGeneration: Int64
}
