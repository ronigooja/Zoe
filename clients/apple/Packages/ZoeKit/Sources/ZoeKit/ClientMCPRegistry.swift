import Foundation

public struct ClientMCPEntry: Codable, Equatable, Sendable {
    public var id: String
    public var command: String
    public var arguments: [String]
    public var environmentReferences: [String: String]
    public var launchOptions: [String: Bool]
    public var optionArguments: [String: String]
    public var idleTimeoutSeconds: TimeInterval
    public var enabled: Bool = false

    private enum CodingKeys: String, CodingKey {
        case id, command, arguments, environmentReferences, launchOptions, optionArguments, idleTimeoutSeconds
    }

    public init(id: String, command: String, arguments: [String] = [], environmentReferences: [String: String] = [:], launchOptions: [String: Bool] = [:], optionArguments: [String: String] = [:], idleTimeoutSeconds: TimeInterval = 1800, enabled: Bool = false) {
        self.id = id
        self.command = command
        self.arguments = arguments
        self.environmentReferences = environmentReferences
        self.launchOptions = launchOptions
        self.optionArguments = optionArguments
        self.idleTimeoutSeconds = idleTimeoutSeconds
        self.enabled = enabled
    }

    public var effectiveArguments: [String] {
        arguments + optionArguments.keys.sorted().compactMap { key in
            launchOptions[key] == true ? optionArguments[key] : nil
        }
    }

    public static let agentDesktop = ClientMCPEntry(id: "agent-desktop", command: "agent-desktop", arguments: ["mcp"], launchOptions: ["headed": false], optionArguments: ["headed": "--headed"])
}

public protocol ClientMCPRegistryStore {
    func load() throws -> [ClientMCPEntry]?
    func save(_ entries: [ClientMCPEntry]) throws
}

public final class UserDefaultsMCPRegistryStore: ClientMCPRegistryStore {
    private let defaults: UserDefaults
    private let key: String

    public init(defaults: UserDefaults = .standard, key: String = "zoe.client-mcp.registry") {
        self.defaults = defaults
        self.key = key
    }

    public func load() throws -> [ClientMCPEntry]? {
        guard let data = defaults.data(forKey: key) else { return nil }
        return try JSONDecoder().decode([ClientMCPEntry].self, from: data)
    }

    public func save(_ entries: [ClientMCPEntry]) throws {
        defaults.set(try JSONEncoder().encode(entries), forKey: key)
    }
}

public final class InMemoryMCPRegistryStore: ClientMCPRegistryStore {
    private var entries: [ClientMCPEntry]?
    public init(entries: [ClientMCPEntry]? = nil) { self.entries = entries }
    public func load() throws -> [ClientMCPEntry]? { entries }
    public func save(_ entries: [ClientMCPEntry]) throws { self.entries = entries }
}

public struct ClientMCPRegistry {
    private let store: ClientMCPRegistryStore
    public private(set) var entries: [ClientMCPEntry]

    public init(store: ClientMCPRegistryStore, defaults: [ClientMCPEntry] = [.agentDesktop]) throws {
        self.store = store
        entries = (try store.load() ?? defaults).map(Self.off)
    }

    public mutating func replace(_ entries: [ClientMCPEntry]) throws {
        try store.save(entries.map(Self.off))
        self.entries = entries.map(Self.off)
    }

    private static func off(_ entry: ClientMCPEntry) -> ClientMCPEntry {
        var entry = entry
        entry.enabled = false
        return entry
    }
}

public protocol MCPExecutableResolving {
    func resolve(_ command: String) -> URL?
}

public struct PATHMCPExecutableResolver: MCPExecutableResolving {
    private let searchPath: [String]

    public init(path: String = ProcessInfo.processInfo.environment["PATH"] ?? "") {
        searchPath = path.split(separator: ":").map(String.init) + ["/opt/homebrew/bin", "/usr/local/bin"]
    }

    public func resolve(_ command: String) -> URL? {
        guard !command.isEmpty else { return nil }
        let candidates = command.contains("/") ? [command] : searchPath.map { ($0 as NSString).appendingPathComponent(command) }
        return candidates.first(where: { FileManager.default.isExecutableFile(atPath: $0) }).map { URL(fileURLWithPath: $0) }
    }
}
