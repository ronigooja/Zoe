#if os(macOS)
import Combine
import Foundation
import ZoeKit

/// The part of the relay used by local settings and the connection lifecycle.
protocol RemoteControlRelaying: Sendable {
    func entries() async -> [ClientMCPEntry]
    func statuses() async -> [String: ClientMCPStatus]
    func statusUpdates() async -> AsyncStream<[String: ClientMCPStatus]>
    func setEnabled(_ enabled: Bool, id: String) async throws
    func setIdleTimeout(_ seconds: TimeInterval, id: String) async throws
    func setLaunchOption(_ key: String, value: Bool, id: String) async throws
    func credentialsChanged() async
    func shutdown() async
}

extension ClientMCPRelay: RemoteControlRelaying {}

@MainActor
public final class RemoteControlModel: ObservableObject {
    public struct Row: Identifiable, Equatable {
        public let id: String
        public let title: String
        public let enabled: Bool
        public let status: ClientMCPStatus
        public let idleTimeoutSeconds: TimeInterval
        /// Keys identify supported options; a missing `headed` key means it is not supported.
        public let launchOptions: [String: Bool]
        public var headed: Bool { launchOptions["headed"] == true }
        public let permissionNeeded: MCPPermissionReason?
        public let lastError: String?
    }

    @Published public private(set) var rows: [Row] = []
    @Published public private(set) var anyEnabled = false
    private var relay: (any RemoteControlRelaying)?
    private var observation: Task<Void, Never>?
    private var actionErrors: [String: String] = [:]
    private var actionTokens: [String: UUID] = [:]
    private var refreshRevision: UInt64 = 0
    private var appliedRefreshRevision: UInt64 = 0

    init(relay: any RemoteControlRelaying) {
        self.relay = relay
        observation = Task { [weak self] in
            let updates = await relay.statusUpdates()
            for await _ in updates {
                guard !Task.isCancelled, let self else { return }
                await self.refresh()
            }
        }
    }

    deinit {
        observation?.cancel()
        if let relay { Task { await relay.shutdown() } }
    }

    public func setEnabled(_ enabled: Bool, id: String) async {
        await perform(id: id) { try await $0.setEnabled(enabled, id: id) }
    }

    public func setIdleTimeout(_ seconds: TimeInterval, id: String) async {
        await perform(id: id) { try await $0.setIdleTimeout(seconds, id: id) }
    }

    public func setLaunchOption(_ key: String, value: Bool, id: String) async {
        await perform(id: id) { try await $0.setLaunchOption(key, value: value, id: id) }
    }

    func detach() -> (any RemoteControlRelaying)? {
        let previous = relay
        relay = nil
        observation?.cancel()
        observation = nil
        rows = []
        anyEnabled = false
        return previous
    }

    private func perform(id: String, action: (any RemoteControlRelaying) async throws -> Void) async {
        guard let relay, !Task.isCancelled else { return }
        let token = UUID()
        actionTokens[id] = token
        actionErrors[id] = nil
        do {
            try await action(relay)
        } catch {
            guard self.relay != nil else { return }
            if actionTokens[id] == token, !(error is CancellationError) {
                actionErrors[id] = Self.message(for: error, id: id)
            }
        }
        await refresh()
    }

    private func refresh() async {
        guard let relay else { return }
        refreshRevision &+= 1
        let revision = refreshRevision
        let entries = await relay.entries()
        let statuses = await relay.statuses()
        guard self.relay != nil, revision > appliedRefreshRevision else { return }
        appliedRefreshRevision = revision
        let nextRows = entries.map { entry in
            let status = statuses[entry.id] ?? (entry.enabled ? .on : .off)
            var failure: String?
            var permission: MCPPermissionReason?
            switch status {
            case .permissionNeeded(let reason): permission = reason
            case .error(.exited): failure = uiText("The MCP server could not start or stopped unexpectedly.")
            case .error(.startupTimedOut): failure = uiText("The MCP server took too long to start.")
            default: break
            }
            return Row(
                id: entry.id, title: entry.id, enabled: status == .starting || status == .on,
                status: status, idleTimeoutSeconds: entry.idleTimeoutSeconds,
                launchOptions: entry.optionArguments.reduce(into: entry.launchOptions) { options, option in
                    if options[option.key] == nil { options[option.key] = false }
                }, permissionNeeded: permission, lastError: actionErrors[entry.id] ?? failure
            )
        }
        if rows != nextRows { rows = nextRows }
        let enabled = nextRows.contains(where: \.enabled)
        if anyEnabled != enabled { anyEnabled = enabled }
    }

    private static func message(for error: Error, id: String) -> String {
        switch error as? MCPChildError {
        case .executableNotFound where id == ClientMCPEntry.agentDesktop.id: uiText("agent-desktop not found. Install it and try again.")
        case .executableNotFound: uiText("The MCP server executable was not found.")
        case .startupTimedOut: uiText("The MCP server took too long to start.")
        case .exited, .notStarted, .invalidResponse, .remoteError, .frameTooLarge:
            uiText("The MCP server could not start or stopped unexpectedly.")
        // Errors can contain paths or other local details; do not present raw strings.
        default: uiText("Could not update remote control. Try again.")
        }
    }
}
#endif
