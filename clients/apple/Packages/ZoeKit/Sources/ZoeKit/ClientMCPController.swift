import Foundation

public enum MCPPermissionReason: Equatable, Sendable { case accessibility, screenRecording }
public enum ClientMCPStatus: Equatable, Sendable {
    case off, starting, on, permissionNeeded(MCPPermissionReason), error(ClientMCPFailure)
}
public enum ClientMCPFailure: Equatable, Sendable { case exited, startupTimedOut }

public protocol MCPPermissionChecking: Sendable {
    func missingPermission(for entry: ClientMCPEntry) -> MCPPermissionReason?
    func requestPermission(_ reason: MCPPermissionReason)
}

public extension MCPPermissionChecking {
    func requestPermission(_ reason: MCPPermissionReason) {}
}

public protocol MCPTimerClock: Sendable {
    var now: TimeInterval { get async }
    func sleep(seconds: TimeInterval) async throws
}

public extension MCPTimerClock {
    var now: TimeInterval { get async { ProcessInfo.processInfo.systemUptime } }
}

public struct SystemMCPTimerClock: MCPTimerClock {
    public init() {}
    public func sleep(seconds: TimeInterval) async throws {
        try await Task.sleep(for: .seconds(seconds))
    }
}

public protocol ClientMCPTransport: Sendable {
    func setNeeded(_ needed: Bool) async
    func send(_ data: Data) async throws
}

public actor ClientMCPController {
    public static let startupTimeoutSeconds: TimeInterval = 15
    public typealias ChildFactory = @Sendable (ClientMCPEntry, @escaping @Sendable () -> Void) throws -> any MCPChildProcess
    public static func processFactory(resolver: any MCPExecutableResolving = PATHMCPExecutableResolver()) -> ChildFactory {
        { entry, onExit in
            guard let executable = resolver.resolve(entry.command) else { throw MCPChildError.executableNotFound }
            let inherited = ProcessInfo.processInfo.environment
            var environment = inherited
            for (name, reference) in entry.environmentReferences {
                if let value = inherited[reference] { environment[name] = value }
                else { environment.removeValue(forKey: name) }
            }
            return FoundationMCPChildProcess(executable: executable, arguments: entry.effectiveArguments, environment: environment, onExit: onExit)
        }
    }
    private var registry: ClientMCPRegistry
    private let permissions: any MCPPermissionChecking
    private let clock: any MCPTimerClock
    private let factory: ChildFactory
    private let transport: any ClientMCPTransport
    private var children: [String: any MCPChildProcess] = [:]
    private var catalogs: [String: [JSONValue]] = [:]
    private var timers: [String: Task<Void, Never>] = [:]
    private var timerTokens: [String: UUID] = [:]
    private var generations: [String: UUID] = [:]
    private struct Startup {
        let task: Task<Void, Never>
        let timeout: Task<Void, Never>
        let continuation: CheckedContinuation<Void, Error>
    }
    private struct Active {
        let token: UUID
        let call: ClientMCPCall
        let task: Task<Void, Never>
    }
    private var startups: [String: Startup] = [:]
    private var retirements: [String: (UUID, Task<Void, Never>)] = [:]
    private var queues: [String: [ClientMCPCall]] = [:]
    private var running: [String: Active] = [:]
    private var cancelling: [String: UUID] = [:]
    // The contract does not define a window after which an ID can be forgotten.
    private var outcomes: [String: ClientMCPResult] = [:]
    private var seen = Set<String>()
    private var connection: UUID?
    private var shuttingDown = false
    public private(set) var statuses: [String: ClientMCPStatus] = [:]
    private var statusObservers: [UUID: AsyncStream<[String: ClientMCPStatus]>.Continuation] = [:]

    public init(registry: ClientMCPRegistry, permissions: any MCPPermissionChecking, clock: any MCPTimerClock = SystemMCPTimerClock(), transport: any ClientMCPTransport, factory: @escaping ChildFactory) {
        self.registry = registry
        self.permissions = permissions
        self.clock = clock
        self.transport = transport
        self.factory = factory
        for entry in registry.entries { statuses[entry.id] = .off }
    }

    deinit {
        for timer in timers.values { timer.cancel() }
        for startup in startups.values {
            startup.task.cancel()
            startup.timeout.cancel()
            startup.continuation.resume(throwing: CancellationError())
        }
        for active in running.values { active.task.cancel() }
        for observer in statusObservers.values { observer.finish() }
        let children = Array(children.values)
        let transport = transport
        Task {
            await transport.setNeeded(false)
            for child in children { await child.terminate() }
        }
    }

    public func entries() -> [ClientMCPEntry] {
        registry.entries.map { entry in
            var entry = entry
            entry.enabled = children[entry.id] != nil
            return entry
        }
    }

    public func statusUpdates() -> AsyncStream<[String: ClientMCPStatus]> {
        let id = UUID()
        return AsyncStream(bufferingPolicy: .bufferingNewest(1)) { continuation in
            statusObservers[id] = continuation
            continuation.yield(statuses)
            continuation.onTermination = { [weak self] _ in Task { await self?.removeObserver(id) } }
        }
    }

    private func removeObserver(_ id: UUID) { statusObservers.removeValue(forKey: id) }
    private func setStatus(_ status: ClientMCPStatus, for id: String) {
        statuses[id] = status
        for continuation in statusObservers.values { continuation.yield(statuses) }
    }

    public func setIdleTimeout(_ seconds: TimeInterval, for id: String) throws {
        guard !shuttingDown, seconds > 0, seconds.isFinite, var entry = registry.entries.first(where: { $0.id == id }) else { return }
        entry.idleTimeoutSeconds = seconds
        try save(entry)
        if children[id] != nil { resetTimer(entry) }
    }

    public func enable(_ id: String) async throws {
        guard !shuttingDown, let entry = registry.entries.first(where: { $0.id == id }), children[id] == nil else { return }
        if let reason = permissions.missingPermission(for: entry) {
            permissions.requestPermission(reason)
            if let remaining = permissions.missingPermission(for: entry) {
                setStatus(.permissionNeeded(remaining), for: id)
                return
            }
        }
        setStatus(.starting, for: id)
        let generation = UUID()
        generations[id] = generation
        let child: any MCPChildProcess
        do { child = try factory(entry) { [weak self] in Task { await self?.childExited(id, generation: generation) } } }
        catch {
            generations.removeValue(forKey: id)
            setStatus(.error(.exited), for: id)
            throw error
        }
        children[id] = child
        resetTimer(entry)
        let retirement = retirements[id]?.1
        let timeoutSeconds = Self.startupTimeoutSeconds
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { continuation in
                let start = Task { [weak self] in
                    await retirement?.value
                    guard !Task.isCancelled else { return }
                    await self?.updateTransportNeed()
                    guard !Task.isCancelled else { return }
                    await self?.publish()
                    do {
                        let tools = try await child.start()
                        await self?.started(id, generation: generation, tools: tools)
                    } catch { await self?.childExited(id, generation: generation, error: error) }
                }
                let clock = clock
                let timeout = Task { [weak self] in
                    do { try await clock.sleep(seconds: timeoutSeconds) } catch { return }
                    guard !Task.isCancelled else { return }
                    await self?.startupTimedOut(id, generation: generation)
                }
                startups[id] = Startup(task: start, timeout: timeout, continuation: continuation)
            }
        } onCancel: {
            Task { await self.cancelStartup(id, generation: generation) }
        }
    }

    private func cancelStartup(_ id: String, generation: UUID) async {
        guard generations[id] == generation, startups[id] != nil else { return }
        await disable(id, code: "mcp_disabled", status: .off, startupError: CancellationError())
    }

    private func started(_ id: String, generation: UUID, tools: [JSONValue]) async {
        guard generations[id] == generation, let startup = startups.removeValue(forKey: id) else { return }
        startup.timeout.cancel()
        catalogs[id] = tools
        setStatus(.on, for: id)
        await publish()
        runNext(id)
        startup.continuation.resume()
    }

    private func startupTimedOut(_ id: String, generation: UUID) async {
        guard generations[id] == generation, startups[id] != nil else { return }
        await disable(id, code: "mcp_exited", status: .error(.startupTimedOut), startupError: MCPChildError.startupTimedOut)
    }

    public func disable(_ id: String) async { await disable(id, code: "mcp_disabled", status: .off) }

    func shutdown() async {
        shuttingDown = true
        // Fence new enables and remove every child before awaiting any retirements.
        let cleanup = Array(statuses.keys).compactMap { stop($0, code: "mcp_disabled", status: .off) }
        for task in cleanup { await task.value }
        await transport.setNeeded(false)
    }

    public func setLaunchOption(_ key: String, value: Bool, for id: String) async throws {
        guard !shuttingDown, var entry = registry.entries.first(where: { $0.id == id }), entry.optionArguments[key] != nil,
              entry.launchOptions[key] != value else { return }
        entry.launchOptions[key] = value
        try save(entry)
        guard children[id] != nil else { return }
        _ = stop(id, code: "mcp_restarted", status: .off)
        // Install the replacement before suspending, but don't start it until cleanup finishes.
        do { try await enable(id) }
        catch { await retirements[id]?.1.value; await updateTransportNeed(); throw error }
        if children[id] == nil { await retirements[id]?.1.value; await updateTransportNeed() }
    }

    public func connectionOpened() async {
        guard connection == nil else { return }
        connection = UUID()
        seen.removeAll()
        outcomes.removeAll()
        await publish()
    }

    public func connectionClosed() async {
        connection = nil
        let active = running
        running.removeAll()
        queues.removeAll()
        seen.removeAll()
        outcomes.removeAll()
        let children = children
        for (id, active) in active { cancelling[id] = active.token; active.task.cancel() }
        await withTaskGroup(of: Void.self) { group in
            for (id, active) in active {
                guard let child = children[id], let rpcID = active.call.rpcID else { continue }
                group.addTask { await child.cancel(id: rpcID) }
            }
        }
        for (id, active) in active where cancelling[id] == active.token {
            cancelling.removeValue(forKey: id)
            runNext(id)
        }
    }

    public func receive(_ data: Data) async {
        guard connection != nil, let value = try? JSONDecoder().decode(JSONValue.self, from: data), case let .object(frame) = value,
              frame["channel"] == .string("mcp"), case let .string(type)? = frame["type"] else { return }
        if type == "cancel", case let .string(id)? = frame["request_id"] { await cancel(id); return }
        guard type == "call", let call = try? JSONDecoder().decode(ClientMCPCall.self, from: data) else { return }
        if let outcome = outcomes[call.requestID] { await send(outcome, connection: connection); return }
        guard seen.insert(call.requestID).inserted else { return }
        guard let entry = registry.entries.first(where: { $0.id == call.mcpID }), children[call.mcpID] != nil else {
            await finish(call, code: "mcp_unavailable")
            return
        }
        resetTimer(entry)
        guard call.toolName != nil else {
            await finish(call, code: "mcp_unavailable")
            return
        }
        queues[call.mcpID, default: []].append(call)
        runNext(call.mcpID)
    }

    private func runNext(_ id: String) {
        guard running[id] == nil, cancelling[id] == nil, let tools = catalogs[id], let generation = generations[id], var queue = queues.removeValue(forKey: id),
              !queue.isEmpty, let child = children[id] else { return }
        let call = queue.removeFirst()
        if !queue.isEmpty { queues[id] = queue }
        let token = UUID()
        let task = Task { [weak self] in
            guard tools.contains(where: {
                if case let .object(tool) = $0, let name = call.toolName { return tool["name"] == .string(name) }
                return false
            }) else {
                await self?.completed(call, token: token, result: nil, errorCode: "mcp_unavailable")
                return
            }
            do {
                try Task.checkCancellation()
                let result = try await child.call(call.jsonrpc)
                await self?.completed(call, token: token, result: result)
            } catch MCPChildError.frameTooLarge {
                await self?.completed(call, token: token, result: nil, errorCode: "frame_too_large")
            } catch MCPChildError.exited {
                await self?.childExited(id, generation: generation)
            } catch { await self?.completed(call, token: token, result: nil) }
        }
        running[id] = Active(token: token, call: call, task: task)
    }

    private func completed(_ call: ClientMCPCall, token: UUID, result: JSONValue?, errorCode: String = "mcp_error") async {
        guard running[call.mcpID]?.token == token else { return }
        running.removeValue(forKey: call.mcpID)
        if let result, case .object = result {
            await finish(call, result: result)
        } else { await finish(call, code: errorCode) }
        runNext(call.mcpID)
    }

    private func cancel(_ requestID: String) async {
        for id in Array(queues.keys) { queues[id]?.removeAll { $0.requestID == requestID } }
        guard let (id, active) = running.first(where: { $0.value.call.requestID == requestID }) else { return }
        running.removeValue(forKey: id)
        cancelling[id] = active.token
        active.task.cancel()
        if let rpcID = active.call.rpcID { await children[id]?.cancel(id: rpcID) }
        guard cancelling[id] == active.token else { return }
        cancelling.removeValue(forKey: id)
        runNext(id)
    }

    private func finish(_ call: ClientMCPCall, result: JSONValue? = nil, code: String? = nil) async {
        let frame = ClientMCPResult(requestID: call.requestID, ok: code == nil, result: result, error: code.map { .init(code: $0, message: nil) })
        guard let connection else { return }
        outcomes[call.requestID] = frame
        await send(frame, connection: connection)
    }

    private func send(_ frame: ClientMCPResult, connection: UUID?) async {
        guard let connection, self.connection == connection, let data = try? JSONEncoder().encode(frame) else { return }
        if data.count > ClientWebSocketLimits.frameBytes {
            let failure = ClientMCPResult(requestID: frame.requestID, ok: false, result: nil, error: .init(code: "frame_too_large", message: nil))
            outcomes[frame.requestID] = failure
            if let encoded = try? JSONEncoder().encode(failure) { try? await transport.send(encoded) }
        } else { try? await transport.send(data) }
    }

    private func publish() async {
        guard connection != nil else { return }
        var stateCatalogs = catalogs
        for id in children.keys where stateCatalogs[id] == nil { stateCatalogs[id] = [] }
        let state = ClientMCPState(catalogs: stateCatalogs)
        if let data = try? JSONEncoder().encode(state), data.count <= ClientWebSocketLimits.frameBytes { try? await transport.send(data) }
    }

    private func save(_ entry: ClientMCPEntry) throws {
        var entries = registry.entries
        guard let index = entries.firstIndex(where: { $0.id == entry.id }) else { return }
        entries[index] = entry
        try registry.replace(entries)
    }

    private func resetTimer(_ entry: ClientMCPEntry) {
        timers[entry.id]?.cancel()
        let token = UUID()
        timerTokens[entry.id] = token
        let clock = clock
        timers[entry.id] = Task { [weak self] in
            do { try await clock.sleep(seconds: entry.idleTimeoutSeconds) } catch { return }
            guard !Task.isCancelled else { return }
            await self?.timerExpired(entry.id, token: token)
        }
    }

    private func timerExpired(_ id: String, token: UUID) async {
        guard timerTokens[id] == token else { return }
        await disable(id, code: "mcp_disabled", status: .off)
    }

    private func childExited(_ id: String, generation: UUID, error: Error = MCPChildError.exited) async {
        guard generations[id] == generation else { return }
        await disable(id, code: "mcp_exited", status: .error(.exited), startupError: error)
    }

    private func disable(_ id: String, code: String, status: ClientMCPStatus, startupError: Error? = nil) async {
        let cleanup = stop(id, code: code, status: status, startupError: startupError)
        await cleanup?.value
        await updateTransportNeed()
    }

    private func stop(_ id: String, code: String, status: ClientMCPStatus, startupError: Error? = nil) -> Task<Void, Never>? {
        guard statuses[id] != nil else { return nil }
        generations.removeValue(forKey: id)
        timers.removeValue(forKey: id)?.cancel()
        timerTokens.removeValue(forKey: id)
        cancelling.removeValue(forKey: id)
        let startup = startups.removeValue(forKey: id)
        startup?.task.cancel()
        startup?.timeout.cancel()
        let child = children.removeValue(forKey: id)
        let active = running.removeValue(forKey: id)
        active?.task.cancel()
        let queued = queues.removeValue(forKey: id) ?? []
        catalogs.removeValue(forKey: id)
        setStatus(status, for: id)
        guard let child else { return retirements[id]?.1 }
        let epoch = connection
        let token = UUID()
        let frames = ([active?.call].compactMap { $0 } + queued).map { call in
            let frame = ClientMCPResult(requestID: call.requestID, ok: false, result: nil, error: .init(code: code, message: nil))
            if epoch != nil { outcomes[call.requestID] = frame }
            return frame
        }
        let previous = retirements[id]?.1
        let cleanup = Task { [weak self] in
            await child.terminate()
            await previous?.value
            for frame in frames { await self?.send(frame, connection: epoch) }
            await self?.publish()
            await self?.retired(id, token: token)
            if let startupError { startup?.continuation.resume(throwing: startupError) }
            else { startup?.continuation.resume() }
        }
        retirements[id] = (token, cleanup)
        return cleanup
    }

    private func retired(_ id: String, token: UUID) {
        if retirements[id]?.0 == token { retirements.removeValue(forKey: id) }
    }

    private func updateTransportNeed() async { await transport.setNeeded(!children.isEmpty) }

}

public enum ClientWebSocketLimits {
    public static let frameBytes = 8_388_608
    public static let heartbeatSeconds: TimeInterval = 30
}
