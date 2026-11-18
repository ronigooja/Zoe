import Foundation
import CryptoKit

public struct PendingCommand: Codable, Equatable, Identifiable, Sendable {
    public var idempotencyKey: String
    public var kind: String
    public var expectedVersion: Int64?
    public var payload: JSONValue
    public var createdAt: String
    public var workspaceID: String?
    public var expectedResourceType: String?
    public var expectedResourceID: String?
    public var id: String { idempotencyKey }
}

public actor CommandJournal {
    private let fileURL: URL
    private let fileManager = FileManager.default
    private let encoder: JSONEncoder
    private let decoder = JSONDecoder()

    public init(fileURL: URL) throws {
        self.fileURL = fileURL
        encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
    }

    public init(serverURL: URL, credentialScope: String) throws {
        let fileManager = FileManager.default
        let root = try fileManager.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
        let directory = root.appendingPathComponent("Zoe", isDirectory: true).appendingPathComponent("Commands", isDirectory: true)
        try fileManager.createDirectory(at: directory, withIntermediateDirectories: true)
        var components = URLComponents(url: serverURL, resolvingAgainstBaseURL: false)
        components?.query = nil
        components?.fragment = nil
        let scope = "\(components?.string ?? serverURL.absoluteString)\u{0}\(credentialScope)"
        let digest = SHA256.hash(data: Data(scope.utf8)).map { String(format: "%02x", $0) }.joined()
        self.fileURL = directory.appendingPathComponent("\(digest).json")
        encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
    }

    public func discard() throws {
        if fileManager.fileExists(atPath: fileURL.path) { try fileManager.removeItem(at: fileURL) }
    }

    public func all() throws -> [PendingCommand] {
        guard fileManager.fileExists(atPath: fileURL.path) else { return [] }
        do { return try decoder.decode([PendingCommand].self, from: Data(contentsOf: fileURL)) }
        catch { throw ZoeError.persistence("Pending command journal is unreadable: \(error.localizedDescription)") }
    }

    public func append(_ command: PendingCommand) throws {
        var current = try all()
        if let existing = current.first(where: { $0.idempotencyKey == command.idempotencyKey }) {
            guard existing.kind == command.kind, existing.expectedVersion == command.expectedVersion, existing.payload == command.payload,
                  existing.workspaceID == command.workspaceID, existing.expectedResourceType == command.expectedResourceType,
                  existing.expectedResourceID == command.expectedResourceID else {
                throw ZoeError.http(status: 409, code: "local_idempotency_conflict", message: "The saved idempotency key is already bound to a different command.", details: nil)
            }
            return
        }
        current.append(command)
        try save(current)
    }

    public func remove(idempotencyKey: String) throws {
        let current = try all().filter { $0.idempotencyKey != idempotencyKey }
        try save(current)
    }

    private func save(_ commands: [PendingCommand]) throws {
        do {
            let data = try encoder.encode(commands)
            try fileManager.createDirectory(at: fileURL.deletingLastPathComponent(), withIntermediateDirectories: true)
            try data.write(to: fileURL, options: .atomic)

            try fileManager.setAttributes([.posixPermissions: NSNumber(value: 0o600)], ofItemAtPath: fileURL.path)
            try fileManager.setAttributes([.posixPermissions: NSNumber(value: 0o700)], ofItemAtPath: fileURL.deletingLastPathComponent().path)

        } catch {
            throw ZoeError.persistence("Could not persist pending command: \(error.localizedDescription)")
        }
    }
}

public struct CommandOutcome: Sendable {
    public let receipt: CommandReceipt
    public let recovered: Bool
    public let command: PendingCommand
}

public actor CommandCoordinator {
    private let api: APIClient
    private let journal: CommandJournal

    public init(api: APIClient, journal: CommandJournal) {
        self.api = api
        self.journal = journal
    }

    /// A command is journaled before POST. A retry first checks the durable receipt by key.
    public func submit(kind: String, expectedVersion: Int64? = nil, payload: JSONValue, workspaceID: String? = nil, idempotencyKey: String? = nil,
                       isValid: @MainActor @Sendable () -> Bool = { true }) async throws -> CommandOutcome {
        let command = PendingCommand(
            idempotencyKey: idempotencyKey ?? UUID().uuidString.lowercased(),
            kind: kind,
            expectedVersion: expectedVersion,
            payload: payload,
            createdAt: ISO8601DateFormatter().string(from: Date()),
            workspaceID: workspaceID,
            expectedResourceType: nil,
            expectedResourceID: nil
        )
        let alreadyPending = try await journal.all().contains { $0.idempotencyKey == command.idempotencyKey }
        try await requireValidity(isValid)
        try await journal.append(command)
        try await requireValidity(isValid)
        return try await deliver(command, checkReceiptFirst: alreadyPending || (kind == "voice.text_input" && idempotencyKey != nil), isValid: isValid)
    }

    public func recoverPending() async throws -> [CommandOutcome] {
        try await recoverPending(currentWorkspaceID: nil, enforceWorkspace: false, onOutcome: { _ in })
    }

    /// Reports each receipt as soon as its journal row is durably removed. Callers can
    /// reconcile accepted commands even if a later row in the same replay fails.
    public func recoverPending(onOutcome: @MainActor @Sendable (CommandOutcome) async -> Void) async throws -> [CommandOutcome] {
        try await recoverPending(currentWorkspaceID: nil, enforceWorkspace: false, onOutcome: onOutcome)
    }

    public func recoverPending(
        currentWorkspaceID: String?,
        enforceWorkspace: Bool = true,
        onOutcome: @MainActor @Sendable (CommandOutcome) async -> Void,
        onWorkspaceMismatch: @MainActor @Sendable (PendingCommand) async -> Void = { _ in },
        onFailure: @MainActor @Sendable (PendingCommand, ZoeError?) async -> Void = { _, _ in },
        isValid: @MainActor @Sendable () -> Bool = { true }
    ) async throws -> [CommandOutcome] {
        let commands = try await journal.all()
        try await requireValidity(isValid)
        var outcomes: [CommandOutcome] = []
        for command in commands where command.kind == "voice.turn.commit" || command.kind == "voice.turn.handoff" || command.kind == "voice.text_input" || command.kind == "ui.rendered" || command.kind == "ui.action" {
            try await requireValidity(isValid)
            guard !enforceWorkspace || (command.workspaceID != nil && currentWorkspaceID != nil && command.workspaceID == currentWorkspaceID) else {
                await onWorkspaceMismatch(command)
                try await requireValidity(isValid)
                continue
            }
            let outcome: CommandOutcome
            do { outcome = try await deliver(command, checkReceiptFirst: true, isValid: isValid) }
            catch {
                try await requireValidity(isValid)
                await onFailure(command, error as? ZoeError)
                try await requireValidity(isValid)
                throw error
            }
            try await requireValidity(isValid)
            outcomes.append(outcome)
            await onOutcome(outcome)
            try await requireValidity(isValid)
        }
        return outcomes
    }

    public func retryPending(idempotencyKey: String, currentWorkspaceID: String?) async throws -> CommandOutcome {
        guard let command = try await journal.all().first(where: { $0.idempotencyKey == idempotencyKey }) else {
            throw ZoeError.persistence("The saved action is no longer pending.")
        }
        guard let commandWorkspaceID = command.workspaceID,
              let currentWorkspaceID,
              commandWorkspaceID == currentWorkspaceID else { throw ZoeError.pendingCommandWorkspaceMismatch }
        return try await deliver(command, checkReceiptFirst: true)
    }

    @MainActor private func requireValidity(_ isValid: @MainActor @Sendable () -> Bool) throws {
        guard !Task.isCancelled, isValid() else { throw CancellationError() }
    }

    private func deliver(_ command: PendingCommand, checkReceiptFirst: Bool,
                         isValid: @MainActor @Sendable () -> Bool = { true }) async throws -> CommandOutcome {
        try await requireValidity(isValid)
        if checkReceiptFirst {
            do {
                let receipt = try await api.receipt(idempotencyKey: command.idempotencyKey)
                try await requireValidity(isValid)
                try validate(receipt, command: command)
                try await journal.remove(idempotencyKey: command.idempotencyKey)
                try await requireValidity(isValid)
                return CommandOutcome(receipt: receipt, recovered: true, command: command)
            } catch let error as ZoeError {
                try await requireValidity(isValid)
                guard case let .http(status, _, _, _) = error, status == 404 else { throw error }
            }
        }

        do {
            try await requireValidity(isValid)
            let receipt = try await api.submit(kind: command.kind, expectedVersion: command.expectedVersion, payload: command.payload, idempotencyKey: command.idempotencyKey)
            try await requireValidity(isValid)
            try validate(receipt, command: command)
            try await journal.remove(idempotencyKey: command.idempotencyKey)
            try await requireValidity(isValid)
            return CommandOutcome(receipt: receipt, recovered: false, command: command)
        } catch {
            try await requireValidity(isValid)
            // Keep the exact key and payload. The next attempt resolves a lost response first.
            if let error = error as? ZoeError,
               case let .http(status, _, _, _) = error,
               [400, 401, 403, 404, 409, 410, 413].contains(status) {
                try? await journal.remove(idempotencyKey: command.idempotencyKey)
                try await requireValidity(isValid)
            }
            throw error
        }
    }

    private func validate(_ receipt: CommandReceipt, command: PendingCommand) throws {
        guard receipt.kind == command.kind, receipt.idempotencyKey == command.idempotencyKey else { throw ZoeError.invalidResponse }
        if let workspaceID = command.workspaceID, receipt.workspaceId != workspaceID { throw ZoeError.invalidResponse }
        if let resourceType = command.expectedResourceType, receipt.resourceType != resourceType { throw ZoeError.invalidResponse }
        if let resourceID = command.expectedResourceID, receipt.resourceId != resourceID { throw ZoeError.invalidResponse }
        if receipt.status != "accepted" { throw ZoeError.invalidResponse }
        guard receipt.eventCursor.allSatisfy(\.isNumber) else { throw ZoeError.invalidResponse }
    }

}
