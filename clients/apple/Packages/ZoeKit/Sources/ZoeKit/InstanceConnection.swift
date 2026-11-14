import Foundation
import CryptoKit
import Darwin
import IOKit

/// A canonical 256-bit instance credential. Never include this value in diagnostics.
public struct InstanceKey: Equatable, Sendable, BearerTokenProviding {
    private let value: String
    public init(_ value: String) throws {
        guard value.utf8.count == 43,
              value.utf8.allSatisfy({ (65...90).contains($0) || (97...122).contains($0) || (48...57).contains($0) || $0 == 45 || $0 == 95 }),
              let bytes = Data(base64Encoded: value.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/") + "="),
              bytes.count == 32,
              bytes.base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "") == value else {
            throw ConnectionInputError.invalidKey
        }
        self.value = value
    }
    public func bearerToken() throws -> String { value }
}

public enum ConnectionInputError: Error, LocalizedError {
    case invalidKey, invalidLink, missingQRCode, ambiguousQRCode
    public var errorDescription: String? {
        switch self {
        case .invalidKey: "Enter the 43-character instance key from your server."
        case .invalidLink: "This installation link is invalid."
        case .missingQRCode: "No installation QR code was found in this image."
        case .ambiguousQRCode: "Choose an image containing one installation QR code."
        }
    }
}

public enum ServerAddress {
    public static func normalize(_ address: String) throws -> URL {
        let text = address.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.contains("\\"),
              var parts = URLComponents(string: text),
              let scheme = parts.scheme?.lowercased(), ["https", "http"].contains(scheme),
              let host = parts.host, !host.isEmpty,
              parts.user == nil, parts.password == nil, parts.query == nil, parts.fragment == nil,
              !parts.percentEncodedPath.lowercased().contains("%2f"),
              !parts.percentEncodedPath.lowercased().contains("%5c"),
              !parts.path.split(separator: "/").contains(where: { $0 == "." || $0 == ".." }),
              parts.port.map({ (1...65535).contains($0) }) ?? true else { throw ZoeError.invalidServerURL }
        parts.scheme = scheme
        parts.host = host.lowercased()
        if (scheme == "https" && parts.port == 443) || (scheme == "http" && parts.port == 80) { parts.port = nil }
        while parts.path.hasSuffix("/") { parts.path.removeLast() }
        guard let url = parts.url else { throw ZoeError.invalidServerURL }
        return url
    }
}

/// Validated installation details. The caller decides whether the user authorized connection.
public struct InstallationConnection: Equatable, Sendable {
    public let serverURL: URL
    public let key: InstanceKey
    public init(serverAddress: String, key: String) throws {
        serverURL = try ServerAddress.normalize(serverAddress)
        self.key = try InstanceKey(key)
    }
    public init(link: URL) throws {
        guard let parts = URLComponents(url: link, resolvingAgainstBaseURL: false),
              parts.scheme?.lowercased() == "zoe", parts.host == "connect",
              parts.path.isEmpty, parts.user == nil, parts.password == nil, parts.port == nil,
              let query = parts.queryItems, query.count == 1,
              query[0].name == "server", let server = query[0].value,
              let fragment = parts.fragment, fragment.hasPrefix("key="),
              !fragment.contains("&"), !fragment.contains("%") else { throw ConnectionInputError.invalidLink }
        try self.init(serverAddress: server, key: String(fragment.dropFirst(4)))
    }

    /// Shared camera/image policy: never select one credential from multiple QR payloads.
    public static func decodeQRPayloads(_ payloads: [String]) throws -> InstallationConnection {
        let unique = Set(payloads)
        guard !unique.isEmpty else { throw ConnectionInputError.missingQRCode }
        guard unique.count == 1, let payload = unique.first else { throw ConnectionInputError.ambiguousQRCode }
        guard let link = URL(string: payload) else { throw ConnectionInputError.invalidLink }
        return try InstallationConnection(link: link)
    }
}

/// One encrypted local credential per normalized server address.
public protocol InstanceKeyStoring: BearerTokenProviding {
    func load() throws -> InstanceKey?
    /// Atomically replace the credential; a thrown write must preserve the previous value.
    func replace(_ key: InstanceKey) throws
    func reset() throws
}

public final class InstanceKeyStore: InstanceKeyStoring, @unchecked Sendable {
    /// The bundle-and-server scope used for the filename and encryption context.
    let service: String
    let fileURL: URL
    private let directory: URL
    private let hardwareIDProvider: () throws -> String
    private static let magic = Data([0x5a, 0x4f, 0x45, 0x01])
    var writeFile: (Data, URL) throws -> Void = InstanceKeyStore.atomicWrite

    public init(serverURL: URL, bundleIdentifier: String = Bundle.main.bundleIdentifier ?? "com.zoe.client", directory: URL? = nil, hardwareIDProvider: (() throws -> String)? = nil) throws {
        let serverURL = try ServerAddress.normalize(serverURL.absoluteString)
        service = "\(bundleIdentifier).instance-key.\(serverURL.absoluteString)"
        self.directory = try directory ?? FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: false).appendingPathComponent("Zoe", isDirectory: true)
        let filename = SHA256.hash(data: Data(service.utf8)).map { String(format: "%02x", $0) }.joined() + ".key"
        fileURL = self.directory.appendingPathComponent(filename)
        self.hardwareIDProvider = hardwareIDProvider ?? Self.hardwareUUID
    }
    /// Process-wide decrypted-value cache, scoped by server and validated against file and hardware.
    private static let cache = KeyCache()
    private struct CachedKey {
        let fingerprint: Data
        let key: InstanceKey
    }
    private final class KeyCache: @unchecked Sendable {
        private let lock = NSLock()
        private var values: [String: CachedKey] = [:]
        func get(_ service: String) -> CachedKey? { lock.lock(); defer { lock.unlock() }; return values[service] }
        func set(_ service: String, _ key: CachedKey?) { lock.lock(); defer { lock.unlock() }; values[service] = key }
    }

    public func load() throws -> InstanceKey? {
        let data: Data
        do { data = try Data(contentsOf: fileURL) }
        catch CocoaError.fileReadNoSuchFile { Self.cache.set(service, nil); return nil }
        catch { throw ZoeError.persistence("The saved instance key cannot be read.") }
        let hardwareID = try hardwareIDProvider()
        guard !hardwareID.isEmpty else { throw ZoeError.persistence("The hardware identifier cannot be read.") }
        do {
            guard data.count >= Self.magic.count + 32 + 28, data.starts(with: Self.magic) else { throw CocoaError(.fileReadCorruptFile) }
            let salt = data.subdata(in: Self.magic.count..<(Self.magic.count + 32))
            let key = encryptionKey(hardwareID: hardwareID, salt: salt)
            let fingerprint = Data(SHA256.hash(data: data + key.withUnsafeBytes { Data($0) }))
            if let cached = Self.cache.get(service), cached.fingerprint == fingerprint { return cached.key }
            let box = try AES.GCM.SealedBox(combined: data.dropFirst(Self.magic.count + 32))
            let plaintext = try AES.GCM.open(box, using: key)
            guard let text = String(data: plaintext, encoding: .utf8) else { throw CocoaError(.fileReadCorruptFile) }
            let loaded = try InstanceKey(text)
            Self.cache.set(service, CachedKey(fingerprint: fingerprint, key: loaded))
            return loaded
        } catch { throw ZoeError.persistence("The saved instance key cannot be read.") }
    }
    public func replace(_ key: InstanceKey) throws {
        let hardwareID = try hardwareIDProvider()
        guard !hardwareID.isEmpty else { throw ZoeError.persistence("The hardware identifier cannot be read.") }
        do {
            let salt = SymmetricKey(size: .bits256).withUnsafeBytes { Data($0) }
            let sealed = try AES.GCM.seal(Data(try key.bearerToken().utf8), using: encryptionKey(hardwareID: hardwareID, salt: salt))
            guard let combined = sealed.combined else { throw CocoaError(.fileWriteUnknown) }
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
            try writeFile(Self.magic + salt + combined, fileURL)
            Self.cache.set(service, nil)
        } catch { throw ZoeError.persistence("The instance key could not be saved in local storage.") }
    }
    public func reset() throws {
        do { try FileManager.default.removeItem(at: fileURL) }
        catch CocoaError.fileNoSuchFile {}
        catch { throw ZoeError.persistence("The saved instance key could not be removed.") }
        Self.cache.set(service, nil)
    }
    public func bearerToken() throws -> String {
        guard let key = try load() else { throw ZoeError.missingCredential }
        return try key.bearerToken()
    }
    private func encryptionKey(hardwareID: String, salt: Data) -> SymmetricKey {
        HKDF<SHA256>.deriveKey(inputKeyMaterial: SymmetricKey(data: Data(hardwareID.utf8)), salt: salt, info: Data("Zoe.instance-key.v1.\(service)".utf8), outputByteCount: 32)
    }

    private static func atomicWrite(_ data: Data, to destination: URL) throws {
        let temporary = destination.deletingLastPathComponent().appendingPathComponent(".\(UUID().uuidString).tmp")
        let descriptor = open(temporary.path, O_WRONLY | O_CREAT | O_EXCL, 0o600)
        guard descriptor >= 0 else { throw CocoaError(.fileWriteUnknown) }
        close(descriptor)
        defer { try? FileManager.default.removeItem(at: temporary) }
        try data.write(to: temporary)
        guard rename(temporary.path, destination.path) == 0 else { throw CocoaError(.fileWriteUnknown) }
    }

    private static func hardwareUUID() throws -> String {
        let service = IOServiceGetMatchingService(kIOMainPortDefault, IOServiceMatching("IOPlatformExpertDevice"))
        guard service != 0 else { throw ZoeError.persistence("The hardware identifier cannot be read.") }
        defer { IOObjectRelease(service) }
        guard let value = IORegistryEntryCreateCFProperty(service, "IOPlatformUUID" as CFString, kCFAllocatorDefault, 0)?.takeRetainedValue() as? String, !value.isEmpty else {
            throw ZoeError.persistence("The hardware identifier cannot be read.")
        }
        return value
    }
}
