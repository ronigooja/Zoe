import Darwin
import Foundation

public enum MCPChildError: Error, Equatable {
    case exited, invalidResponse, remoteError, notStarted, executableNotFound, startupTimedOut, frameTooLarge
}

public protocol MCPChildProcess: Sendable {
    func start() async throws -> [JSONValue]
    func call(_ request: JSONValue) async throws -> JSONValue
    func cancel(id: JSONValue) async
    func terminate() async
}

public actor FoundationMCPChildProcess: MCPChildProcess {
    private static let terminationGrace: Duration = .milliseconds(250)
    private let executable: URL
    private let arguments: [String]
    private let environment: [String: String]
    private let onExit: @Sendable () -> Void
    private var process: Process?
    private var input: FileHandle?
    private var output: MCPReadPipe?
    private var stderr: FileHandle?
    private var reader: Task<Void, Never>?
    private var writer: DispatchIO?
    private var lines = MCPStdioLines()
    private var pending: [JSONValueKey: CheckedContinuation<JSONValue, Error>] = [:]
    private var callIDs: [JSONValueKey: JSONValue] = [:]
    private var nextID: Int64 = 0
    private var stopping = false
    private var terminationWaiters: [CheckedContinuation<Void, Never>] = []
    private var killTask: Task<Void, Never>?

    public init(executable: URL, arguments: [String], environment: [String: String] = ProcessInfo.processInfo.environment, onExit: @escaping @Sendable () -> Void = {}) {
        self.executable = executable
        self.arguments = arguments
        self.environment = environment
        self.onExit = onExit
    }

    deinit {
        if let process, process.isRunning { _ = kill(process.processIdentifier, SIGKILL) }
        reader?.cancel()
        output?.close()
        writer?.close(flags: .stop)
        stderr?.readabilityHandler = nil
        try? input?.close()
        try? stderr?.close()
    }

    public func start() async throws -> [JSONValue] {
        try Task.checkCancellation()
        guard !stopping, process == nil else { throw MCPChildError.invalidResponse }
        let child = Process()
        let stdinPipe = Pipe(), stdoutPipe = Pipe(), stderrPipe = Pipe()
        child.executableURL = executable
        child.arguments = arguments
        child.environment = environment
        child.standardInput = stdinPipe
        child.standardOutput = stdoutPipe
        child.standardError = stderrPipe
        let out = stdoutPipe.fileHandleForReading
        let err = stderrPipe.fileHandleForReading
        err.readabilityHandler = { handle in
            if handle.availableData.isEmpty { handle.readabilityHandler = nil }
        }
        child.terminationHandler = { [weak self] _ in Task { await self?.exited() } }
        process = child
        input = stdinPipe.fileHandleForWriting
        stderr = err
        do { try child.run() } catch {
            cleanup()
            throw error
        }
        do {
            let fd = dup(stdinPipe.fileHandleForWriting.fileDescriptor)
            guard fd >= 0 else { throw MCPChildError.notStarted }
            guard fcntl(fd, F_SETNOSIGPIPE, 1) != -1 else { _ = Darwin.close(fd); throw MCPChildError.notStarted }
            writer = DispatchIO(type: .stream, fileDescriptor: fd, queue: .global()) { _ in _ = Darwin.close(fd) }
            let pipe = MCPReadPipe(out)
            output = pipe
            reader = Task { [weak self] in
                while !Task.isCancelled, let data = await pipe.next() { await self?.receive(data) }
                if !Task.isCancelled { await self?.stdoutClosed() }
            }
            return try await initialize()
        } catch {
            await terminate()
            throw error
        }
    }

    private func initialize() async throws -> [JSONValue] {
        let initialization = try await request(method: "initialize", params: .object([
            "protocolVersion": .string("2025-11-25"), "capabilities": .object([:]),
            "clientInfo": .object(["name": .string("zoe-client"), "version": .string("1.0.0")])
        ]))
        guard case .object = initialization else { throw MCPChildError.invalidResponse }
        try await write(.object(["jsonrpc": .string("2.0"), "method": .string("notifications/initialized")]))
        var tools: [JSONValue] = []
        var cursor: String?
        var seen = Set<String>()
        var remaining = ClientWebSocketLimits.frameBytes
        repeat {
            let result = try await request(method: "tools/list", params: .object(cursor.map { ["cursor": .string($0)] } ?? [:]))
            guard case let .object(object) = result, case let .array(page)? = object["tools"] else { throw MCPChildError.invalidResponse }
            remaining -= try JSONEncoder().encode(result).count
            guard remaining >= 0 else { throw MCPChildError.frameTooLarge }
            tools.append(contentsOf: page)
            if case let .string(value)? = object["nextCursor"], !value.isEmpty {
                guard seen.insert(value).inserted else { throw MCPChildError.invalidResponse }
                cursor = value
            } else { cursor = nil }
        } while cursor != nil
        return tools
    }

    public func call(_ request: JSONValue) async throws -> JSONValue {
        guard case var .object(object) = request, let id = object["id"] else { throw MCPChildError.invalidResponse }
        let key = JSONValueKey(id)
        guard callIDs[key] == nil else { throw MCPChildError.invalidResponse }
        // Reconnect may reuse the server ID while the same child still has a
        // cancelled request in flight. Never reuse a child-side ID.
        let childID = allocateID()
        callIDs[key] = childID
        defer { if callIDs[key] == childID { callIDs.removeValue(forKey: key) } }
        object["id"] = childID
        return try await sendRequest(.object(object), id: childID)
    }

    func hasPendingRequest(_ id: JSONValue) -> Bool {
        callIDs[JSONValueKey(id)].map { pending[JSONValueKey($0)] != nil } ?? false
    }

    public func cancel(id: JSONValue) async {
        guard let childID = callIDs.removeValue(forKey: JSONValueKey(id)),
              let continuation = pending.removeValue(forKey: JSONValueKey(childID)) else { return }
        enqueue(.object(["jsonrpc": .string("2.0"), "method": .string("notifications/cancelled"), "params": .object(["requestId": childID])])) { _ in }
        continuation.resume(throwing: CancellationError())
    }

    public func terminate() async {
        await withCheckedContinuation { continuation in
            terminationWaiters.append(continuation)
            guard !stopping else {
                if process == nil { finishTermination() }
                return
            }
            stopping = true
            closeInput()
            failPending()
            guard let process, process.isRunning else { exited(); return }
            process.terminate()

            let clock = ContinuousClock()
            let deadline = clock.now.advanced(by: Self.terminationGrace)
            killTask = Task { [weak self] in
                do { try await clock.sleep(until: deadline) } catch { return }
                await self?.forceStop()
            }
            // The Process termination handler resumes the waiters once the child exits.
        }
    }

    private func forceStop() {
        if let process, process.isRunning { _ = kill(process.processIdentifier, SIGKILL) }
    }

    private func request(method: String, params: JSONValue) async throws -> JSONValue {
        let id = allocateID()
        return try await sendRequest(.object(["jsonrpc": .string("2.0"), "id": id, "method": .string(method), "params": params]), id: id)
    }

    private func allocateID() -> JSONValue {
        nextID += 1
        return .string("zoe-\(nextID)")
    }

    private func sendRequest(_ request: JSONValue, id: JSONValue) async throws -> JSONValue {
        try Task.checkCancellation()
        guard !stopping else { throw MCPChildError.exited }
        let key = JSONValueKey(id)
        guard pending[key] == nil else { throw MCPChildError.invalidResponse }
        return try await withCheckedThrowingContinuation { continuation in
            pending[key] = continuation
            enqueue(request) { [weak self] error in
                if error != nil { Task { await self?.stdoutClosed() } }
            }
        }
    }

    private func write(_ value: JSONValue) async throws {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            enqueue(value) { error in
                if let error { continuation.resume(throwing: error) } else { continuation.resume() }
            }
        }
    }

    private func enqueue(_ value: JSONValue, completion: @escaping @Sendable (Error?) -> Void) {
        guard let writer else { completion(MCPChildError.notStarted); return }
        // Bound outstanding writes even if a child never reads stdin.
        guard writes < 4 else { completion(MCPChildError.exited); return }
        do {
            var data = try JSONEncoder().encode(value)
            data.append(10)
            let bytes = data.withUnsafeBytes { DispatchData(bytes: $0) }
            writes += 1
            writer.write(offset: 0, data: bytes, queue: .global()) { [weak self] done, _, error in
                if done {
                    Task { await self?.writeCompleted() }
                    completion(error == 0 ? nil : MCPChildError.exited)
                }
            }
        } catch { completion(error) }
    }

    private var writes = 0
    private func writeCompleted() { writes -= 1 }

    private func receive(_ data: Data) {
        guard !stopping else { return }
        var lines = self.lines
        let tooLarge = lines.receive(data) { line in
            guard let value = try? JSONDecoder().decode(JSONValue.self, from: line),
                  case let .object(object) = value, object["jsonrpc"] == .string("2.0"), let id = object["id"],
                  (object["result"] != nil || object["error"] != nil),
                  let continuation = pending.removeValue(forKey: JSONValueKey(id)) else { return }
            if let result = object["result"] { continuation.resume(returning: result) }
            else { continuation.resume(throwing: MCPChildError.remoteError) }
        }
        self.lines = lines
        if tooLarge {
            let waiting = pending.values
            pending.removeAll()
            for continuation in waiting { continuation.resume(throwing: MCPChildError.frameTooLarge) }
        }
    }

    private func stdoutClosed() {
        guard !stopping else { return }
        if let process, process.isRunning { _ = kill(process.processIdentifier, SIGKILL) }
        exited()
    }

    private func exited() {
        let notify = !stopping
        stopping = true
        failPending()
        cleanup()
        finishTermination()
        if notify { onExit() }
    }

    private func finishTermination() {
        killTask?.cancel()
        killTask = nil
        let waiters = terminationWaiters
        terminationWaiters.removeAll()
        for waiter in waiters { waiter.resume() }
    }

    private func failPending() {
        let waiting = pending.values
        pending.removeAll()
        for continuation in waiting { continuation.resume(throwing: MCPChildError.exited) }
    }

    private func closeInput() {
        writer?.close(flags: .stop)
        writer = nil
        try? input?.close()
        input = nil
    }

    private func cleanup() {
        reader?.cancel()
        self.reader = nil
        closeInput()
        stderr?.readabilityHandler = nil
        output?.close()
        try? stderr?.close()
        output = nil
        stderr = nil
        process = nil
    }
}

/// Read only on dispatch readiness and only when the actor is ready for another
/// chunk. All source operations and descriptor closure use one serial queue.
private final class MCPReadPipe: @unchecked Sendable {
    private let queue = DispatchQueue(label: "zoe.mcp.stdout")
    private let source: DispatchSourceRead
    private let handle: FileHandle
    private var waiting: CheckedContinuation<Data?, Never>?
    private var closed = false

    init(_ handle: FileHandle) {
        self.handle = handle
        source = DispatchSource.makeReadSource(fileDescriptor: handle.fileDescriptor, queue: queue)
        source.setEventHandler { [weak self] in self?.read() }
        source.setCancelHandler { try? handle.close() }
    }

    func next() async -> Data? {
        await withCheckedContinuation { continuation in
            queue.async {
                guard !self.closed else { continuation.resume(returning: nil); return }
                self.waiting = continuation
                self.source.resume()
            }
        }
    }

    private func read() {
        guard !closed, let waiting else { return }
        self.waiting = nil
        source.suspend()
        var data = Data(count: min(max(1, Int(source.data)), 64 * 1024))
        let count = data.withUnsafeMutableBytes { Darwin.read(handle.fileDescriptor, $0.baseAddress, $0.count) }
        if count > 0 {
            data.count = count
            waiting.resume(returning: data)
        } else if count < 0 && errno == EINTR {
            self.waiting = waiting
            source.resume()
        } else {
            waiting.resume(returning: nil)
            finish()
        }
    }

    func close() { queue.async { self.finish() } }

    private func finish() {
        guard !closed else { return }
        closed = true
        let waiting = self.waiting
        self.waiting = nil
        source.cancel()
        if let waiting { waiting.resume(returning: nil) } else { source.resume() }
    }
}

struct MCPStdioLines {
    // Allow a full wire payload plus JSON-RPC envelope; discard an oversized
    // unterminated line until its next delimiter without growing the buffer.
    static let maxBytes = ClientWebSocketLimits.frameBytes + 64 * 1024
    private var buffer = Data()
    private var discarding = false

    @discardableResult
    mutating func receive(_ bytes: Data, line: (Data) -> Void) -> Bool {
        var tooLarge = false
        var rest = bytes[...]
        while !rest.isEmpty {
            let newline = rest.firstIndex(of: 10)
            let chunk = rest[..<(newline ?? rest.endIndex)]
            if !discarding {
                if chunk.count <= Self.maxBytes - buffer.count { buffer.append(contentsOf: chunk) }
                else { tooLarge = true; discarding = true; buffer.removeAll(keepingCapacity: false) }
            }
            guard let newline else { break }
            if !discarding { line(buffer) }
            buffer.removeAll(keepingCapacity: true)
            discarding = false
            rest = rest[rest.index(after: newline)...]
        }
        return tooLarge
    }
}

private struct JSONValueKey: Hashable {
    let data: Data
    init(_ value: JSONValue) { data = (try? JSONEncoder().encode(value)) ?? Data() }
}
