import XCTest
@testable import ZoeKit
@testable import ZoeUI
import Foundation
import CryptoKit
import WebRTC

private final class ExportURLProtocol: URLProtocol {
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

private struct StaticExportCredential: BearerTokenProviding {
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
    func testMemoryInputValidationTrimsAndRejectsEmptyOrOverLengthFields() {
        XCTAssertEqual(MemoryInputValidation.validate(subject: " Subject ", key: " Key\n", content: " Content "), MemoryInput(subject: "Subject", key: "Key", content: "Content"))
        XCTAssertNil(MemoryInputValidation.validate(subject: " ", key: "key", content: "content"))
        XCTAssertNil(MemoryInputValidation.validate(subject: String(repeating: "s", count: 81), key: "key", content: "content"))
        XCTAssertNil(MemoryInputValidation.validate(subject: "subject", key: String(repeating: "k", count: 81), content: "content"))
        XCTAssertNil(MemoryInputValidation.validate(subject: "subject", key: "key", content: String(repeating: "c", count: 2001)))
    }

    @MainActor
    func testRememberPersonalScopeSendsUserNotePayload() async throws {
        var postedPayload: [String: Any]?
        ExportURLProtocol.responseHandler = { request in
            if request.httpMethod == "POST" {
                postedPayload = (try? JSONSerialization.jsonObject(with: Self.requestBody(request) ?? Data())) as? [String: Any]
                return (200, ["Content-Type": "application/json"], Self.commandReceipt(key: request.value(forHTTPHeaderField: "Idempotency-Key") ?? "", kind: "memory.remember", workspaceID: "workspace-1", resourceID: "memory-1", resourceType: "memory"))
            }
            return (500, [:], Data())
        }
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let emptySnapshot = conversationSnapshot([])
        await model.preparePendingCheckForTesting(api: api, journal: try CommandJournal(fileURL: FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)), snapshot: emptySnapshot)
        model.snapshotFetchOverrideForTesting = { emptySnapshot }
        model.memorySearchOverrideForTesting = { _ in MemorySearchResult(items: [], missing: [], conflicts: []) }

        let accepted = await model.remember(scope: "personal", scopeID: nil, subject: "subject", key: "key", content: "content")
        XCTAssertTrue(accepted)
        let payload = try XCTUnwrap(postedPayload?["payload"] as? [String: Any])
        XCTAssertEqual((payload["scope"] as? [String: Any])?["kind"] as? String, "personal")
        XCTAssertEqual(payload["subject"] as? String, "subject")
        XCTAssertEqual(payload["key"] as? String, "key")
        XCTAssertEqual(payload["content"] as? String, "content")
        XCTAssertEqual(payload["source_kind"] as? String, "user_note")
    }
    private func exportSession() -> URLSession {
        ExportURLProtocol.failureHandler = nil
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [ExportURLProtocol.self]
        return URLSession(configuration: configuration)
    }

    private func exportPackage(scope: String = "personal", schemaVersion: Int = 2, contents: Data? = nil, snapshotCursor: String = "17", validatedCursor: String = "18", auditCursor: String = "18", auditID: String = "export-1", auditType: String = "data.export.prepared") -> (Data, [String: String]) {
        let contents = contents ?? Data(#"{"projects":[],"tasks":[],"task_dependencies":[],"task_entries":[],"approvals":[],"runs":[],"evidence":[],"memory":[]}"#.utf8)
        let manifest: [String: Any] = [
            "format": "zoe.authority-export", "schema_version": schemaVersion,
            "export_id": "export-1", "created_at": "2026-10-21T12:00:00Z",
            "workspace_id": "workspace-1", "subject_id": "device-1", "scope": scope,
            "authority": ["instance_lineage_id": "lineage-1", "generation": 3, "deletion_sequence": "4", "snapshot_event_cursor": snapshotCursor, "validated_through_cursor": validatedCursor, "export_audit_event_cursor": auditCursor, "export_audit_event_id": auditID, "export_audit_event_type": auditType, "source": "sqlite_authority"],
            "integrity": ["data_bytes": contents.count, "data_sha256": Self.sha256(contents)],
            "coverage": [:]
        ]
        let manifestBytes = try! JSONSerialization.data(withJSONObject: manifest, options: [.sortedKeys])
        let archive = Self.storedZip(entries: [("manifest.json", manifestBytes), ("data.json", contents)])
        return (archive, [
            "Content-Type": "application/vnd.zoe.export+zip",
            "Content-Disposition": "attachment; filename=\"zoe-export-exp-1.zip\"",
            "X-Zoe-Archive-SHA256": Self.sha256(archive),
            "X-Zoe-Manifest-SHA256": Self.sha256(manifestBytes)
        ])
    }

    private static func sha256(_ data: Data) -> String {
        SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    private static func storedZip(entries: [(String, Data)]) -> Data {
        func u16(_ value: UInt16) -> Data { Data([UInt8(value & 0xff), UInt8((value >> 8) & 0xff)]) }
        func u32(_ value: UInt32) -> Data { Data([UInt8(value & 0xff), UInt8((value >> 8) & 0xff), UInt8((value >> 16) & 0xff), UInt8((value >> 24) & 0xff)]) }
        func crc(_ data: Data) -> UInt32 {
            let table = (0..<256).map { value -> UInt32 in
                var entry = UInt32(value)
                for _ in 0..<8 { entry = entry & 1 == 1 ? (entry >> 1) ^ 0xedb8_8320 : entry >> 1 }
                return entry
            }
            var value: UInt32 = 0xffff_ffff
            for byte in data {
                let index = Int((value ^ UInt32(byte)) & 0xff)
                value = table[index] ^ (value >> 8)
            }
            return ~value
        }
        var result = Data()
        var central = Data()
        for (name, bytes) in entries {
            let nameBytes = Data(name.utf8)
            let checksum = crc(bytes)
            let localOffset = UInt32(result.count)
            result.append(u32(0x04034b50)); result.append(u16(20)); result.append(u16(8)); result.append(u16(0))
            result.append(u16(0)); result.append(u16(0)); result.append(u32(0)); result.append(u32(0))
            result.append(u32(0)); result.append(u16(UInt16(nameBytes.count))); result.append(u16(0)); result.append(nameBytes); result.append(bytes)
            result.append(u32(0x08074b50)); result.append(u32(checksum)); result.append(u32(UInt32(bytes.count))); result.append(u32(UInt32(bytes.count)))
            central.append(u32(0x02014b50)); central.append(u16(20)); central.append(u16(20)); central.append(u16(8)); central.append(u16(0))
            central.append(u16(0)); central.append(u16(0)); central.append(u32(checksum)); central.append(u32(UInt32(bytes.count)))
            central.append(u32(UInt32(bytes.count))); central.append(u16(UInt16(nameBytes.count))); central.append(u16(0)); central.append(u16(0))
            central.append(u16(0)); central.append(u16(0)); central.append(u32(0)); central.append(u32(localOffset)); central.append(nameBytes)
        }
        let directoryOffset = UInt32(result.count)
        result.append(central)
        result.append(u32(0x06054b50)); result.append(u16(0)); result.append(u16(0)); result.append(u16(UInt16(entries.count)))
        result.append(u16(UInt16(entries.count))); result.append(u32(UInt32(central.count))); result.append(u32(directoryOffset)); result.append(u16(0))
        return result
    }

    private func memoryClaim(id: String, content: String) -> MemoryClaim {
        MemoryClaim(id: id, workspaceId: "workspace-1", scope: MemoryScope(kind: "personal", id: nil, principalId: "subject-1"), subject: "preference", key: "format", content: content, authorityKind: "user_confirmed", status: "active", revision: 1, validFrom: nil, validTo: nil, supersedes: nil, sources: [], deletedAt: nil)
    }

    func testScopedExportUsesAuthenticatedScopedRequestAndPrivateTemporaryFile() async throws {
        ExportURLProtocol.capturedRequest = nil
        let (package, headers) = exportPackage()
        ExportURLProtocol.responseHandler = { _ in (200, headers, package) }
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let client = try APIClient(serverURL: URL(string: "https://zoe.example/base")!, credentials: StaticExportCredential(), session: exportSession())

        let artifact = try await client.downloadExport(scope: "personal", temporaryDirectory: directory)

        let request = try XCTUnwrap(ExportURLProtocol.capturedRequest)
        XCTAssertEqual(request.httpMethod, "GET")
        XCTAssertEqual(URLComponents(url: try XCTUnwrap(request.url), resolvingAgainstBaseURL: false)?.path, "/base/v1/export")
        XCTAssertEqual(URLComponents(url: try XCTUnwrap(request.url), resolvingAgainstBaseURL: false)?.queryItems, [URLQueryItem(name: "scope", value: "personal")])
        XCTAssertEqual(request.value(forHTTPHeaderField: "Accept"), "application/vnd.zoe.export+zip")
        XCTAssertTrue(try XCTUnwrap(request.value(forHTTPHeaderField: "Authorization")).hasPrefix("Bearer "))
        XCTAssertEqual(artifact.scope, "personal")
        XCTAssertEqual(artifact.filename, "zoe-export-exp-1.zip")
        XCTAssertEqual(artifact.snapshotCursor, "17")
        XCTAssertEqual(artifact.validatedThroughCursor, "18")
        XCTAssertEqual(artifact.exportAuditEventCursor, "18")
        XCTAssertEqual(artifact.exportAuditEventID, "export-1")
        XCTAssertEqual(artifact.exportAuditEventType, "data.export.prepared")
        XCTAssertEqual(artifact.instanceLineageID, "lineage-1")
        XCTAssertEqual(artifact.generation, 3)
        XCTAssertEqual(artifact.deletionSequence, "4")
        XCTAssertEqual(try Data(contentsOf: artifact.fileURL), package)
        let attributes = try FileManager.default.attributesOfItem(atPath: artifact.fileURL.path)
        XCTAssertEqual((attributes[.posixPermissions] as? NSNumber)?.intValue, 0o600)
        try FileManager.default.removeItem(at: artifact.fileURL)
    }

    @MainActor
    func testExportDownloadRefreshAndSaveAcceptPreparedAuditCursorThenRejectChangedAuthority() async throws {
        let (package, headers) = exportPackage()
        ExportURLProtocol.responseHandler = { request in
            request.url?.path == "/v1/export" ? (200, headers, package) : (500, [:], Data())
        }
        let client = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        defer { model.cancelExport() }
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        var latest = Snapshot(serverVersion: "srv-1", eventCursor: "18", projects: [], tasks: [], approvals: [], runs: [], workspaceId: "workspace-1", authority: SnapshotAuthority(instanceLineageId: "lineage-1", generation: 3, deletionSequence: "4"))
        model.snapshotFetchOverrideForTesting = { latest }
        await model.recoverPendingCommandsForTesting(api: client, journal: journal)

        await model.prepareExport(scope: "personal", expectedConnectionEpoch: model.connectionEpoch)

        let artifact = try XCTUnwrap(model.exportArtifact)
        XCTAssertEqual(artifact.snapshotCursor, "17", "the data image cursor remains distinct from the audit cursor")
        XCTAssertEqual(artifact.validatedThroughCursor, "18")
        let stillAuthorized = await model.validateExportForSave(expectedConnectionEpoch: model.connectionEpoch)
        XCTAssertTrue(stillAuthorized, "the matching prepared audit event is the sole allowed cursor advance")
        XCTAssertTrue(FileManager.default.fileExists(atPath: artifact.fileURL.path))

        latest = Snapshot(serverVersion: "srv-2", eventCursor: "18", projects: [], tasks: [], approvals: [], runs: [], workspaceId: "workspace-1", authority: SnapshotAuthority(instanceLineageId: "lineage-1", generation: 3, deletionSequence: "5"))
        let revoked = await model.validateExportForSave(expectedConnectionEpoch: model.connectionEpoch)
        XCTAssertFalse(revoked, "changed deletion authority must invalidate the copy")
        XCTAssertNil(model.exportArtifact)
        XCTAssertFalse(FileManager.default.fileExists(atPath: artifact.fileURL.path))
    }

    func testScopedExportRejectsInvalidScopeSchemaFilenameAndDeclaredOversize() async throws {
        let client = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        do { _ = try await client.downloadExport(scope: "all"); XCTFail("invalid scope accepted") } catch { if case ZoeError.invalidResponse = error {} else { XCTFail("unexpected error: \(error)") } }

        let (wrongVersion, wrongVersionHeaders) = exportPackage(schemaVersion: 3)
        let (wrongScope, wrongScopeHeaders) = exportPackage(scope: "workspace")
        let (wrongAuditCursor, wrongAuditCursorHeaders) = exportPackage(validatedCursor: "19")
        let (validPackage, validHeaders) = exportPackage()
        var invalidFilenameHeaders = validHeaders
        invalidFilenameHeaders["Content-Disposition"] = "attachment; filename=\"../zoe-export-x.zip\""
        for (headers, body) in [
            (wrongVersionHeaders, wrongVersion),
            (wrongAuditCursorHeaders, wrongAuditCursor),
            (invalidFilenameHeaders, validPackage),
            (wrongScopeHeaders, wrongScope)
        ] {
            ExportURLProtocol.responseHandler = { _ in (200, headers, body) }
            do { _ = try await client.downloadExport(scope: "personal"); XCTFail("invalid export accepted") }
            catch { if case ZoeError.invalidResponse = error {} else { XCTFail("unexpected error: \(error)") } }
        }

        ExportURLProtocol.responseHandler = { _ in
            (200, ["Content-Type": "application/vnd.zoe.export+zip", "Content-Disposition": "attachment; filename=\"zoe-export-x.zip\"", "Content-Length": String(1024 * 1024 * 1024 + 1)], Data())
        }
        do { _ = try await client.downloadExport(scope: "personal"); XCTFail("oversized export accepted") }
        catch { if case ZoeError.exportTooLarge = error {} else { XCTFail("unexpected error: \(error)") } }
    }

    func testBoundedDownloadCancelsWhenTransferExceedsConfiguredLimit() async throws {
        let body = Data(repeating: 0x61, count: 128 * 1024)
        ExportURLProtocol.responseHandler = { request in
            (200, ["Content-Length": String(body.count)], body)
        }
        let request = URLRequest(url: URL(string: "https://zoe.example/oversize")!)
        do {
            _ = try await downloadBounded(request, using: exportSession(), maximumBytes: 1024)
            XCTFail("over-limit transfer was accepted")
        } catch {
            if case ZoeError.exportTooLarge = error { }
            else { XCTFail("unexpected error: \(error)") }
        }
    }

    func testAbandonedDownloadCleanupPreservesActiveFileLock() throws {
        let directory = BoundedDownloadDelegate.temporaryDirectory
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let stem = "zoe-download-test-\(UUID().uuidString.lowercased())"
        let lockURL = directory.appendingPathComponent(stem + ".lock")
        let temporaryURL = directory.appendingPathComponent(stem + ".tmp")
        try Data("private archive bytes".utf8).write(to: temporaryURL)
        let activeLock = try DownloadFileLock(fileURL: lockURL, create: true)

        APIClient.cleanupAbandonedExportDownloads()
        XCTAssertTrue(FileManager.default.fileExists(atPath: temporaryURL.path), "cleanup must skip another active download lock")

        activeLock.release(removeFile: false)
        APIClient.cleanupAbandonedExportDownloads()
        XCTAssertFalse(FileManager.default.fileExists(atPath: temporaryURL.path), "cleanup must remove a crash-left archive after its lock is released")
        XCTAssertFalse(FileManager.default.fileExists(atPath: lockURL.path))
    }

    func testScopedExportStreamsArchivesLargerThan64MiB() async throws {
        let contents = Data(repeating: 0x61, count: 65 * 1024 * 1024)
        let (package, headers) = exportPackage(contents: contents)
        ExportURLProtocol.responseHandler = { _ in (200, headers, package) }
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let client = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())

        let artifact = try await client.downloadExport(scope: "personal", temporaryDirectory: directory)

        let size = (try FileManager.default.attributesOfItem(atPath: artifact.fileURL.path)[.size] as? NSNumber)?.uint64Value
        XCTAssertGreaterThan(size ?? 0, 64 * 1024 * 1024)
        try FileManager.default.removeItem(at: artifact.fileURL)
    }

    @MainActor
    func testSnapshotFailureRemovesDownloadedExportTemporaryFile() async throws {
        let temporaryDirectory = FileManager.default.temporaryDirectory
        let existing = Set((try FileManager.default.contentsOfDirectory(at: temporaryDirectory, includingPropertiesForKeys: nil))
            .filter { $0.lastPathComponent.hasPrefix("zoe-export-") && $0.pathExtension == "zip" })
        let (package, headers) = exportPackage()
        ExportURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/export" { return (200, headers, package) }
            return (503, ["Content-Type": "application/json"], Data())
        }
        let client = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let directory = temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        model.snapshotFetchOverrideForTesting = { throw ZoeError.invalidResponse }
        await model.recoverPendingCommandsForTesting(api: client, journal: journal)

        await model.prepareExport(scope: "personal", expectedConnectionEpoch: model.connectionEpoch)

        XCTAssertNil(model.exportArtifact)
        let after = Set((try FileManager.default.contentsOfDirectory(at: temporaryDirectory, includingPropertiesForKeys: nil))
            .filter { $0.lastPathComponent.hasPrefix("zoe-export-") && $0.pathExtension == "zip" })
        XCTAssertEqual(after, existing, "an export downloaded before a failed snapshot refresh must be removed")
    }

    @MainActor
    func testExportResultFromOldConnectionIsDeletedAndNotPresented() async throws {
        let defaults = UserDefaults(suiteName: UUID().uuidString)!
        let model = ZoeAppModel(defaults: defaults)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let gate = AsyncTestGate()
        var producedURL: URL?
        var shouldPause = true
        model.exportDownloadOverrideForTesting = { scope in
            XCTAssertEqual(scope, "personal")
            if shouldPause {
                shouldPause = false
                await gate.pause()
            }
            let url = directory.appendingPathComponent("late.json")
            try Data("{}".utf8).write(to: url)
            producedURL = url
            return ScopedExportArtifact(fileURL: url, filename: "zoe-export-late.json", scope: scope, snapshotCursor: "1")
        }
        let epoch = model.connectionEpoch
        let task = Task { await model.prepareExport(scope: "personal", expectedConnectionEpoch: epoch) }
        await gate.waitUntilStarted()
        model.serverAddress = ""
        await model.clearCredential()
        await gate.resume()
        await task.value
        XCTAssertNil(model.exportArtifact)
        XCTAssertNil(model.exportStatus, "switching connections should clear the stale Preparing status")
        XCTAssertFalse(FileManager.default.fileExists(atPath: try XCTUnwrap(producedURL).path))

        await model.prepareExport(scope: "personal", expectedConnectionEpoch: model.connectionEpoch)
        XCTAssertEqual(model.exportArtifact?.filename, "zoe-export-late.json", "a later request on the new epoch should be allowed")
        model.cancelExport()
    }

    @MainActor
    func testExportSaveAndCancelRemovePreparedTemporaryFile() throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let fileURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data("{}".utf8).write(to: fileURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: fileURL, filename: "zoe-export-one.json", scope: "personal", snapshotCursor: "1"))

        model.exportSaveFinished(saved: false)

        XCTAssertNil(model.exportArtifact)
        XCTAssertFalse(FileManager.default.fileExists(atPath: fileURL.path))
        XCTAssertEqual(model.exportStatus, uiText("Saving was canceled. The prepared temporary file was removed."))

        let secondURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data("{}".utf8).write(to: secondURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: secondURL, filename: "zoe-export-two.json", scope: "workspace", snapshotCursor: "2"))
        model.cancelExport()

        XCTAssertNil(model.exportArtifact)
        XCTAssertFalse(FileManager.default.fileExists(atPath: secondURL.path))
    }

    @MainActor
    func testCanceledPrepareTaskCleansLateFileAndCanBeRetried() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let gate = AsyncTestGate()
        var shouldPause = true
        var producedURL: URL?
        model.exportDownloadOverrideForTesting = { scope in
            if shouldPause {
                shouldPause = false
                await gate.pause()
            }
            let url = directory.appendingPathComponent("task-cancelled.json")
            try Data("{}".utf8).write(to: url)
            producedURL = url
            return ScopedExportArtifact(fileURL: url, filename: "zoe-export-task-cancelled.json", scope: scope, snapshotCursor: "1")
        }

        let task = Task { await model.prepareExport(scope: "personal", expectedConnectionEpoch: model.connectionEpoch) }
        await gate.waitUntilStarted()
        task.cancel()
        await gate.resume()
        await task.value

        XCTAssertNil(model.exportArtifact)
        XCTAssertNotEqual(model.exportStatus, uiText("Preparing the authorized export…"))
        XCTAssertFalse(FileManager.default.fileExists(atPath: try XCTUnwrap(producedURL).path))
        await model.prepareExport(scope: "personal", expectedConnectionEpoch: model.connectionEpoch)
        XCTAssertEqual(model.exportArtifact?.filename, "zoe-export-task-cancelled.json")
        model.cancelExport()
    }

    @MainActor
    func testMemoryReadAuthorizationFailureClearsCachedClaimsAndPreparedExport() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setMemoryClaimsForTesting([memoryClaim(id: "memory-old", content: "stale private content")])
        let fileURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data("export containing stale memory".utf8).write(to: fileURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: fileURL, filename: "zoe-export-old.json", scope: "personal", snapshotCursor: "5"))
        model.memorySearchOverrideForTesting = { _ in
            throw ZoeError.http(status: 403, code: "forbidden", message: "memory read denied", details: nil)
        }

        await model.refreshMemory()

        XCTAssertTrue(model.memoryClaims.isEmpty)
        XCTAssertTrue(model.memoryMissing.isEmpty)
        XCTAssertTrue(model.memoryConflicts.isEmpty)
        XCTAssertNil(model.exportArtifact)
        XCTAssertFalse(FileManager.default.fileExists(atPath: fileURL.path))
        XCTAssertEqual(model.exportStatus, uiText("Memory access was denied; cached memory and the prepared export were cleared."))
    }

    @MainActor
    func testSnapshotAuthorizationFailureClearsMemoryAndPreparedExport() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setMemoryClaimsForTesting([memoryClaim(id: "memory-old", content: "stale private content")])
        let fileURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data("export containing stale memory".utf8).write(to: fileURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: fileURL, filename: "zoe-export-old.json", scope: "personal", snapshotCursor: "5"))
        model.snapshotFetchOverrideForTesting = {
            throw ZoeError.http(status: 403, code: "forbidden", message: "snapshot denied", details: nil)
        }

        do { try await model.refreshSnapshotForTesting(); XCTFail("forbidden snapshot accepted") }
        catch {
            if let failure = error as? ZoeError, case let .http(status, _, _, _) = failure, status == 403 {} else {
                XCTFail("unexpected error: \(error)")
            }
        }

        XCTAssertTrue(model.memoryClaims.isEmpty)
        XCTAssertNil(model.exportArtifact)
        XCTAssertFalse(FileManager.default.fileExists(atPath: fileURL.path))
    }

    @MainActor
    func testDeletionSequenceChangeInvalidatesPreparedExportAndPrivateFile() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let fileURL = FileManager.default.temporaryDirectory.appendingPathComponent("zoe-export-authority.zip")
        try Data("archive".utf8).write(to: fileURL)
        let originalAuthority = SnapshotAuthority(instanceLineageId: "lineage-1", generation: 5, deletionSequence: "8")
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "17", projects: [], tasks: [], approvals: [], runs: [], workspaceId: "workspace-1", authority: originalAuthority))
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: fileURL, filename: "zoe-export-authority.zip", scope: "personal", snapshotCursor: "17", instanceLineageID: "lineage-1", generation: 5, deletionSequence: "8"))
        model.snapshotFetchOverrideForTesting = {
            Snapshot(serverVersion: "1", eventCursor: "18", projects: [], tasks: [], approvals: [], runs: [], workspaceId: "workspace-1", authority: SnapshotAuthority(instanceLineageId: "lineage-1", generation: 5, deletionSequence: "9"))
        }

        try await model.refreshSnapshotForTesting()

        XCTAssertNil(model.exportArtifact)
        XCTAssertFalse(FileManager.default.fileExists(atPath: fileURL.path))
        XCTAssertEqual(model.exportStatus, uiText("Server authority changed; cached memory and the prepared export were cleared."))
    }

    @MainActor
    func testConnectionChangeClearsMemoryEvenWhenNewAddressIsInvalid() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setMemoryClaimsForTesting([memoryClaim(id: "memory-old-origin", content: "old origin private content")])
        model.serverAddress = "invalid-url"

        await model.clearCredential()

        XCTAssertTrue(model.memoryClaims.isEmpty)
        XCTAssertTrue(model.memoryMissing.isEmpty)
        XCTAssertTrue(model.memoryConflicts.isEmpty)
    }

    @MainActor
    func testMemoryEventClearsStaleClaimAndPreparedExportBeforeRefreshing() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setMemoryClaimsForTesting([memoryClaim(id: "memory-deleted", content: "deleted secret")])
        let fileURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data("export containing deleted memory".utf8).write(to: fileURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: fileURL, filename: "zoe-export-old.json", scope: "personal", snapshotCursor: "5"))
        model.memorySearchOverrideForTesting = { _ in MemorySearchResult(items: [self.memoryClaim(id: "memory-current", content: "current result")], missing: [], conflicts: []) }

        await model.handleEventForTesting(EventEnvelope(schemaVersion: 1, eventId: "event-1", cursor: "6", type: "memory.deleted", occurredAt: "2026-10-21T12:00:00Z", workspaceId: "workspace-1", subjectId: "subject-1", aggregateType: "memory", aggregateId: "memory-deleted", aggregateVersion: 2, payload: .object([:])))

        XCTAssertEqual(model.memoryClaims.map(\.id), ["memory-current"])
        XCTAssertNil(model.exportArtifact)
        XCTAssertFalse(FileManager.default.fileExists(atPath: fileURL.path))
    }

    @MainActor
    func testPreparedExportAuditEventDoesNotInvalidateItsOwnArtifact() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let fileURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data("prepared export".utf8).write(to: fileURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: fileURL, filename: "zoe-export-one.json", scope: "personal", snapshotCursor: "5", exportID: "export-one", validatedThroughCursor: "6", exportAuditEventCursor: "6", exportAuditEventID: "export-one", exportAuditEventType: "data.export.prepared"))
        model.snapshotFetchOverrideForTesting = {
            Snapshot(serverVersion: "srv-1", eventCursor: "6", projects: [], tasks: [], approvals: [], runs: [], workspaceId: "workspace-1", authority: SnapshotAuthority(instanceLineageId: "", generation: 0, deletionSequence: "0"))
        }

        await model.handleEventForTesting(EventEnvelope(schemaVersion: 1, eventId: "export-one", cursor: "6", type: "data.export.prepared", occurredAt: "2026-10-21T12:00:00Z", workspaceId: "workspace-1", subjectId: "subject-1", aggregateType: "data_export", aggregateId: "export-one", aggregateVersion: 1, payload: .object([:])))

        XCTAssertNotNil(model.exportArtifact)
        XCTAssertTrue(FileManager.default.fileExists(atPath: fileURL.path))
        model.cancelExport()
    }

    @MainActor
    func testPermissionChangeRemovesPreparedExportEvenWhenSnapshotRefreshFails() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let fileURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data("prepared export".utf8).write(to: fileURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: fileURL, filename: "zoe-export-one.json", scope: "personal", snapshotCursor: "5"))
        model.snapshotFetchOverrideForTesting = { throw ZoeError.invalidResponse }

        await model.handleEventForTesting(EventEnvelope(schemaVersion: 1, eventId: "event-revoked", cursor: "6", type: "instance.key.rotated", occurredAt: "2026-10-21T12:00:00Z", workspaceId: "workspace-1", subjectId: "owner", aggregateType: "instance", aggregateId: "default", aggregateVersion: 2, payload: .object([:])))

        XCTAssertNil(model.exportArtifact)
        XCTAssertFalse(FileManager.default.fileExists(atPath: fileURL.path))
    }

    @MainActor
    func testDelayedOldConnectionMemoryEventCannotInvalidateNewConnectionData() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let oldEpoch = model.connectionEpoch
        model.serverAddress = "invalid-url"
        await model.clearCredential()
        let fileURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data("new connection export".utf8).write(to: fileURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: fileURL, filename: "zoe-export-new.json", scope: "personal", snapshotCursor: "1"))
        model.setMemoryClaimsForTesting([memoryClaim(id: "memory-new", content: "new connection claim")])

        await model.handleEventForTesting(EventEnvelope(schemaVersion: 1, eventId: "old-event", cursor: "2", type: "memory.deleted", occurredAt: "2026-10-21T12:00:00Z", workspaceId: "workspace-old", subjectId: "subject-old", aggregateType: "memory", aggregateId: "memory-old", aggregateVersion: 2, payload: .object([:])), expectedConnectionEpoch: oldEpoch)

        XCTAssertEqual(model.memoryClaims.map(\.id), ["memory-new"])
        XCTAssertEqual(model.exportArtifact?.filename, "zoe-export-new.json")
        XCTAssertTrue(FileManager.default.fileExists(atPath: fileURL.path))
        model.cancelExport()
    }

    @MainActor
    func testRecoveredMemoryDeleteReceiptReplaysJournalAndInvalidatesPreparedExport() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setMemoryClaimsForTesting([memoryClaim(id: "memory-before-replay", content: "before replay")])
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let artifactURL = directory.appendingPathComponent("private-export.json")
        try Data("export containing deleted memory".utf8).write(to: artifactURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: artifactURL, filename: "zoe-export-private.json", scope: "personal", snapshotCursor: "7"))

        let key = "recovery-key-\(UUID().uuidString.lowercased())"
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "memory.delete", expectedVersion: 2, payload: .object(["memory_id": .string("memory-1")]), createdAt: "2026-10-21T12:00:00Z", workspaceID: "workspace-1", expectedResourceType: "memory", expectedResourceID: "memory-1"))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        var observedReceiptLookup = false
        ExportURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/commands", request.httpMethod == "GET", URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.contains(where: { $0.name == "key" && $0.value == key }) == true {
                observedReceiptLookup = true
                return (200, ["Content-Type": "application/json"], Data("{\"command_id\":\"cmd-recovered\",\"idempotency_key\":\"\(key)\",\"workspace_id\":\"workspace-1\",\"kind\":\"memory.delete\",\"status\":\"accepted\",\"resource_type\":\"memory\",\"resource_id\":\"memory-1\",\"resource_version\":3,\"accepted_at\":\"2026-10-21T12:00:00Z\",\"event_cursor\":\"8\"}".utf8))
            }
            return (503, ["Content-Type": "application/json"], Data())
        }
        var observedQueries: [String] = []
        model.memorySearchOverrideForTesting = { query in
            observedQueries.append(query)
            return MemorySearchResult(items: [], missing: [], conflicts: [])
        }

        await model.recoverPendingCommandsForTesting(api: api, journal: journal)

        let remainingCommands = try await journal.all()
        XCTAssertTrue(observedReceiptLookup, "recovery must resolve the persisted idempotency key through the receipt endpoint")
        XCTAssertTrue(remainingCommands.isEmpty, "an accepted recovered receipt must remove its journal row")
        XCTAssertEqual(observedQueries, [""])
        XCTAssertTrue(model.memoryClaims.isEmpty)
        XCTAssertNil(model.exportArtifact, "a recovered memory deletion must invalidate a prepared private export")
        XCTAssertFalse(FileManager.default.fileExists(atPath: artifactURL.path), "the invalidated private export file must be removed")
    }

    @MainActor
    func testRecoveredMemoryReceiptFromOldGenerationCannotInvalidateNewExport() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let artifactURL = directory.appendingPathComponent("new-private-export.json")
        try Data("new connection export".utf8).write(to: artifactURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: artifactURL, filename: "zoe-export-new.json", scope: "personal", snapshotCursor: "9"))

        let key = "old-recovery-key-\(UUID().uuidString.lowercased())"
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "memory.delete", expectedVersion: 2, payload: .object(["memory_id": .string("memory-1")]), createdAt: "2026-10-21T12:00:00Z", workspaceID: "workspace-1", expectedResourceType: "memory", expectedResourceID: "memory-1"))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        ExportURLProtocol.responseHandler = { request in
            guard request.url?.path == "/v1/commands" else { return (503, [:], Data()) }
            return (200, ["Content-Type": "application/json"], Data("{\"command_id\":\"cmd-old\",\"idempotency_key\":\"\(key)\",\"workspace_id\":\"workspace-1\",\"kind\":\"memory.delete\",\"status\":\"accepted\",\"resource_type\":\"memory\",\"resource_id\":\"memory-1\",\"resource_version\":3,\"accepted_at\":\"2026-10-21T12:00:00Z\",\"event_cursor\":\"8\"}".utf8))
        }

        await model.recoverPendingCommandsForTesting(api: api, journal: journal, expectedGeneration: model.connectionEpoch &- 1)

        XCTAssertEqual(model.exportArtifact?.filename, "zoe-export-new.json")
        XCTAssertTrue(FileManager.default.fileExists(atPath: artifactURL.path))
    }

    @MainActor
    func testNewMemoryQueryFencesOldInFlightResultAndCoalescesToLatestQuery() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setMemoryClaimsForTesting([memoryClaim(id: "memory-before", content: "before")])
        let gate = AsyncTestGate()
        var observedQueries: [String] = []
        model.memorySearchOverrideForTesting = { query in
            observedQueries.append(query)
            if query == "old" { await gate.pause() }
            return MemorySearchResult(items: [self.memoryClaim(id: "memory-\(query)", content: query)], missing: [], conflicts: [])
        }

        let oldSearch = Task { await model.refreshMemory(query: "old") }
        await gate.waitUntilStarted()
        await model.refreshMemory(query: "new")
        await gate.resume()
        await oldSearch.value

        XCTAssertEqual(observedQueries, ["old", "new"])
        XCTAssertEqual(model.memoryClaims.map(\.id), ["memory-new"])
    }

    @MainActor
    func testMemoryQueryFromPreviousConnectionCannotRepopulateClearedClaims() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setMemoryClaimsForTesting([memoryClaim(id: "memory-before", content: "before")])
        let gate = AsyncTestGate()
        model.memorySearchOverrideForTesting = { _ in
            await gate.pause()
            return MemorySearchResult(items: [self.memoryClaim(id: "memory-old-origin", content: "old origin result")], missing: [], conflicts: [])
        }

        let oldSearch = Task { await model.refreshMemory() }
        await gate.waitUntilStarted()
        model.serverAddress = "invalid-url"
        await model.clearCredential()
        await gate.resume()
        await oldSearch.value

        XCTAssertTrue(model.memoryClaims.isEmpty)
    }

    @MainActor
    func testMemoryEventFencesAndDeletesInFlightPreparedExport() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let gate = AsyncTestGate()
        var producedURL: URL?
        model.exportDownloadOverrideForTesting = { scope in
            await gate.pause()
            let url = directory.appendingPathComponent("stale-export.json")
            try Data("stale export".utf8).write(to: url)
            producedURL = url
            return ScopedExportArtifact(fileURL: url, filename: "zoe-export-stale.json", scope: scope, snapshotCursor: "5")
        }
        model.memorySearchOverrideForTesting = { _ in MemorySearchResult(items: [], missing: [], conflicts: []) }

        let preparing = Task { await model.prepareExport(scope: "personal", expectedConnectionEpoch: model.connectionEpoch) }
        await gate.waitUntilStarted()
        await model.handleEventForTesting(EventEnvelope(schemaVersion: 1, eventId: "event-delete", cursor: "6", type: "memory.deleted", occurredAt: "2026-10-21T12:00:00Z", workspaceId: "workspace-1", subjectId: "subject-1", aggregateType: "memory", aggregateId: "memory-1", aggregateVersion: 2, payload: .object([:])))
        await gate.resume()
        await preparing.value

        XCTAssertNil(model.exportArtifact)
        XCTAssertFalse(FileManager.default.fileExists(atPath: try XCTUnwrap(producedURL).path))
        XCTAssertEqual(model.exportStatus, uiText("Server memory changed; prepare a fresh export."))
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

    func testExecutionCapacityAndQueueReasonDecodeWithoutRequiringNewFieldsOnOlderServers() throws {
        let task: [String: Any] = ["id": "queued", "title": "Work", "goal": "Work", "source": "user", "status": "queued", "version": 1, "created_at": "now", "updated_at": "now", "evidence": [], "can_accept_result": false]
        var body: [String: Any] = ["server_version": "1", "event_cursor": "1", "workspace_id": "default", "subject_id": "owner", "projects": [], "tasks": [task], "approvals": [], "runs": []]
        let legacy = try ZoeJSON.decoder.decode(Snapshot.self, from: JSONSerialization.data(withJSONObject: body))
        XCTAssertNil(legacy.executionCapacity)
        XCTAssertNil(legacy.tasks.first?.queueReason)
        var queued = task
        queued["queue_reason"] = "execution_reconciliation_required"
        body["tasks"] = [queued]
        body["execution_capacity"] = ["active_runs": 1, "uncertain_runs": 3, "max_runs": 4, "available_slots": 0, "dispatch_blocked": true, "reason_code": "execution_reconciliation_required"]
        let blocked = try ZoeJSON.decoder.decode(Snapshot.self, from: JSONSerialization.data(withJSONObject: body))
        XCTAssertEqual(blocked.executionCapacity?.occupiedSlots, 4)
        XCTAssertEqual(blocked.executionCapacity?.availableSlots, 0)
        XCTAssertEqual(blocked.tasks.first?.status, .queued)
        XCTAssertEqual(blocked.tasks.first?.queueReason, "execution_reconciliation_required")
    }

    func testSnapshotDecodesAuthoritativeApprovalsAndRuns() throws {
        let data = Data(#"{"server_version":"srv-3","event_cursor":"19","projects":[],"tasks":[{"id":"task-1","title":"Review","goal":"Review changes","source":"user","status":"awaiting_user","version":4,"created_at":"2026-10-20T12:00:00Z","updated_at":"2026-10-20T12:01:00Z","evidence":[],"can_accept_result":true}],"approvals":[{"id":"approval-1","task_id":"task-1","status":"pending","prompt":"Approve deployment?","version":2,"created_at":"2026-10-20T12:00:00Z","requested_action_digest":"sha256:example"}],"workspace_id":"default","subject_id":"owner","runs":[{"id":"run-1","task_id":"task-1","attempt_id":"attempt-1","status":"verification_pending","desired_control":"stop","control_version":1,"started_at":"2026-10-20T12:00:00Z","evidence":[],"termination_confirmed":true}]}"#.utf8)
        let snapshot = try ZoeJSON.decoder.decode(Snapshot.self, from: data)
        XCTAssertEqual(snapshot.eventCursor, "19")
        XCTAssertEqual(snapshot.tasks.first?.status, .awaitingUser)
        XCTAssertEqual(snapshot.approvals.first?.id, "approval-1")
        XCTAssertEqual(snapshot.runs.first?.desiredControl, "stop")
        XCTAssertEqual(snapshot.tasks.first?.canAcceptResult, true)
        XCTAssertEqual(snapshot.runs.first?.terminationConfirmed, true)
    }

    func testCapabilityInventoryKeepsDesiredAndObservedDeploymentStatesSeparate() throws {
        let data = Data(#"{"items":[{"deployment_id":"deploy-1","workspace_id":"workspace-1","service_id":"service-1","release_digest":"sha256:release","requested_operation":"start","desired_state":"running","observed_state":"pending","grant_id":"grant-1","config_ref":"","config_sha256":"","version":2,"created_by":"device-1","created_at":"2026-10-21T12:00:00Z","updated_at":"2026-10-21T12:01:00Z","latest_attempt_id":"attempt-1","latest_fence":3,"last_error_code":""}]}"#.utf8)
        let inventory = try ZoeJSON.decoder.decode(CapabilityDeploymentList.self, from: data)
        XCTAssertEqual(inventory.items.first?.desiredState, "running")
        XCTAssertEqual(inventory.items.first?.observedState, "pending")
        XCTAssertEqual(inventory.items.first?.latestFence, 3)
    }

    func testCapabilityRevocationKeepsRunnerCleanupOutcomeSeparate() throws {
        func deployment(revokedAt: Any, revokedBy: Any, cleanup: Any = NSNull(), desired: String, observed: String) throws -> CapabilityDeployment {
            let item: [String: Any] = [
                "deployment_id": "deploy-1",
                "workspace_id": "workspace-1",
                "service_id": "service-1",
                "release_digest": "sha256:release",
                "requested_operation": "start",
                "desired_state": desired,
                "observed_state": observed,
                "grant_id": "grant-1",
                "config_ref": "",
                "config_sha256": "",
                "version": 8,
                "created_by": "owner-1",
                "created_at": "2026-10-21T12:00:00Z",
                "updated_at": "2026-10-21T12:01:00Z",
                "latest_attempt_id": "attempt-1",
                "latest_fence": 4,
                "last_error_code": "",
                "revoked_at": revokedAt,
                "revoked_by": revokedBy,
                "revocation_cleanup_state": cleanup
            ]
            let data = try JSONSerialization.data(withJSONObject: ["items": [item]])
            return try XCTUnwrap(ZoeJSON.decoder.decode(CapabilityDeploymentList.self, from: data).items.first)
        }

        let active = try deployment(revokedAt: NSNull(), revokedBy: NSNull(), desired: "running", observed: "running")
        XCTAssertNil(active.revokedAt)
        XCTAssertNil(active.revokedBy)
        XCTAssertNil(active.revocationCleanupStatus)

        let cleanupPending = try deployment(revokedAt: "2026-10-21T12:02:00Z", revokedBy: "owner-1", cleanup: "pending", desired: "stopped", observed: "running")
        XCTAssertEqual(cleanupPending.revokedAt, "2026-10-21T12:02:00Z")
        XCTAssertEqual(cleanupPending.revokedBy, "owner-1")
        XCTAssertEqual(cleanupPending.revocationCleanupStatus, .pending(observedState: "running"))

        let cleanupUncertain = try deployment(revokedAt: "2026-10-21T12:02:00Z", revokedBy: "owner-1", cleanup: "uncertain", desired: "stopped", observed: "uncertain")
        XCTAssertEqual(cleanupUncertain.revocationCleanupStatus, .uncertain)

        let cleanupUnknown = try deployment(revokedAt: "2026-10-21T12:02:00Z", revokedBy: "owner-1", desired: "stopped", observed: "unknown")
        XCTAssertEqual(cleanupUnknown.revocationCleanupStatus, .unknown(observedState: "unknown"))
        let futureCleanupState = try deployment(revokedAt: "2026-10-21T12:02:00Z", revokedBy: "owner-1", cleanup: "future_state", desired: "stopped", observed: "stopped")
        XCTAssertEqual(futureCleanupState.revocationCleanupStatus, .unknown(observedState: "stopped"))

        let previouslyStopped = try deployment(revokedAt: "2026-10-21T12:02:00Z", revokedBy: "owner-1", desired: "stopped", observed: "stopped")
        XCTAssertEqual(previouslyStopped.revocationCleanupStatus, .unknown(observedState: "stopped"), "A stopped observation without cleanup proof may predate revocation.")

        let cleanupConfirmed = try deployment(revokedAt: "2026-10-21T12:02:00Z", revokedBy: "owner-1", cleanup: "confirmed", desired: "stopped", observed: "stopped")
        XCTAssertEqual(cleanupConfirmed.revocationCleanupStatus, .confirmed)
    }

    @MainActor
    func testRevocationReturnsFalseWhenConnectionChangesWhileCommandIsSuspended() async throws {
        let deployment = try makeRevocationDeployment(observed: "stopped")
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setCapabilityDeploymentsForTesting([deployment])
        let submitGate = AsyncTestGate()
        var didRefresh = false
        model.capabilityRevocationSubmitOverrideForTesting = { version, payload in
            XCTAssertEqual(version, 8)
            XCTAssertEqual(payload, .object(["deployment_id": .string("deploy-1")]))
            await submitGate.pause()
            return true
        }
        model.capabilityRevocationRefreshOverrideForTesting = { didRefresh = true }

        let epoch = model.connectionEpoch
        let task = Task { await model.revokeCapabilityDeployment(deployment, expectedConnectionEpoch: epoch) }
        await submitGate.waitUntilStarted()
        model.serverAddress = ""
        await model.clearCredential()
        await submitGate.resume()

        let result = await task.value
        XCTAssertFalse(result)
        XCTAssertNotEqual(model.notice, uiText("Authorization revoked. Stop cleanup status is shown in the capability inventory."))
        XCTAssertFalse(didRefresh)
    }

    @MainActor
    func testRevocationReturnsFalseWhenConnectionChangesDuringInventoryRefresh() async throws {
        let deployment = try makeRevocationDeployment(observed: "stopped")
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setCapabilityDeploymentsForTesting([deployment])
        model.capabilityRevocationSubmitOverrideForTesting = { version, _ in
            XCTAssertEqual(version, 8)
            return true
        }
        let refreshGate = AsyncTestGate()
        model.capabilityRevocationRefreshOverrideForTesting = { await refreshGate.pause() }

        let epoch = model.connectionEpoch
        let task = Task { await model.revokeCapabilityDeployment(deployment, expectedConnectionEpoch: epoch) }
        await refreshGate.waitUntilStarted()
        model.serverAddress = ""
        await model.clearCredential()
        await refreshGate.resume()

        let result = await task.value
        XCTAssertFalse(result)
        XCTAssertNotEqual(model.notice, uiText("Authorization revoked. Stop cleanup status is shown in the capability inventory."))
    }

    @MainActor
    func testRevocationVersionConflictRefreshesInventoryAndDoesNotReportSuccess() async throws {
        let deployment = try makeRevocationDeployment(observed: "running")
        let refreshedDeployment = try makeRevocationDeployment(observed: "running", version: 9)
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.setCapabilityDeploymentsForTesting([deployment])
        model.capabilityRevocationSubmitOverrideForTesting = { version, payload in
            XCTAssertEqual(version, 8)
            XCTAssertEqual(payload, .object(["deployment_id": .string("deploy-1")]))
            return false
        }
        model.capabilityRevocationRefreshOverrideForTesting = {
            model.setCapabilityDeploymentsForTesting([refreshedDeployment])
        }

        let result = await model.revokeCapabilityDeployment(deployment, expectedConnectionEpoch: model.connectionEpoch)

        XCTAssertFalse(result)
        XCTAssertEqual(model.capabilityDeployments.first?.version, 9)
        XCTAssertEqual(model.notice, uiText("The deployment version changed. The latest capability inventory has been loaded; review it before trying again."))
    }

    @MainActor
    private func makeRevocationDeployment(observed: String, version: Int64 = 8) throws -> CapabilityDeployment {
        let data = Data(#"{"items":[{"deployment_id":"deploy-1","workspace_id":"workspace-1","service_id":"service-1","release_digest":"sha256:release","requested_operation":"start","desired_state":"stopped","observed_state":"\#(observed)","grant_id":"grant-1","config_ref":"","config_sha256":"","version":\#(version),"created_by":"owner-1","created_at":"2026-10-21T12:00:00Z","updated_at":"2026-10-21T12:01:00Z","latest_attempt_id":"attempt-1","latest_fence":4,"last_error_code":"","revoked_at":null,"revoked_by":null,"revocation_cleanup_state":null}]}"#.utf8)
        return try XCTUnwrap(ZoeJSON.decoder.decode(CapabilityDeploymentList.self, from: data).items.first)
    }

    func testCapabilityRequestBindsExistingServiceToCurrentDeploymentVersion() throws {
        let data = Data(#"{"items":[{"deployment_id":"deploy-1","workspace_id":"workspace-1","service_id":"service-1","release_digest":"sha256:release","requested_operation":"start","desired_state":"running","observed_state":"pending","grant_id":"grant-1","config_ref":"","config_sha256":"","version":7,"created_by":"device-1","created_at":"2026-10-21T12:00:00Z","updated_at":"2026-10-21T12:01:00Z","latest_attempt_id":"attempt-1","latest_fence":3,"last_error_code":""}]}"#.utf8)
        let deployments = try ZoeJSON.decoder.decode(CapabilityDeploymentList.self, from: data).items

        XCTAssertEqual(deployments.version(forServiceID: "service-1"), 7)
        XCTAssertNil(deployments.version(forServiceID: "new-service"))
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
        guard case let .snapshotRequired(reason, authority)? = try parser.consume(line: "") else {
            return XCTFail("Expected snapshot repair request")
        }
        XCTAssertEqual(reason, "cursor_expired")
        XCTAssertEqual(authority, SnapshotAuthority(instanceLineageId: "lineage-2", generation: 6, deletionSequence: "9"))
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
        let data = Data(#"{"command_id":"cmd-1","idempotency_key":"key-1","workspace_id":"ws-1","kind":"task.append","status":"accepted","accepted_at":"2026-10-20T12:00:00Z","event_cursor":"21"}"#.utf8)
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
        let first = PendingCommand(idempotencyKey: "stable-key", kind: "task.append", expectedVersion: 2, payload: .object(["content": .string("first")]), createdAt: "now", workspaceID: "ws", expectedResourceType: "task", expectedResourceID: "t1")
        let changed = PendingCommand(idempotencyKey: "stable-key", kind: "task.append", expectedVersion: 2, payload: .object(["content": .string("second")]), createdAt: "later", workspaceID: "ws", expectedResourceType: "task", expectedResourceID: "t1")
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
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let coordinator = CommandCoordinator(api: api, journal: journal)
        let key = "stable-retry-\(UUID().uuidString.lowercased())"
        var postCount = 0
        var receiptCount = 0
        var acceptedReceipt = Data()
        ExportURLProtocol.failureHandler = { request in
            guard request.url?.path == "/v1/commands", request.httpMethod == "POST" else { return nil }
            postCount += 1
            let submittedKey = request.value(forHTTPHeaderField: "Idempotency-Key") ?? ""
            acceptedReceipt = Self.commandReceipt(key: submittedKey, kind: "task.create", workspaceID: "workspace-1", resourceID: "task-created")
            return URLError(.timedOut)
        }
        ExportURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/commands", request.httpMethod == "GET" {
                receiptCount += 1
                return (200, ["Content-Type": "application/json"], acceptedReceipt)
            }
            return (503, ["Content-Type": "application/json"], Data())
        }

        do {
            _ = try await coordinator.submit(kind: "task.create", payload: .object(["title": .string("Work")]), workspaceID: "workspace-1", idempotencyKey: key)
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
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "task.create", expectedVersion: nil, payload: payload, createdAt: "2026-10-22T12:00:00Z", workspaceID: "workspace-1", expectedResourceType: "task", expectedResourceID: nil))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let coordinator = CommandCoordinator(api: api, journal: journal)
        var receiptCount = 0
        var postCount = 0
        var postKey: String?
        var postBody: Data?
        ExportURLProtocol.responseHandler = { request in
            if request.url?.path == "/v1/commands", request.httpMethod == "GET" {
                receiptCount += 1
                return (404, ["Content-Type": "application/json"], Data())
            }
            if request.url?.path == "/v1/commands", request.httpMethod == "POST" {
                postCount += 1
                postKey = request.value(forHTTPHeaderField: "Idempotency-Key")
                postBody = Self.requestBody(request)
                return (200, ["Content-Type": "application/json"], Self.commandReceipt(key: key, kind: "task.create", workspaceID: "workspace-1", resourceID: "task-created"))
            }
            return (503, ["Content-Type": "application/json"], Data())
        }

        let outcome = try await coordinator.retryPending(idempotencyKey: key, currentWorkspaceID: "workspace-1")

        XCTAssertEqual(receiptCount, 1, "receipt lookup must precede the safe retry")
        XCTAssertEqual(postCount, 1)
        XCTAssertEqual(postKey, key)
        let postedEnvelope = try XCTUnwrap(postBody.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] })
        XCTAssertEqual(postedEnvelope["kind"] as? String, "task.create")
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
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "task.create", expectedVersion: nil, payload: .object([:]), createdAt: "now", workspaceID: "old-workspace", expectedResourceType: "task", expectedResourceID: nil))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let coordinator = CommandCoordinator(api: api, journal: journal)
        ExportURLProtocol.responseHandler = { _ in XCTFail("workspace mismatch must not make a network request"); return (500, [:], Data()) }

        do {
            _ = try await coordinator.retryPending(idempotencyKey: key, currentWorkspaceID: "new-workspace")
            XCTFail("Expected a workspace guard")
        } catch let error as ZoeError {
            guard case .pendingCommandWorkspaceMismatch = error else { return XCTFail("Unexpected error: \(error)") }
        }
        let remaining = try await journal.all()
        XCTAssertEqual(remaining.map(\.idempotencyKey), [key])
    }

    @MainActor
    func testUnknownWorkspaceNeverPostsLegacyPendingCommand() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let key = "legacy-unbound-\(UUID().uuidString.lowercased())"
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "task.create", expectedVersion: nil, payload: .object(["title": .string("Unbound")]), createdAt: "now", workspaceID: nil, expectedResourceType: "task", expectedResourceID: nil))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        var requestCount = 0
        ExportURLProtocol.responseHandler = { _ in requestCount += 1; return (404, [:], Data()) }
        let coordinator = CommandCoordinator(api: api, journal: journal)
        do {
            _ = try await coordinator.retryPending(idempotencyKey: key, currentWorkspaceID: nil)
            XCTFail("An unknown workspace cannot authorize a pending command")
        } catch let error as ZoeError {
            guard case .pendingCommandWorkspaceMismatch = error else { return XCTFail("Unexpected error: \(error)") }
        }

        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        await model.recoverPendingCommandsForTesting(api: api, journal: journal, workspaceID: nil)

        XCTAssertEqual(requestCount, 0)
        let remaining = try await journal.all()
        XCTAssertEqual(remaining.map(\.idempotencyKey), [key])
        XCTAssertEqual(model.pendingCommandSummaries.first?.state, .workspaceUnavailable)
    }

    func testPendingReceiptLookupNetworkFailureDoesNotResubmitOrDropJournalRow() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let key = "receipt-timeout-\(UUID().uuidString.lowercased())"
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "task.create", expectedVersion: nil, payload: .object(["title": .string("Work")]), createdAt: "now", workspaceID: "workspace-1", expectedResourceType: "task", expectedResourceID: nil))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let coordinator = CommandCoordinator(api: api, journal: journal)
        var postCount = 0
        ExportURLProtocol.failureHandler = { request in
            if request.url?.path == "/v1/commands", request.httpMethod == "GET" { return URLError(.timedOut) }
            if request.url?.path == "/v1/commands", request.httpMethod == "POST" { postCount += 1 }
            return nil
        }
        ExportURLProtocol.responseHandler = { _ in XCTFail("a failed receipt lookup must not fall through to POST"); return (500, [:], Data()) }

        do {
            _ = try await coordinator.retryPending(idempotencyKey: key, currentWorkspaceID: "workspace-1")
            XCTFail("Expected receipt lookup to remain uncertain")
        } catch { }

        XCTAssertEqual(postCount, 0)
        let remaining = try await journal.all()
        XCTAssertEqual(remaining.map(\.idempotencyKey), [key])
    }

    @MainActor
    func testVersionConflictMarksPendingActionForReviewAndRefreshesWithoutRewritingIt() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let key = "version-conflict-\(UUID().uuidString.lowercased())"
        let payload: JSONValue = .object(["task_id": .string("task-1"), "content": .string("original content")])
        try await journal.append(PendingCommand(idempotencyKey: key, kind: "task.append", expectedVersion: 4, payload: payload, createdAt: "now", workspaceID: "workspace-1", expectedResourceType: "task", expectedResourceID: "task-1"))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        var postedVersion: Int64?
        var postedEnvelope: [String: Any]?
        var snapshotRefreshes = 0
        ExportURLProtocol.responseHandler = { request in
            if request.httpMethod == "GET" {
                return (404, ["Content-Type": "application/json"], Data())
            }
            if request.httpMethod == "POST" {
                if let body = Self.requestBody(request), let envelope = try? JSONSerialization.jsonObject(with: body) as? [String: Any] {
                    postedEnvelope = envelope
                    postedVersion = (envelope["expected_version"] as? NSNumber)?.int64Value
                }
                return (409, ["Content-Type": "application/json"], Data(#"{"code":"version_conflict","message":"version changed","details":{"current_version":5}}"#.utf8))
            }
            return (500, [:], Data())
        }
        let oldSnapshot = Snapshot(serverVersion: "1", eventCursor: "1", projects: [], tasks: [], approvals: [], runs: [], workspaceId: "workspace-1")
        let freshSnapshot = Snapshot(serverVersion: "2", eventCursor: "2", projects: [], tasks: [], approvals: [], runs: [], workspaceId: oldSnapshot.workspaceId)
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        await model.preparePendingCheckForTesting(api: api, journal: journal, snapshot: oldSnapshot)
        model.snapshotFetchOverrideForTesting = { snapshotRefreshes += 1; return freshSnapshot }
        let summary = try XCTUnwrap(model.pendingCommandSummaries.first)

        await model.checkPendingCommand(summary)

        XCTAssertEqual(postedVersion, 4)
        XCTAssertEqual(postedEnvelope?["payload"] as? [String: String], ["task_id": "task-1", "content": "original content"])
        XCTAssertEqual(snapshotRefreshes, 1)
        XCTAssertEqual(model.snapshot?.serverVersion, "2")
        XCTAssertEqual(model.pendingCommandSummaries.first?.state, .needsReview)
        let remaining = try await journal.all()
        XCTAssertTrue(remaining.isEmpty, "a conflict is final for the original immutable command")
    }

    @MainActor
    func testDelayedOldJournalReadCannotReplaceSummariesAfterConnectionChanges() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let oldJournal = try CommandJournal(fileURL: directory.appendingPathComponent("old-commands.json"))
        let newJournal = try CommandJournal(fileURL: directory.appendingPathComponent("new-commands.json"))
        let oldCommand = PendingCommand(idempotencyKey: "old-connection", kind: "task.create", expectedVersion: nil, payload: .object(["title": .string("Old task")]), createdAt: "now", workspaceID: "workspace-1", expectedResourceType: "task", expectedResourceID: nil)
        try await oldJournal.append(oldCommand)
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        await model.preparePendingCheckForTesting(api: api, journal: oldJournal, snapshot: Snapshot(serverVersion: "1", eventCursor: "1", projects: [], tasks: [], approvals: [], runs: [], workspaceId: "workspace-1"))
        let readStarted = expectation(description: "old journal read started")
        var resumeRead: CheckedContinuation<[PendingCommand], Never>!
        model.pendingCommandReadOverrideForTesting = {
            await withCheckedContinuation { continuation in
                resumeRead = continuation
                readStarted.fulfill()
            }
        }

        let staleReload = Task { await model.reloadPendingCommandSummariesForTesting() }
        await fulfillment(of: [readStarted], timeout: 2)
        model.replacePendingCheckConnectionForTesting(api: api, journal: newJournal)
        resumeRead.resume(returning: [oldCommand])
        await staleReload.value

        XCTAssertTrue(model.pendingCommandSummaries.isEmpty)
    }

    @MainActor
    func testAppendForTaskMissingFromLatestSnapshotDoesNotCreateAnotherTask() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let task = TaskRecord(id: "task-removed", projectId: nil, title: "Existing task", goal: "Existing goal", source: "user", profileId: nil, status: .active, version: 2, createdAt: "now", updatedAt: "now", acceptedAt: nil, startedAt: nil, finishedAt: nil, result: nil, error: nil, evidence: [], canAcceptResult: false)
        let snapshot = Snapshot(serverVersion: "1", eventCursor: "1", projects: [], tasks: [task], approvals: [], runs: [], workspaceId: "workspace-1")
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        await model.preparePendingCheckForTesting(api: api, journal: try CommandJournal(fileURL: FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)), snapshot: snapshot)
        model.setSnapshotForTesting(Snapshot(serverVersion: "2", eventCursor: "2", projects: [], tasks: [], approvals: [], runs: [], workspaceId: "workspace-1"))
        var requestCount = 0
        ExportURLProtocol.responseHandler = { _ in requestCount += 1; return (500, [:], Data()) }

        await model.append(to: task, content: "Continue the existing task")

        XCTAssertEqual(requestCount, 0)
        XCTAssertEqual(model.notice, uiText("This task is unavailable in the latest snapshot. Refresh it or choose New task before sending."))
    }

    @MainActor
    func testRecoveredMemoryReceiptInvalidatesExportBeforeLaterJournalRowFails() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let exportURL = directory.appendingPathComponent("private-export.json")
        try Data("stale memory export".utf8).write(to: exportURL)
        model.setExportArtifactForTesting(ScopedExportArtifact(fileURL: exportURL, filename: "zoe-export-old.json", scope: "personal", snapshotCursor: "4"))
        var memoryRefreshCount = 0
        model.memorySearchOverrideForTesting = { _ in
            memoryRefreshCount += 1
            return MemorySearchResult(items: [], missing: [], conflicts: [])
        }
        let deleteKey = "accepted-delete-\(UUID().uuidString.lowercased())"
        let laterKey = "later-command-\(UUID().uuidString.lowercased())"
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        try await journal.append(PendingCommand(idempotencyKey: deleteKey, kind: "memory.delete", expectedVersion: 1, payload: .object(["memory_id": .string("memory-1")]), createdAt: "now", workspaceID: "workspace-1", expectedResourceType: "memory", expectedResourceID: "memory-1"))
        try await journal.append(PendingCommand(idempotencyKey: laterKey, kind: "task.create", expectedVersion: nil, payload: .object(["title": .string("Later")]), createdAt: "later", workspaceID: "workspace-1", expectedResourceType: "task", expectedResourceID: nil))
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        ExportURLProtocol.responseHandler = { request in
            guard request.url?.path == "/v1/commands", request.httpMethod == "GET" else { return (503, [:], Data()) }
            let key = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "key" })?.value
            if key == deleteKey {
                return (200, ["Content-Type": "application/json"], Self.commandReceipt(key: deleteKey, kind: "memory.delete", workspaceID: "workspace-1", resourceID: "memory-1", resourceType: "memory"))
            }
            return (503, ["Content-Type": "application/json"], Data())
        }

        await model.recoverPendingCommandsForTesting(api: api, journal: journal)

        let remaining = try await journal.all()
        XCTAssertEqual(remaining.map(\.idempotencyKey), [laterKey], "the accepted row is removed while the unresolved row remains durable")
        XCTAssertEqual(memoryRefreshCount, 1, "accepted memory receipts reconcile immediately before replay moves to the next row")
        XCTAssertNil(model.exportArtifact)
        XCTAssertFalse(FileManager.default.fileExists(atPath: exportURL.path))
    }

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
        let task = TaskRecord(id: "task-live", projectId: nil, title: "Keep this task", goal: "Task state survives voice media loss", source: "user", profileId: nil, status: .active, version: 3, createdAt: "now", updatedAt: "now", acceptedAt: nil, startedAt: nil, finishedAt: nil, result: nil, error: nil, evidence: [], canAcceptResult: false)
        model.setSnapshotForTesting(Snapshot(serverVersion: "1", eventCursor: "5", projects: [], tasks: [task], approvals: [], runs: [], workspaceId: "workspace-1"))
        var hangupCount = 0
        var localStopCount = 0
        ExportURLProtocol.responseHandler = { request in
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
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
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
        XCTAssertEqual(model.tasks.map(\.id), ["task-live"], "media loss does not cancel or rewrite the authoritative task")
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
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
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
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
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
    private func conversationTask(_ id: String, updatedAt: String = "2026-10-24T12:00:00Z", status: TaskStatus = .active) -> TaskRecord {
        TaskRecord(id: id, projectId: nil, title: id, goal: id, source: "user", profileId: nil, status: status, version: 2, createdAt: "2026-10-24T11:00:00Z", updatedAt: updatedAt, acceptedAt: nil, startedAt: nil, finishedAt: nil, result: nil, error: nil, evidence: [], canAcceptResult: false)
    }

    private func conversationSnapshot(_ tasks: [TaskRecord]) -> Snapshot {
        Snapshot(serverVersion: "1", eventCursor: "1", projects: [], tasks: tasks, approvals: [], runs: [], workspaceId: "workspace-1")
    }

    func testTaskEntriesAPIUsesPagedWireContractAndRejectsOversizedPage() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        var count = 0
        ExportURLProtocol.responseHandler = { request in
            count += 1
            XCTAssertEqual(request.url?.path, "/v1/tasks/task-a/entries")
            let query = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems
            XCTAssertEqual(query?.first(where: { $0.name == "cursor" })?.value, "opaque+cursor")
            XCTAssertEqual(query?.first(where: { $0.name == "limit" })?.value, "50")
            XCTAssertNotNil(request.value(forHTTPHeaderField: "Authorization"))
            return (200, [:], Data(#"{"messages":[{"id":"entry-1","task_id":"task-a","kind":"user","content":"Continue","created_at":"2026-10-24T12:00:00Z"}],"next_cursor":"next"}"#.utf8))
        }
        let page = try await api.taskEntries(taskID: "task-a", cursor: "opaque+cursor", limit: 50)
        XCTAssertEqual(page.messages.first?.taskId, "task-a")
        XCTAssertEqual(page.nextCursor, "next")
        do {
            _ = try await api.taskEntries(taskID: "task-a", limit: 101)
            XCTFail("oversized pages must fail before a request")
        } catch { XCTAssertEqual(count, 1) }
    }

    @MainActor
    func testTaskHistoryPreservesServerOrderAndDeduplicatesOverlappingPages() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        await model.preparePendingCheckForTesting(api: api, journal: try CommandJournal(fileURL: FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)), snapshot: conversationSnapshot([conversationTask("a")]))
        let first = TaskEntry(id: "first", taskId: "a", kind: "user", content: "First", createdAt: "2026-10-24T12:00:00Z")
        let second = TaskEntry(id: "second", taskId: "a", kind: "user", content: "Second", createdAt: "2026-10-24T12:00:00.1Z")
        model.taskEntriesFetchOverrideForTesting = { taskID, cursor in
            XCTAssertEqual(taskID, "a")
            if cursor == nil { return TaskEntriesPage(messages: [first], nextCursor: "next") }
            XCTAssertEqual(cursor, "next")
            return TaskEntriesPage(messages: [first, second], nextCursor: nil)
        }
        await model.selectTask("a")
        XCTAssertTrue(model.hasMoreTaskEntries)
        await model.loadMoreTaskEntries()
        XCTAssertEqual(model.taskEntries.map(\.id), ["first", "second"])
        XCTAssertFalse(model.hasMoreTaskEntries)
        model.taskEntriesFetchOverrideForTesting = { _, _ in throw URLError(.notConnectedToInternet) }
        await model.selectTask("a")
        XCTAssertTrue(model.taskEntries.isEmpty)
        XCTAssertNotNil(model.taskEntriesError)
    }

    @MainActor
    func testDelayedTaskHistoryCannotReplaceNewSelectionAndDraftsStaySeparate() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        await model.preparePendingCheckForTesting(api: api, journal: try CommandJournal(fileURL: FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)), snapshot: conversationSnapshot([conversationTask("a"), conversationTask("b")]))
        let gate = AsyncTestGate()
        model.taskEntriesFetchOverrideForTesting = { taskID, _ in
            if taskID == "a" { await gate.pause() }
            return TaskEntriesPage(messages: [TaskEntry(id: taskID, taskId: taskID, kind: "user", content: taskID, createdAt: "now")], nextCursor: nil)
        }
        model.updateTaskDraft("new draft")
        model.updateTaskDraft("draft a", for: "a")
        let oldRead = Task { await model.selectTask("a") }
        await gate.waitUntilStarted()
        await model.selectTask("b")
        XCTAssertEqual(model.selectedTaskDraft, "", "an empty existing task never inherits the new task draft")
        model.updateTaskDraft("draft b", for: "b")
        await gate.resume()
        await oldRead.value
        XCTAssertEqual(model.selectedTaskID, "b")
        XCTAssertEqual(model.taskEntries.map(\.id), ["b"])
        XCTAssertEqual(model.taskDrafts["a"], "draft a")
        XCTAssertEqual(model.selectedTaskDraft, "draft b")
        XCTAssertEqual(model.newTaskDraft, "new draft")
        XCTAssertFalse(model.isLoadingTaskEntries)
    }

    @MainActor
    func testUnknownTaskSendRetainsDraftAndReusesReceiptWithoutSecondPost() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        await model.preparePendingCheckForTesting(api: api, journal: journal, snapshot: conversationSnapshot([]))
        model.updateTaskDraft("Please investigate")
        var postCount = 0
        var savedKey = ""
        ExportURLProtocol.responseHandler = { request in
            if request.httpMethod == "POST" {
                postCount += 1
                savedKey = request.value(forHTTPHeaderField: "Idempotency-Key") ?? ""
                return (503, [:], Data())
            }
            XCTAssertEqual(request.httpMethod, "GET")
            let key = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "key" })?.value
            XCTAssertEqual(key, savedKey)
            return (200, [:], Self.commandReceipt(key: savedKey, kind: "task.create", workspaceID: "workspace-1", resourceID: "created"))
        }
        let firstResult = await model.sendTaskDraft(for: nil)
        guard case let .unknown(key, _) = firstResult else { return XCTFail("ambiguous HTTP failure must preserve a retryable intent") }
        XCTAssertEqual(key, savedKey)
        XCTAssertEqual(model.newTaskDraft, "Please investigate")
        let pending = try await journal.all()
        XCTAssertEqual(pending.map(\.idempotencyKey), [savedKey])
        let retryResult = await model.sendTaskDraft(for: nil)
        guard case .accepted = retryResult else { return XCTFail("retry must resolve the original receipt") }
        XCTAssertEqual(postCount, 1)
        XCTAssertEqual(model.newTaskDraft, "")
        let remaining = try await journal.all()
        XCTAssertTrue(remaining.isEmpty)
    }

    @MainActor
    func testRejectedTaskSendRetainsDraftAndAcceptedSendPreservesNewerEdit() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let snapshot = conversationSnapshot([])
        await model.preparePendingCheckForTesting(api: api, journal: journal, snapshot: snapshot)
        model.updateTaskDraft("Original")
        ExportURLProtocol.responseHandler = { _ in
            (409, [:], Data(#"{"error":{"code":"version_conflict","message":"Changed"}}"#.utf8))
        }
        let rejected = await model.sendTaskDraft(for: nil)
        guard case .failed = rejected else { return XCTFail("conflict must be a known failure") }
        XCTAssertEqual(model.newTaskDraft, "Original")
        let remaining = try await journal.all()
        XCTAssertTrue(remaining.isEmpty)
        let gate = AsyncTestGate()
        model.snapshotFetchOverrideForTesting = { await gate.pause(); return snapshot }
        ExportURLProtocol.responseHandler = { request in
            let key = request.value(forHTTPHeaderField: "Idempotency-Key") ?? ""
            return (200, [:], Self.commandReceipt(key: key, kind: "task.create", workspaceID: "workspace-1", resourceID: "created"))
        }
        let sending = Task { await model.sendTaskDraft(for: nil) }
        await gate.waitUntilStarted()
        model.updateTaskDraft("A newer edit")
        await gate.resume()
        let accepted = await sending.value
        guard case .accepted = accepted else { return XCTFail("receipt remains accepted") }
        XCTAssertEqual(model.newTaskDraft, "A newer edit")
    }

    @MainActor
    func testTaskOrderingParsesFractionalSecondsAndClearingCredentialClearsDraftContext() async throws {
        let defaults = UserDefaults(suiteName: UUID().uuidString)!
        defaults.set("https://zoe.example", forKey: "zoe.instance.serverAddress")
        let model = ZoeAppModel(defaults: defaults)
        model.setSnapshotForTesting(conversationSnapshot([conversationTask("older"), conversationTask("newer", updatedAt: "2026-10-24T05:00:00.1-07:00")]))
        XCTAssertEqual(model.tasks.map(\.id), ["newer", "older"])
        model.updateTaskDraft("private draft")
        model.updateTaskDraft("private task draft", for: "a")
        await model.clearCredential()
        XCTAssertEqual(model.newTaskDraft, "")
        XCTAssertTrue(model.taskDrafts.isEmpty)
        XCTAssertTrue(model.taskEntries.isEmpty)
        XCTAssertNil(model.selectedTaskID)
    }

    @MainActor
    func testExplicitTaskSendKeepsTargetWhenAnotherTaskIsSelected() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        await model.preparePendingCheckForTesting(api: api, journal: try CommandJournal(fileURL: directory.appendingPathComponent("commands.json")), snapshot: conversationSnapshot([conversationTask("a"), conversationTask("b")]))
        let gate = AsyncTestGate()
        model.taskEntriesFetchOverrideForTesting = { taskID, _ in
            if taskID == "a" { await gate.pause() }
            return TaskEntriesPage(messages: [], nextCursor: nil)
        }
        model.updateTaskDraft("Message for a", for: "a")
        model.updateTaskDraft("Message for b", for: "b")
        let selecting = Task { await model.selectTask("a") }
        await gate.waitUntilStarted()
        await model.selectTask("b")
        var postedTaskID: String?
        ExportURLProtocol.responseHandler = { request in
            let body = Self.requestBody(request) ?? Data()
            let command = try? JSONSerialization.jsonObject(with: body) as? [String: Any]
            postedTaskID = (command?["payload"] as? [String: Any])?["task_id"] as? String
            let key = request.value(forHTTPHeaderField: "Idempotency-Key") ?? ""
            return (200, [:], Self.commandReceipt(key: key, kind: "task.append", workspaceID: "workspace-1", resourceID: "a"))
        }
        let result = await model.sendTaskDraft(for: "a")
        guard case .accepted = result else { return XCTFail("explicit task send should be accepted") }
        await gate.resume()
        await selecting.value
        XCTAssertEqual(postedTaskID, "a")
        XCTAssertEqual(model.selectedTaskID, "b")
        XCTAssertEqual(model.taskDrafts["a"], "")
        XCTAssertEqual(model.taskDrafts["b"], "Message for b")
    }

    @MainActor
    func testUnknownReceiptRetryPreservesEditedDraftAndDoesNotPostIt() async throws {
        let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: StaticExportCredential(), session: exportSession())
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
        await model.preparePendingCheckForTesting(api: api, journal: journal, snapshot: conversationSnapshot([]))
        var postCount = 0
        var key = ""
        ExportURLProtocol.responseHandler = { request in
            if request.httpMethod == "POST" {
                postCount += 1
                key = request.value(forHTTPHeaderField: "Idempotency-Key") ?? ""
                return (503, [:], Data())
            }
            return (200, [:], Self.commandReceipt(key: key, kind: "task.create", workspaceID: "workspace-1", resourceID: "created"))
        }
        model.updateTaskDraft("Original task")
        let unknown = await model.sendTaskDraft(for: nil)
        guard case .unknown = unknown else { return XCTFail("send must remain unconfirmed") }
        model.updateTaskDraft("A different draft")
        let resolved = await model.retryTaskSubmission(idempotencyKey: key)
        guard case .accepted = resolved else { return XCTFail("retry must check original receipt") }
        XCTAssertEqual(postCount, 1)
        XCTAssertEqual(model.newTaskDraft, "A different draft")
    }

    @MainActor
    func testMemoryDeletionSequenceDoesNotEraseTaskDrafts() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        var before = conversationSnapshot([])
        before.authority = SnapshotAuthority(instanceLineageId: "lineage", generation: 1, deletionSequence: "0")
        model.setSnapshotForTesting(before)
        model.updateTaskDraft("Keep this draft")
        var after = before
        after.eventCursor = "2"
        after.authority = SnapshotAuthority(instanceLineageId: "lineage", generation: 1, deletionSequence: "1")
        model.snapshotFetchOverrideForTesting = { after }
        try await model.refreshSnapshotForTesting()
        XCTAssertEqual(model.newTaskDraft, "Keep this draft")
    }

}
