import XCTest
import Foundation
@testable import ZoeKit

final class InstanceKeyStoreTests: XCTestCase {
    private var root: URL!
    private var directory: URL { root.appendingPathComponent("Zoe", isDirectory: true) }
    private var bundle: String!
    private let hardwareID = "test-hardware-uuid"
    private let token = String(repeating: "A", count: 43)

    override func setUpWithError() throws {
        root = FileManager.default.temporaryDirectory.appendingPathComponent("zoe-key-tests-\(UUID().uuidString)", isDirectory: true)
        bundle = "com.zoe.tests.\(UUID().uuidString)"
    }

    override func tearDownWithError() throws {
        if FileManager.default.fileExists(atPath: root.path) { try FileManager.default.removeItem(at: root) }
    }

    private func store(_ address: String = "https://zoe.example/base", hardwareID: String? = nil, bundle: String? = nil) throws -> InstanceKeyStore {
        let hardwareID = hardwareID ?? self.hardwareID
        return try InstanceKeyStore(serverURL: URL(string: address)!, bundleIdentifier: bundle ?? self.bundle, directory: directory, hardwareIDProvider: { hardwareID })
    }

    private func assertUnreadable(_ store: InstanceKeyStore, file: StaticString = #filePath, line: UInt = #line) {
        XCTAssertThrowsError(try store.load(), file: file, line: line) { error in
            guard case ZoeError.persistence(let message) = error else { return XCTFail("Expected persistence error", file: file, line: line) }
            XCTAssertEqual(message, "The saved instance key cannot be read.", file: file, line: line)
        }
    }

    func testRoundTripAndBearerToken() throws {
        let key = try InstanceKey(token)
        let saved = try store()
        try saved.replace(key)
        XCTAssertEqual(try saved.load(), key)
        XCTAssertEqual(try store().load(), key)
        XCTAssertEqual(try saved.bearerToken(), token)
    }

    func testScopeNormalizesEquivalentAddressesAndSeparatesPathSchemeAndBundle() throws {
        let canonical = try store()
        let equivalent = try store("HTTPS://ZOE.example:443/base/")
        XCTAssertEqual(canonical.service, equivalent.service)
        XCTAssertEqual(canonical.fileURL, equivalent.fileURL)
        try canonical.replace(InstanceKey(token))
        XCTAssertEqual(try equivalent.load(), try canonical.load())
        for different in [try store("https://zoe.example/other"), try store("http://zoe.example/base"), try store(bundle: bundle + ".other")] {
            XCTAssertNotEqual(canonical.service, different.service)
            XCTAssertNotEqual(canonical.fileURL, different.fileURL)
            XCTAssertNil(try different.load())
        }
        XCTAssertEqual(canonical.fileURL.deletingPathExtension().lastPathComponent.count, 64)
        XCTAssertTrue(canonical.fileURL.deletingPathExtension().lastPathComponent.allSatisfy { "0123456789abcdef".contains($0) })
    }

    func testEmptyLoadDoesNotCreateFileOrDirectory() throws {
        let saved = try store()
        XCTAssertNil(try saved.load())
        XCTAssertThrowsError(try saved.bearerToken())
        XCTAssertFalse(FileManager.default.fileExists(atPath: directory.path))
    }

    func testPrivatePermissionsAndEncryptedContent() throws {
        let saved = try store()
        try saved.replace(InstanceKey(token))
        let fileMode = try FileManager.default.attributesOfItem(atPath: saved.fileURL.path)[.posixPermissions] as? NSNumber
        let directoryMode = try FileManager.default.attributesOfItem(atPath: directory.path)[.posixPermissions] as? NSNumber
        XCTAssertEqual(fileMode?.intValue, 0o600)
        XCTAssertEqual(directoryMode?.intValue, 0o700)
        XCTAssertNil(try Data(contentsOf: saved.fileURL).range(of: Data(token.utf8)))
    }

    func testTamperingInvalidatesCachedRead() throws {
        let saved = try store()
        try saved.replace(InstanceKey(token))
        XCTAssertNotNil(try saved.load())
        var data = try Data(contentsOf: saved.fileURL)
        data[data.count - 1] ^= 1
        try data.write(to: saved.fileURL)
        assertUnreadable(saved)
    }

    func testWrongMagicAndTruncatedFileAreUnreadable() throws {
        let saved = try store()
        try saved.replace(InstanceKey(token))
        var data = try Data(contentsOf: saved.fileURL)
        data[0] ^= 1
        try data.write(to: saved.fileURL)
        assertUnreadable(saved)
        try Data(data.prefix(20)).write(to: saved.fileURL)
        assertUnreadable(saved)
    }

    func testDifferentHardwareIDCannotUseCachedKey() throws {
        let saved = try store()
        try saved.replace(InstanceKey(token))
        XCTAssertNotNil(try saved.load())
        assertUnreadable(try store(hardwareID: "different-hardware-uuid"))
    }

    func testUnavailableHardwareIDFailsWithoutWriting() throws {
        let saved = try store(hardwareID: "")
        XCTAssertThrowsError(try saved.replace(InstanceKey(token))) { error in
            guard case ZoeError.persistence = error else { return XCTFail("Expected persistence error") }
        }
        XCTAssertFalse(FileManager.default.fileExists(atPath: directory.path))
    }

    func testReplacementUsesFreshSaltAndNonce() throws {
        let saved = try store()
        let key = try InstanceKey(token)
        try saved.replace(key)
        let first = try Data(contentsOf: saved.fileURL)
        try saved.replace(key)
        let second = try Data(contentsOf: saved.fileURL)
        XCTAssertNotEqual(first.subdata(in: 4..<36), second.subdata(in: 4..<36))
        XCTAssertNotEqual(first.subdata(in: 36..<48), second.subdata(in: 36..<48))
        XCTAssertEqual(try saved.load(), key)
    }

    func testReplaceFailurePreservesOldValueAndFile() throws {
        let saved = try store()
        let key = try InstanceKey(token)
        try saved.replace(key)
        XCTAssertEqual(try saved.load(), key)
        let before = try Data(contentsOf: saved.fileURL)
        saved.writeFile = { _, _ in throw CocoaError(.fileWriteNoPermission) }
        XCTAssertThrowsError(try saved.replace(InstanceKey(String(repeating: "B", count: 42) + "A")))
        XCTAssertEqual(try Data(contentsOf: saved.fileURL), before)
        XCTAssertEqual(try saved.load(), key)
        XCTAssertEqual(try FileManager.default.contentsOfDirectory(atPath: directory.path), [saved.fileURL.lastPathComponent])
    }

    func testAtomicRenameFailureCleansTemporaryFile() throws {
        let saved = try store()
        try FileManager.default.createDirectory(at: saved.fileURL, withIntermediateDirectories: true)
        XCTAssertThrowsError(try saved.replace(InstanceKey(token)))
        XCTAssertEqual(try FileManager.default.contentsOfDirectory(atPath: directory.path), [saved.fileURL.lastPathComponent])
        var isDirectory: ObjCBool = false
        XCTAssertTrue(FileManager.default.fileExists(atPath: saved.fileURL.path, isDirectory: &isDirectory))
        XCTAssertTrue(isDirectory.boolValue)
    }

    func testResetRemovesFileAndCacheAndAllowsMissingFile() throws {
        let saved = try store()
        try saved.reset()
        try saved.replace(InstanceKey(token))
        XCTAssertNotNil(try saved.load())
        try saved.reset()
        XCTAssertFalse(FileManager.default.fileExists(atPath: saved.fileURL.path))
        XCTAssertNil(try saved.load())
        XCTAssertNil(try store().load())
        try saved.reset()
    }
}
