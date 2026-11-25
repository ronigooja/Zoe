import XCTest
import Foundation
@testable import ZoeKit
@testable import ZoeUI

private final class InstanceURLProtocol: URLProtocol {
    static var handle: ((URLRequest) throws -> (Int, Data))?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        // Background diagnostic uploads can outlive a test. A request that arrives
        // after tearDown has no handler and must fail as a transport error, not as
        // an XCTest assertion attributed to whichever test is running.
        guard let handler = Self.handle else {
            client?.urlProtocol(self, didFailWithError: URLError(.cancelled))
            return
        }
        do {
            let (status, data) = try handler(request)
            let response = try XCTUnwrap(HTTPURLResponse(url: XCTUnwrap(request.url), statusCode: status, httpVersion: nil, headerFields: nil))
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        } catch { client?.urlProtocol(self, didFailWithError: error) }
    }
    override func stopLoading() {}
}

private final class MemoryInstanceKeyStore: InstanceKeyStoring, @unchecked Sendable {
    var key: InstanceKey?
    var failWrites = false
    var failNextWriteReadback = false
    private var hideNextRead = false
    func load() throws -> InstanceKey? {
        if hideNextRead { hideNextRead = false; return nil }
        return key
    }
    func replace(_ key: InstanceKey) throws {
        if failWrites { throw ZoeError.persistence("Simulated local storage failure") }
        self.key = key
        if failNextWriteReadback {
            failNextWriteReadback = false
            hideNextRead = true
        }
    }
    func reset() throws { key = nil }
    func bearerToken() throws -> String {
        guard let key else { throw ZoeError.missingCredential }
        return try key.bearerToken()
    }
}

final class InstanceConnectionTests: XCTestCase {
    private func key(_ byte: UInt8 = 1) -> String {
        Data(repeating: byte, count: 32).base64EncodedString()
            .replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
    }
    private func link(server: String = "https://zoe.example/base", key: String) -> URL {
        var parts = URLComponents()
        parts.scheme = "zoe"
        parts.host = "connect"
        parts.queryItems = [URLQueryItem(name: "server", value: server)]
        parts.fragment = "key=\(key)"
        return parts.url!
    }
    private func session() -> URLSession {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [InstanceURLProtocol.self]
        return URLSession(configuration: configuration)
    }
    private var snapshotData: Data {
        Data(#"{"server_version":"candidate","event_cursor":"0","authority":{"instance_lineage_id":"default"},"subject_id":"owner","projects":[],"tasks":[],"approvals":[],"runs":[]}"#.utf8)
    }
    override func tearDown() {
        InstanceURLProtocol.handle = nil
        super.tearDown()
    }

    func testCanonicalKeyRejectsPaddingWhitespaceAndUnusedBits() throws {
        let canonical = key()
        XCTAssertNoThrow(try InstanceKey(canonical))
        for invalid in ["", String(canonical.dropLast()), canonical + "=", " " + canonical, canonical + "\n", String(repeating: "+", count: 43), String(canonical.dropLast()) + "B"] {
            XCTAssertThrowsError(try InstanceKey(invalid))
        }
    }

    func testServerNormalizationAndUnsafeAddresses() throws {
        XCTAssertEqual(try ServerAddress.normalize(" HTTPS://Zoe.Example:443/base/// ").absoluteString, "https://zoe.example/base")
        XCTAssertEqual(try ServerAddress.normalize("http://zoe.local:80/").absoluteString, "http://zoe.local")
        XCTAssertEqual(try ServerAddress.normalize("http://[::1]:8787/").port, 8787)
        for invalid in ["zoe.example", "file:///tmp/server", "https://user:secret@zoe.example", "https://zoe.example?key=hidden", "https://zoe.example#hidden", "https://zoe.example:0", "https://zoe.example:65536", "https://zoe.example/a/../b", "https://zoe.example/a%2fb", "https://zoe.example/a%5cb", "https://zoe.example/\\host"] {
            XCTAssertThrowsError(try ServerAddress.normalize(invalid), invalid)
        }
    }

    func testStrictInstallationLinkAndURLRoundTrip() throws {
        let imported = try InstallationConnection(link: link(server: "https://ZOE.example:443/private/", key: key()))
        XCTAssertEqual(imported.serverURL.absoluteString, "https://zoe.example/private")
        XCTAssertEqual(try imported.key.bearerToken(), key())
        let valid = link(key: key()).absoluteString
        let invalid = [
            valid.replacingOccurrences(of: "zoe://", with: "https://"),
            valid.replacingOccurrences(of: "connect?", with: "connect/other?"),
            valid.replacingOccurrences(of: "connect?", with: "other?"),
            valid + "&extra=value",
            valid.replacingOccurrences(of: "#key=", with: "&key="),
            valid.replacingOccurrences(of: "?server=", with: "?extra=1&server="),
            valid.replacingOccurrences(of: "#key=", with: "&server=https%3A%2F%2Fevil.example#key="),
            valid.replacingOccurrences(of: "#key=", with: "#other="),
            "zoe://user@connect?server=https%3A%2F%2Fzoe.example#key=\(key())",
            "zoe://connect?server=https%3A%2F%2Fzoe.example%23hidden#key=\(key())"
        ]
        for raw in invalid { XCTAssertThrowsError(try InstallationConnection(link: XCTUnwrap(URL(string: raw)))) }
    }

    func testCameraPayloadPolicyRejectsAmbiguityAndAcceptsRepeatedDetection() throws {
        let payload = link(key: key()).absoluteString
        let expected = try InstallationConnection(link: link(key: key()))
        XCTAssertEqual(try InstallationConnection.decodeQRPayloads([payload, payload]), expected)
        XCTAssertThrowsError(try InstallationConnection.decodeQRPayloads([]))
        XCTAssertThrowsError(try InstallationConnection.decodeQRPayloads([payload, link(key: key(2)).absoluteString]))
        XCTAssertThrowsError(try InstallationConnection.decodeQRPayloads([payload, "unrelated QR content"]))
        XCTAssertThrowsError(try InstallationConnection.decodeQRPayloads(["https://untrusted.example"]))
    }

    func testCLIQueryEncodedInstallationLinkIsAccepted() throws {
        // Go url.QueryEscape escapes every reserved character in the server query value.
        let raw = "zoe://connect?server=https%3A%2F%2Fzoe.example%3A8787%2Fprivate#key=\(key())"
        let imported = try InstallationConnection(link: XCTUnwrap(URL(string: raw)))
        XCTAssertEqual(imported.serverURL.absoluteString, "https://zoe.example:8787/private")
        XCTAssertEqual(try imported.key.bearerToken(), key())
    }

    @MainActor
    func testExplicitQRImportConnectsAndStoresOnlyAfterValidation() async throws {
        let defaults = UserDefaults(suiteName: UUID().uuidString)!
        let model = ZoeAppModel(defaults: defaults)
        let store = MemoryInstanceKeyStore()
        model.keyStoreOverrideForTesting = { _ in store }
        model.connectionAPIOverrideForTesting = { url, credential, installation in
            try APIClient(serverURL: url, credentials: credential, session: self.session(), installationID: installation)
        }
        model.suppressConnectionFollowupsForTesting = true
        let body = snapshotData
        var requests = 0
        InstanceURLProtocol.handle = { request in
            requests += 1
            XCTAssertEqual(request.url?.host, "zoe.example")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer \(self.key())")
            if requests == 1 { XCTAssertNil(try store.load()) }
            return (200, body)
        }
        let imported = try InstallationConnection.decodeQRPayloads([link(key: key()).absoluteString])
        let accepted = await model.connectImportedConnection(imported)
        XCTAssertTrue(accepted)
        XCTAssertGreaterThan(requests, 0)
        XCTAssertTrue(model.authentication.isConnected)
        XCTAssertEqual(try store.load(), imported.key)
        XCTAssertEqual(model.serverAddress, imported.serverURL.absoluteString)
        XCTAssertFalse(defaults.dictionaryRepresentation().values.contains { String(describing: $0).contains(key()) })
    }

    @MainActor
    func testExplicitQRImportCannotReplaceAnotherSavedServerOrUnvalidatedKey() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let store = MemoryInstanceKeyStore()
        try store.replace(InstanceKey(key()))
        model.keyStoreOverrideForTesting = { _ in store }
        model.serverAddress = "https://zoe.example"
        model.setAuthenticationForTesting(.connected)
        model.connectionAPIOverrideForTesting = { _, _, _ in
            XCTFail("Different-server QR imports must be rejected before network access")
            throw ZoeError.invalidServerURL
        }
        let different = try InstallationConnection(link: link(server: "https://another.example", key: key(2)))
        let switched = await model.connectImportedConnection(different)
        XCTAssertFalse(switched)
        model.connectionAPIOverrideForTesting = { url, credential, installation in
            try APIClient(serverURL: url, credentials: credential, session: self.session(), installationID: installation)
        }
        InstanceURLProtocol.handle = { _ in (401, Data()) }
        let rejected = await model.connectImportedConnection(try InstallationConnection(serverAddress: model.serverAddress, key: key(2)))
        XCTAssertFalse(rejected)
        XCTAssertEqual(model.serverAddress, "https://zoe.example")
        XCTAssertEqual(model.authentication, .connected)
        XCTAssertEqual(try store.load(), try InstanceKey(key()))
    }

    func testSnapshotWithoutDevicesUsesBearerAndIndependentInstallationHeader() async throws {
        let installation = UUID().uuidString.lowercased()
        let credential = try InstanceKey(key())
        let body = snapshotData
        InstanceURLProtocol.handle = { request in
            XCTAssertEqual(request.url?.path, "/base/v1/snapshot")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer \(try credential.bearerToken())")
            XCTAssertEqual(request.value(forHTTPHeaderField: "X-Zoe-Installation-ID"), installation)
            XCTAssertNil(request.url?.query)
            XCTAssertNil(request.url?.fragment)
            return (200, body)
        }
        let api = try APIClient(serverURL: URL(string: "https://zoe.example/base/")!, credentials: credential, session: session(), installationID: installation)
        let snapshot = try await api.snapshot()
        XCTAssertEqual(snapshot.authority.instanceLineageId, "default")
        XCTAssertEqual(snapshot.subjectId, "owner")
    }

    func testMissingOrDifferentOwnerContractCannotValidateConnection() async throws {
        for body in [snapshotData.replacingOccurrences(of: "default", with: ""), snapshotData.replacingOccurrences(of: "owner", with: "another"), Data(#"{"server_version":"old","event_cursor":"0","projects":[],"tasks":[],"approvals":[],"devices":[],"runs":[]}"#.utf8)] {
            InstanceURLProtocol.handle = { _ in (200, body) }
            let api = try APIClient(serverURL: URL(string: "https://zoe.example")!, credentials: InstanceKey(key()), session: session())
            do { _ = try await api.snapshot(); XCTFail("Incompatible authority must fail validation") }
            catch { XCTAssertTrue(error is ZoeError) }
        }
    }

    @MainActor
    func testTransportFailureUsesGenericUserMessage() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.connectionAPIOverrideForTesting = { url, credential, installation in
            try APIClient(serverURL: url, credentials: credential, session: self.session(), installationID: installation)
        }
        InstanceURLProtocol.handle = { _ in throw URLError(.notConnectedToInternet) }

        let connected = await model.connect(serverAddress: "https://zoe.example", key: key())

        XCTAssertFalse(connected)
        XCTAssertEqual(model.notice, "Something went wrong. Try again.")
    }

    @MainActor
    func testFreshInstallHasNoAddressAndImportNeedsExplicitConnect() async throws {
        let defaults = UserDefaults(suiteName: UUID().uuidString)!
        defaults.set("http://obsolete.example", forKey: "zoe.serverAddress")
        let model = ZoeAppModel(defaults: defaults)
        XCTAssertEqual(model.serverAddress, "")
        await model.start()
        XCTAssertEqual(model.authentication, .disconnected)
        model.receiveInstallationLink(link(key: key()))
        XCTAssertNotNil(model.pendingConnectionImport)
        XCTAssertEqual(model.serverAddress, "")
        XCTAssertNil(defaults.string(forKey: "zoe.instance.serverAddress"))
        XCTAssertFalse(defaults.dictionaryRepresentation().values.contains { String(describing: $0).contains(key()) })
        model.receiveInstallationLink(URL(string: "zoe://connect?server=invalid#key=invalid")!)
        XCTAssertNil(model.pendingConnectionImport)
        XCTAssertNotNil(model.connectionImportError)
    }

    @MainActor
    func testRejectedReplacementKeepsWorkingConnectionAndDraft() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let store = MemoryInstanceKeyStore()
        let oldKey = try InstanceKey(key())
        try store.replace(oldKey)
        model.keyStoreOverrideForTesting = { _ in store }
        model.serverAddress = "https://zoe.example"
        model.setAuthenticationForTesting(.connected)
        model.connectionAPIOverrideForTesting = { url, key, installation in
            try APIClient(serverURL: url, credentials: key, session: self.session(), installationID: installation)
        }
        InstanceURLProtocol.handle = { _ in (401, Data(#"{"error":{"code":"unauthorized","message":"Rejected"}}"#.utf8)) }
        let accepted = await model.connect(serverAddress: model.serverAddress, key: key(2))
        XCTAssertFalse(accepted)
        XCTAssertEqual(model.authentication, .connected)
        XCTAssertEqual(try store.load(), oldKey)
        XCTAssertFalse(model.isBusy)
        XCTAssertFalse(model.notice?.contains(key(2)) ?? true)
    }

    @MainActor
    func testLocalStorageReplacementFailurePreservesKeyDraftSnapshotAndQueuedCommands() async throws {
        for failReadback in [false, true] {
            let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
            defer { try? FileManager.default.removeItem(at: directory) }
            let journal = try CommandJournal(fileURL: directory.appendingPathComponent("commands.json"))
            let queued = PendingCommand(idempotencyKey: UUID().uuidString, kind: "voice.text_input", expectedVersion: nil,
                                        payload: .object(["title": .string("Queued work")]), createdAt: "now",
                                        instanceLineageID: "default", expectedResourceType: nil, expectedResourceID: nil)
            try await journal.append(queued)
            let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
            let store = MemoryInstanceKeyStore()
            let oldKey = try InstanceKey(key())
            try store.replace(oldKey)
            model.keyStoreOverrideForTesting = { _ in store }
            model.connectionAPIOverrideForTesting = { url, credential, installation in
                try APIClient(serverURL: url, credentials: credential, session: self.session(), installationID: installation)
            }
            let url = URL(string: "https://zoe.example")!
            let api = try APIClient(serverURL: url, credentials: oldKey, session: session())
            let decoder = JSONDecoder()
            decoder.keyDecodingStrategy = .convertFromSnakeCase
            let snapshot = try decoder.decode(Snapshot.self, from: snapshotData)
            model.configureVoiceCoordinatorForTesting(api: api, journal: journal)
            model.setSnapshotForTesting(snapshot)
            model.serverAddress = url.absoluteString
            model.setAuthenticationForTesting(.connected)
            let epoch = model.connectionEpoch
            store.failWrites = !failReadback
            store.failNextWriteReadback = failReadback
            let body = snapshotData
            InstanceURLProtocol.handle = { _ in (200, body) }
            let imported = try InstallationConnection(serverAddress: url.absoluteString, key: key(2))
            let accepted = await model.connectImportedConnection(imported)
            XCTAssertFalse(accepted)
            XCTAssertEqual(try store.load(), oldKey)
            XCTAssertEqual(model.serverAddress, url.absoluteString)
            XCTAssertEqual(model.authentication, .connected)
            XCTAssertEqual(model.connectionEpoch, epoch)
            let remaining = try await journal.all()
            XCTAssertEqual(remaining, [queued])
            XCTAssertFalse(model.isBusy)
        }
    }

    @MainActor
    func testConfirmedReplacementPersistsKeyAndClearsLocalQueueAndDrafts() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        let store = MemoryInstanceKeyStore()
        try store.replace(InstanceKey(key()))
        model.keyStoreOverrideForTesting = { _ in store }
        model.connectionAPIOverrideForTesting = { url, key, installation in
            try APIClient(serverURL: url, credentials: key, session: self.session(), installationID: installation)
        }
        model.suppressConnectionFollowupsForTesting = true
        model.serverAddress = "https://zoe.example"
        model.setAuthenticationForTesting(.connected)
        let body = snapshotData
        InstanceURLProtocol.handle = { _ in (200, body) }
        let accepted = await model.connect(serverAddress: model.serverAddress, key: key(2))
        XCTAssertTrue(accepted)
        XCTAssertEqual(model.authentication, .connected)
        XCTAssertEqual(try store.load(), try InstanceKey(key(2)))
        let before = model.connectionEpoch
        await model.clearCredential()
        XCTAssertNil(try store.load())
        XCTAssertEqual(model.serverAddress, "")
        XCTAssertEqual(model.authentication, .disconnected)
        XCTAssertGreaterThan(model.connectionEpoch, before)
    }

    @MainActor
    func testDifferentServerRequiresExplicitClearAndLocalStorageFailureStaysDisconnected() async throws {
        let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.serverAddress = "https://zoe.example"
        model.setAuthenticationForTesting(.connected)
        let switched = await model.connect(serverAddress: "https://another.example", key: key())
        XCTAssertFalse(switched)
        XCTAssertEqual(model.serverAddress, "https://zoe.example")
        XCTAssertEqual(model.authentication, .connected)
        let store = MemoryInstanceKeyStore()
        model.keyStoreOverrideForTesting = { _ in store }
        await model.clearCredential()
        store.failWrites = true
        model.connectionAPIOverrideForTesting = { url, key, installation in
            try APIClient(serverURL: url, credentials: key, session: self.session(), installationID: installation)
        }
        let body = snapshotData
        InstanceURLProtocol.handle = { _ in (200, body) }
        let accepted = await model.connect(serverAddress: "https://another.example", key: key())
        XCTAssertFalse(accepted)
        XCTAssertEqual(model.authentication, .disconnected)
        XCTAssertFalse(model.isBusy)
        XCTAssertNil(try store.load())
    }

    #if os(macOS)
    @MainActor
    func testRemoteControlFollowsValidatedConnectionAndCredentialLifecycle() async throws {
        let defaults = try XCTUnwrap(UserDefaults(suiteName: UUID().uuidString))
        let model = ZoeAppModel(defaults: defaults)
        let store = MemoryInstanceKeyStore()
        model.keyStoreOverrideForTesting = { _ in store }
        model.connectionAPIOverrideForTesting = { url, credential, installation in
            try APIClient(serverURL: url, credentials: credential, session: self.session(), installationID: installation)
        }
        model.suppressConnectionFollowupsForTesting = true
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        model.journalFactoryForTesting = { _ in try CommandJournal(fileURL: directory.appendingPathComponent("commands.json")) }
        let body = snapshotData
        InstanceURLProtocol.handle = { _ in (200, body) }
        var relays: [FakeRemoteControlRelay] = []
        model.remoteControlRelayFactoryForTesting = { api in
            XCTAssertTrue(api.hasCredentials)
            XCTAssertEqual(api.serverURL.absoluteString, model.serverAddress)
            let relay = FakeRemoteControlRelay()
            relays.append(relay)
            return relay
        }
        defer { model.remoteControlRelayFactoryForTesting = nil }

        XCTAssertNil(model.remoteControl)
        let connected = await model.connect(serverAddress: "https://zoe.example", key: key())
        XCTAssertTrue(connected)
        let initialRelay = try XCTUnwrap(relays.first)
        let initialControl = try XCTUnwrap(model.remoteControl)
        XCTAssertFalse(model.remoteControlActive)
        await initialControl.setEnabled(true, id: "agent-desktop")
        XCTAssertTrue(model.remoteControlActive)

        await model.reconnect()
        XCTAssertEqual(relays.count, 1)
        XCTAssertTrue(model.remoteControl === initialControl)
        XCTAssertTrue(model.remoteControlActive)

        let changed = await model.connect(serverAddress: model.serverAddress, key: key(2))
        XCTAssertTrue(changed)
        XCTAssertEqual(relays.count, 2)
        let rotatedRelay = try XCTUnwrap(relays.last)
        let rotatedControl = try XCTUnwrap(model.remoteControl)
        XCTAssertFalse(rotatedControl === initialControl)
        let initialShutdowns = await initialRelay.shutdowns
        let credentialChanges = await rotatedRelay.credentialChanges
        XCTAssertEqual(initialShutdowns, 1)
        XCTAssertEqual(credentialChanges, 1)
        XCTAssertFalse(model.remoteControlActive)

        await rotatedControl.setEnabled(true, id: "agent-desktop")
        await model.clearCredential()
        XCTAssertNil(model.remoteControl)
        XCTAssertFalse(model.remoteControlActive)
        let rotatedShutdowns = await rotatedRelay.shutdowns
        XCTAssertEqual(rotatedShutdowns, 1)
        await rotatedControl.setEnabled(true, id: "agent-desktop")
        let finalStatuses = await rotatedRelay.statuses()
        XCTAssertEqual(finalStatuses["agent-desktop"], .off)

        let switched = await model.connect(serverAddress: "https://other.example", key: key(3))
        XCTAssertTrue(switched)
        XCTAssertEqual(relays.count, 3)
        let differentURLRelay = try XCTUnwrap(relays.last)
        let differentURLCredentialChanges = await differentURLRelay.credentialChanges
        XCTAssertEqual(differentURLCredentialChanges, 1)
        try store.reset() // Removal outside Zoe must be detected on reconnect too.
        await model.reconnect()
        let missingCredentialShutdowns = await differentURLRelay.shutdowns
        XCTAssertEqual(missingCredentialShutdowns, 1)
        XCTAssertNil(model.remoteControl)
        XCTAssertFalse(model.remoteControlActive)

        let connectedBeforeExit = await model.connect(serverAddress: model.serverAddress, key: key(4))
        XCTAssertTrue(connectedBeforeExit)
        let exitingRelay = try XCTUnwrap(relays.last)
        let exitingControl = try XCTUnwrap(model.remoteControl)
        XCTAssertFalse(model.remoteControlNeedsShutdown)
        await exitingControl.setEnabled(true, id: "agent-desktop")
        XCTAssertTrue(model.remoteControlNeedsShutdown)
        await model.shutdownRemoteControlForAppExit()
        await model.shutdownRemoteControlForAppExit()
        let exitShutdowns = await exitingRelay.shutdowns
        XCTAssertEqual(exitShutdowns, 1)
        XCTAssertNil(model.remoteControl)
        XCTAssertFalse(model.remoteControlActive)
        XCTAssertFalse(model.remoteControlNeedsShutdown)
    }
    #endif

    @MainActor
    func testSavedServerRemainsLockedAfterKeyRejectionOrUnavailableConnection() async throws {
        for state in [InstanceConnectionState.disconnected, .unavailable] {
            let model = ZoeAppModel(defaults: UserDefaults(suiteName: UUID().uuidString)!)
            model.serverAddress = "https://zoe.example"
            model.setAuthenticationForTesting(state)
            let store = MemoryInstanceKeyStore()
            try store.replace(InstanceKey(key()))
            model.keyStoreOverrideForTesting = { _ in store }
            model.connectionAPIOverrideForTesting = { _, _, _ in
                XCTFail("A different server must be refused before any request")
                throw ZoeError.invalidServerURL
            }
            let accepted = await model.connect(serverAddress: "https://another.example", key: key(2))
            XCTAssertFalse(accepted)
            XCTAssertEqual(model.serverAddress, "https://zoe.example")
            XCTAssertEqual(model.authentication, state)
            XCTAssertEqual(try store.load(), try InstanceKey(key()))
            await model.clearCredential()
            XCTAssertEqual(model.serverAddress, "")
            XCTAssertNil(try store.load())
        }
    }
}

private extension Data {
    func replacingOccurrences(of target: String, with replacement: String) -> Data {
        Data(String(decoding: self, as: UTF8.self).replacingOccurrences(of: target, with: replacement).utf8)
    }
}
