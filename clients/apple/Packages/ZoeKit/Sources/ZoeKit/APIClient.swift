import Foundation
import CryptoKit

#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

public protocol BearerTokenProviding: Sendable {
    func bearerToken() throws -> String
}

public struct ScopedExportArtifact: Sendable {
    public let fileURL: URL
    public let filename: String
    public let scope: String
    public let snapshotCursor: String
    public let instanceLineageID: String
    public let generation: Int64
    public let deletionSequence: String
    public let exportID: String
    public let validatedThroughCursor: String
    public let exportAuditEventCursor: String
    public let exportAuditEventID: String
    public let exportAuditEventType: String

    public init(fileURL: URL, filename: String, scope: String, snapshotCursor: String, instanceLineageID: String = "", generation: Int64 = 0, deletionSequence: String = "0", exportID: String = "", validatedThroughCursor: String = "", exportAuditEventCursor: String = "", exportAuditEventID: String = "", exportAuditEventType: String = "") {
        self.fileURL = fileURL
        self.filename = filename
        self.scope = scope
        self.snapshotCursor = snapshotCursor
        self.instanceLineageID = instanceLineageID
        self.generation = generation
        self.deletionSequence = deletionSequence
        self.exportID = exportID
        self.validatedThroughCursor = validatedThroughCursor
        self.exportAuditEventCursor = exportAuditEventCursor
        self.exportAuditEventID = exportAuditEventID
        self.exportAuditEventType = exportAuditEventType
    }
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

    public static func cleanupAbandonedExportDownloads() {
        BoundedDownloadDelegate.cleanupAbandonedFiles()
    }

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

    public func snapshot() async throws -> Snapshot {
        let snapshot: Snapshot = try await request(path: ["v1", "snapshot"], method: "GET", authenticated: true)
        guard snapshot.workspaceId == "default", snapshot.subjectId == "owner" else { throw ZoeError.invalidResponse }
        return snapshot
    }

    public func taskEntries(taskID: String, cursor: String? = nil, limit: Int = 100) async throws -> TaskEntriesPage {
        guard !taskID.isEmpty, (1...100).contains(limit) else { throw ZoeError.invalidResponse }
        var components = URLComponents(url: try makeURL(path: ["v1", "tasks", taskID, "entries"]), resolvingAgainstBaseURL: false)
        var queryItems = [URLQueryItem(name: "limit", value: String(limit))]
        if let cursor, !cursor.isEmpty { queryItems.append(URLQueryItem(name: "cursor", value: cursor)) }
        components?.queryItems = queryItems
        guard let url = components?.url else { throw ZoeError.invalidServerURL }
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        try authorize(&request)
        return try await perform(request)
    }

    public func downloadExport(scope: String, temporaryDirectory: URL = FileManager.default.temporaryDirectory) async throws -> ScopedExportArtifact {
        guard scope == "personal" || scope == "workspace" else { throw ZoeError.invalidResponse }
        var components = URLComponents(url: try makeURL(path: ["v1", "export"]), resolvingAgainstBaseURL: false)
        components?.queryItems = [URLQueryItem(name: "scope", value: scope)]
        guard let url = components?.url else { throw ZoeError.invalidServerURL }
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        request.setValue("application/vnd.zoe.export+zip", forHTTPHeaderField: "Accept")
        try authorize(&request)

        let download = try await downloadBounded(request, using: session, maximumBytes: Int64(Self.maximumExportBytes))
        let downloadLease = download.lease
        let downloadURL = download.fileURL
        defer { downloadLease.cleanup() }
        let response = download.response
        guard (200..<300).contains(response.statusCode) else {
            let data = try Data(contentsOf: downloadURL)
            throw decodeFailure(status: response.statusCode, data: data)
        }
        if let length = response.value(forHTTPHeaderField: "Content-Length").flatMap(UInt64.init), length > Self.maximumExportBytes {
            throw ZoeError.exportTooLarge
        }
        let downloadedAttributes = try FileManager.default.attributesOfItem(atPath: downloadURL.path)
        let downloadedSize = (downloadedAttributes[.size] as? NSNumber)?.uint64Value
        guard let downloadedSize, downloadedSize <= Self.maximumExportBytes else {
            throw ZoeError.exportTooLarge
        }
        if let length = response.value(forHTTPHeaderField: "Content-Length").flatMap(UInt64.init), length != downloadedSize {
            throw ZoeError.invalidResponse
        }
        guard response.value(forHTTPHeaderField: "Content-Type")?.lowercased().hasPrefix("application/vnd.zoe.export+zip") == true,
              let filename = Self.safeExportFilename(response.value(forHTTPHeaderField: "Content-Disposition")) else {
            throw ZoeError.invalidResponse
        }
        let archive: ValidatedExportArchive
        do {
            archive = try ValidatedExportArchive(fileURL: downloadURL,
                                                 archiveSHA256: response.value(forHTTPHeaderField: "X-Zoe-Archive-SHA256"),
                                                 manifestSHA256: response.value(forHTTPHeaderField: "X-Zoe-Manifest-SHA256"),
                                                 decoder: decoder)
        } catch { throw ZoeError.invalidResponse }
        let manifest = archive.manifest
        let deletionSequence = Int64(manifest.authority.deletionSequence)
        let dataCursor = UInt64(manifest.authority.snapshotEventCursor)
        let validatedCursor = UInt64(manifest.authority.validatedThroughCursor)
        let auditCursor = UInt64(manifest.authority.exportAuditEventCursor)
        guard manifest.format == "zoe.authority-export", manifest.schemaVersion == 2,
              manifest.scope == scope, !manifest.workspaceId.isEmpty, !manifest.subjectId.isEmpty,
              !manifest.exportId.isEmpty,
              !manifest.authority.snapshotEventCursor.isEmpty, !manifest.authority.instanceLineageId.isEmpty,
              manifest.authority.generation > 0, let deletionSequence, deletionSequence >= 0,
              let dataCursor, let validatedCursor, let auditCursor,
              dataCursor < UInt64.max, validatedCursor == dataCursor + 1,
              auditCursor == validatedCursor,
              manifest.authority.exportAuditEventId == manifest.exportId,
              manifest.authority.exportAuditEventType == "data.export.prepared" else { throw ZoeError.invalidResponse }

        let fileURL = temporaryDirectory.appendingPathComponent("zoe-export-" + UUID().uuidString.lowercased() + ".zip", isDirectory: false)
        do {
            try FileManager.default.createDirectory(at: temporaryDirectory, withIntermediateDirectories: true)
            try FileManager.default.copyItem(at: downloadURL, to: fileURL)
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: fileURL.path)
            return ScopedExportArtifact(fileURL: fileURL, filename: filename, scope: scope,
                                        snapshotCursor: manifest.authority.snapshotEventCursor,
                                        instanceLineageID: manifest.authority.instanceLineageId,
                                        generation: manifest.authority.generation,
                                        deletionSequence: manifest.authority.deletionSequence,
                                        exportID: manifest.exportId,
                                        validatedThroughCursor: manifest.authority.validatedThroughCursor,
                                        exportAuditEventCursor: manifest.authority.exportAuditEventCursor,
                                        exportAuditEventID: manifest.authority.exportAuditEventId,
                                        exportAuditEventType: manifest.authority.exportAuditEventType)
        } catch {
            try? FileManager.default.removeItem(at: fileURL)
            throw ZoeError.persistence("The export could not be written to a private temporary file.")
        }
    }

    private static let maximumExportBytes: UInt64 = 1 << 30

    private static func safeExportFilename(_ header: String?) -> String? {
        guard let header, let marker = header.range(of: "filename=\""),
              let end = header[marker.upperBound...].firstIndex(of: "\"") else { return nil }
        let candidate = String(header[marker.upperBound..<end])
        guard candidate.hasPrefix("zoe-export-"), candidate.hasSuffix(".zip"),
              !candidate.contains("/"), !candidate.contains("\\"),
              candidate.dropFirst("zoe-export-".count).dropLast(".zip".count).allSatisfy({ $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-") }) else { return nil }
        return candidate
    }

    public func searchMemory(query: String = "", limit: Int = 50) async throws -> MemorySearchResult {
        var components = URLComponents(url: try makeURL(path: ["v1", "memory"]), resolvingAgainstBaseURL: false)
        components?.queryItems = [URLQueryItem(name: "q", value: query), URLQueryItem(name: "limit", value: String(limit))]
        guard let url = components?.url else { throw ZoeError.invalidServerURL }
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        try authorize(&request)
        return try await perform(request)
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
        return try decoder.decode(ClientPolicy.self, from: data)
    }

    public func uploadDiagnosticsIfPossible() async {
        guard let credentials, let queue = diagnosticQueue, let token = try? credentials.bearerToken() else { return }
        guard await queue.canAttempt() else { return }
        guard await queue.beginUpload() else { return }
        defer { Task { await queue.endUpload() } }
        let events = await queue.take(limit: 50)
        guard !events.isEmpty, let url = try? makeURL(path: ["v1", "diagnostics", "client"]), let body = try? encoder.encode(["events": events]) else { noteDiagnosticDeliveryFailure("prepare"); return }
        var request = URLRequest(url: url)
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

    private func noteDiagnosticDeliveryFailure(_ kind: String) {
        let key = diagnosticDeliveryFailureKey + "." + kind
        UserDefaults.standard.set(min(UserDefaults.standard.integer(forKey: key) + 1, 1000), forKey: key)
    }

    public func voicePoolStatus() async throws -> VoicePoolStatus {
        try await request(path: ["v1", "voice", "pool", "status"], method: "GET", authenticated: true)
    }

    public func notifications(unreadOnly: Bool = false, limit: Int = 50) async throws -> NotificationList {
        var components = URLComponents(url: try makeURL(path: ["v1", "notifications"]), resolvingAgainstBaseURL: false)
        components?.queryItems = [
            URLQueryItem(name: "unread", value: unreadOnly ? "true" : "false"),
            URLQueryItem(name: "limit", value: String(limit))
        ]
        guard let url = components?.url else { throw ZoeError.invalidServerURL }
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        try authorize(&request)
        return try await perform(request)
    }

    public func capabilityReleases() async throws -> CapabilityReleaseList {
        try await request(path: ["v1", "capabilities", "releases"], method: "GET", authenticated: true)
    }

    public func capabilityDeployments() async throws -> CapabilityDeploymentList {
        try await request(path: ["v1", "capabilities", "deployments"], method: "GET", authenticated: true)
    }

    public func capabilityDeployment(id: String) async throws -> CapabilityDeployment {
        try await request(path: ["v1", "capabilities", "deployments", id], method: "GET", authenticated: true)
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

    public func receipt(id: String) async throws -> CommandReceipt {
        try await request(path: ["v1", "commands", id], method: "GET", authenticated: true)
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
        return try await perform(request)
    }

    public func registerPushToken(platform: String, tokenHex: String, idempotencyKey: String = UUID().uuidString) async throws -> CommandReceipt {
        try await submit(kind: "device.push_token.register", payload: PushTokenRegistration(platform: platform, token: tokenHex), idempotencyKey: idempotencyKey)
    }

    public func revokePushToken(tokenID: String, idempotencyKey: String = UUID().uuidString) async throws -> CommandReceipt {
        try await submit(kind: "device.push_token.revoke", payload: PushTokenRevocation(tokenId: tokenID), idempotencyKey: idempotencyKey)
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

    private func perform<Response: Decodable>(_ request: URLRequest, clientRequestID: String? = nil) async throws -> Response {
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
            guard data.count < limit else { throw ZoeError.exportTooLarge }
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

private struct PushTokenRegistration: Encodable, Sendable {
    let platform: String
    let token: String
}

private struct PushTokenRevocation: Encodable, Sendable {
    let tokenId: String
}

private struct VoiceGenerationRequest: Encodable {
    var generation: Int64
}

private struct VoiceHangupRequest: Encodable {
    var mediaClosed: Bool
    var generation: Int64
    var accountLeaseId: String
    var accountLeaseGeneration: Int64
}
