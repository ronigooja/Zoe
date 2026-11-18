import A2UISwiftCore
import CryptoKit
import Foundation
import ZoeKit

/// Identifies one server and workspace. Each scope gets its own folder, so history never mixes across servers.
public struct UISurfaceHistoryScope: Codable, Sendable, Equatable {
    public let serverURL: String
    public let workspaceID: String

    public init(serverURL: String, workspaceID: String) {
        self.serverURL = serverURL
        self.workspaceID = workspaceID
    }
}

/// One row of the index file. Holds only what the list needs; content stays in the record file.
public struct UISurfaceHistoryEntry: Codable, Sendable, Equatable, Hashable, Identifiable {
    public let id: String
    public let time: Date
    public let title: String
    public let size: Int
    public let fileName: String

    public init(id: String, time: Date, title: String, size: Int, fileName: String) {
        self.id = id
        self.time = time
        self.title = title
        self.size = size
        self.fileName = fileName
    }
}

/// Full content of one rendered presentation, read only when the entry is opened.
public struct UISurfaceHistoryRecord: Codable, Sendable {
    public let id: String
    public let time: Date
    public let container: SurfaceKind
    public let title: String
    public let messages: [A2uiMessage]

    public init(id: String, time: Date, container: SurfaceKind, title: String, messages: [A2uiMessage]) {
        self.id = id
        self.time = time
        self.container = container
        self.title = title
        self.messages = messages
    }
}

public enum UISurfaceHistoryError: Error, Equatable {
    case invalidID
    case notFound
}

/// Title is the first heading Text, else the first Text, normalized to one short line.
public enum UISurfaceHistoryTitle {
    public static let maxLength = 120
    private static let headingVariants: Set<String> = ["h1", "h2", "h3", "h4", "h5"]

    public static func make(from messages: [A2uiMessage]) -> String {
        var firstText: String?
        for message in messages {
            guard case let .updateComponents(payload) = message else { continue }
            for component in payload.components where component.component == "Text" {
                guard let text = normalized(component.properties["text"]?.stringValue) else { continue }
                if let variant = component.properties["variant"]?.stringValue, headingVariants.contains(variant) {
                    return text
                }
                if firstText == nil { firstText = text }
            }
        }
        return firstText ?? "Untitled"
    }

    static func normalized(_ value: String?) -> String? {
        guard let value else { return nil }
        let collapsed = value.split(whereSeparator: { $0.isWhitespace }).joined(separator: " ")
        guard !collapsed.isEmpty else { return nil }
        return String(collapsed.prefix(maxLength))
    }
}

/// Local, client-side presentation history. One JSON file per entry plus an index file per scope.
/// Writes are atomic. A missing or corrupt index is rebuilt from the record files.
/// Nothing here is ever removed by disconnecting or clearing the saved key.
public actor UISurfaceHistoryStore {
    public static let pageSize = 50
    private static let indexName = "index.json"

    public struct Page: Sendable, Equatable {
        public let entries: [UISurfaceHistoryEntry]
        public let hasMore: Bool

        public init(entries: [UISurfaceHistoryEntry], hasMore: Bool) {
            self.entries = entries
            self.hasMore = hasMore
        }
    }

    private let root: URL

    public init(root: URL) {
        self.root = root
    }

    public static func defaultRoot() -> URL {
        let base = (try? FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: false))
            ?? FileManager.default.temporaryDirectory
        return base.appendingPathComponent("Zoe", isDirectory: true).appendingPathComponent("UISurfaceHistory", isDirectory: true)
    }

    /// Appending an existing id is a no-op, so replayed recovery events never duplicate an entry.
    public func append(scope: UISurfaceHistoryScope, id: String, container: SurfaceKind, messages: [A2uiMessage], time: Date = Date()) throws {
        guard Self.isSafeID(id) else { throw UISurfaceHistoryError.invalidID }
        var entries = try loadIndex(scope)
        guard !entries.contains(where: { $0.id == id }) else { return }
        // Index and record files use whole seconds, so in-memory and reloaded entries compare equal.
        let stamp = Date(timeIntervalSince1970: floor(time.timeIntervalSince1970))
        let title = UISurfaceHistoryTitle.make(from: messages)
        let record = UISurfaceHistoryRecord(id: id, time: stamp, container: container, title: title, messages: messages)
        let data = try Self.encoder().encode(record)
        let directory = folder(for: scope)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let fileName = id + ".json"
        try data.write(to: directory.appendingPathComponent(fileName), options: .atomic)
        entries.append(UISurfaceHistoryEntry(id: id, time: stamp, title: title, size: data.count, fileName: fileName))
        try writeIndex(Self.sorted(entries), scope)
    }

    /// Reads only the index. Entry content is never touched while listing.
    public func page(scope: UISurfaceHistoryScope, offset: Int) throws -> Page {
        let entries = try loadIndex(scope)
        let start = min(max(offset, 0), entries.count)
        let end = min(start + Self.pageSize, entries.count)
        return Page(entries: Array(entries[start..<end]), hasMore: end < entries.count)
    }

    public func record(scope: UISurfaceHistoryScope, id: String) throws -> UISurfaceHistoryRecord {
        guard let entry = try loadIndex(scope).first(where: { $0.id == id }) else { throw UISurfaceHistoryError.notFound }
        let data = try Data(contentsOf: folder(for: scope).appendingPathComponent(entry.fileName))
        return try Self.decoder().decode(UISurfaceHistoryRecord.self, from: data)
    }

    public func delete(scope: UISurfaceHistoryScope, id: String) throws {
        var entries = try loadIndex(scope)
        guard let entry = entries.first(where: { $0.id == id }) else { return }
        entries.removeAll { $0.id == id }
        // Index first: a crash leaves an orphan file, never an index entry without a file.
        try writeIndex(entries, scope)
        try? FileManager.default.removeItem(at: folder(for: scope).appendingPathComponent(entry.fileName))
    }

    /// Removes only this scope's folder.
    public func clearAll(scope: UISurfaceHistoryScope) throws {
        let directory = folder(for: scope)
        guard FileManager.default.fileExists(atPath: directory.path) else { return }
        try FileManager.default.removeItem(at: directory)
    }

    private func loadIndex(_ scope: UISurfaceHistoryScope) throws -> [UISurfaceHistoryEntry] {
        let indexURL = folder(for: scope).appendingPathComponent(Self.indexName)
        if let data = try? Data(contentsOf: indexURL),
           let entries = try? Self.decoder().decode([UISurfaceHistoryEntry].self, from: data) {
            return Self.sorted(entries)
        }
        return try rebuildIndex(scope)
    }

    private func rebuildIndex(_ scope: UISurfaceHistoryScope) throws -> [UISurfaceHistoryEntry] {
        let directory = folder(for: scope)
        guard FileManager.default.fileExists(atPath: directory.path) else { return [] }
        var entries: [UISurfaceHistoryEntry] = []
        for name in try FileManager.default.contentsOfDirectory(atPath: directory.path) where name.hasSuffix(".json") && name != Self.indexName {
            guard let data = try? Data(contentsOf: directory.appendingPathComponent(name)),
                  let record = try? Self.decoder().decode(UISurfaceHistoryRecord.self, from: data),
                  Self.isSafeID(record.id) else { continue }
            entries.append(UISurfaceHistoryEntry(id: record.id, time: record.time, title: record.title, size: data.count, fileName: name))
        }
        let sorted = Self.sorted(entries)
        try? writeIndex(sorted, scope)
        return sorted
    }

    private func writeIndex(_ entries: [UISurfaceHistoryEntry], _ scope: UISurfaceHistoryScope) throws {
        let directory = folder(for: scope)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        try Self.encoder().encode(entries).write(to: directory.appendingPathComponent(Self.indexName), options: .atomic)
    }

    private func folder(for scope: UISurfaceHistoryScope) -> URL {
        let digest = SHA256.hash(data: Data("\(scope.serverURL)\n\(scope.workspaceID)".utf8))
        return root.appendingPathComponent(digest.map { String(format: "%02x", $0) }.joined(), isDirectory: true)
    }

    private static func sorted(_ entries: [UISurfaceHistoryEntry]) -> [UISurfaceHistoryEntry] {
        entries.sorted { lhs, rhs in lhs.time != rhs.time ? lhs.time > rhs.time : lhs.id > rhs.id }
    }

    private static func isSafeID(_ id: String) -> Bool {
        !id.isEmpty && id.count <= 128 && id.allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") }
    }

    private static func encoder() -> JSONEncoder {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        return encoder
    }

    private static func decoder() -> JSONDecoder {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return decoder
    }
}
