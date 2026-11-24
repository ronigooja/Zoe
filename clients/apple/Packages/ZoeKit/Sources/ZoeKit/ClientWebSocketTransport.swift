import Foundation

public enum ClientWebSocketError: Error, Equatable {
    case unauthorized, frameTooLarge, disconnected
}

public protocol ClientWebSocket: Sendable {
    func open() async throws
    func receive() async throws -> Data
    func send(_ data: Data) async throws
    func ping() async throws
    func close() async
    func closeTooLarge() async
}

public extension ClientWebSocket {
    func closeTooLarge() async { await close() }
}

public actor ClientWebSocketTransport: ClientMCPTransport {
    public typealias SocketFactory = @Sendable (URLRequest) -> any ClientWebSocket
    public typealias FrameHandler = @Sendable (Data) async -> Void
    private let api: APIClient
    private let factory: SocketFactory
    private let clock: any MCPTimerClock
    private let onOpen: @Sendable () async -> Void
    private let onClose: @Sendable () async -> Void
    private let onMCPFrame: FrameHandler
    private let diagnostic: @Sendable (String) -> Void
    private var task: Task<Void, Never>?
    private var socket: (any ClientWebSocket)?
    private var needed = false
    private var rejected = false
    private var generation = UUID()
    private var openConnection: UUID?
    private var closing = false
    private(set) var lastActivity: TimeInterval = 0
    private var pingTask: Task<Void, Never>?

    public init(api: APIClient, clock: any MCPTimerClock = SystemMCPTimerClock(), factory: SocketFactory? = nil, onOpen: @escaping @Sendable () async -> Void, onClose: @escaping @Sendable () async -> Void, onMCPFrame: @escaping FrameHandler, diagnostic: @escaping @Sendable (String) -> Void = { _ in }) {
        self.api = api
        self.clock = clock
        self.factory = factory ?? { URLSessionClientWebSocket(request: $0) }
        self.onOpen = onOpen
        self.onClose = onClose
        self.onMCPFrame = onMCPFrame
        self.diagnostic = diagnostic
    }

    public func setNeeded(_ needed: Bool) async {
        self.needed = needed
        if !needed {
            await interrupt()
        } else { startIfNeeded() }
    }

    public func credentialsChanged() async {
        rejected = false
        await interrupt()
        startIfNeeded()
    }

    private func interrupt() async {
        generation = UUID()
        task?.cancel()
        closing = true
        await socket?.close()
    }

    public func send(_ data: Data) async throws {
        guard data.count <= ClientWebSocketLimits.frameBytes else { throw ClientWebSocketError.frameTooLarge }
        guard openConnection != nil, !closing, let socket, !Task.isCancelled else { throw ClientWebSocketError.disconnected }
        try await socket.send(data)
    }

    private func startIfNeeded() {
        guard needed, !rejected, task == nil else { return }
        let generation = generation
        task = Task { await self.run(generation) }
    }

    private func run(_ generation: UUID) async {
        defer {
            task = nil
            if self.generation != generation { startIfNeeded() }
        }
        var delay: TimeInterval = 1
        while needed && !rejected && self.generation == generation && !Task.isCancelled {
            var heartbeat: Task<Void, Never>?
            let connection = UUID()
            do {
                let socket = factory(try api.clientWebSocketRequest())
                self.socket = socket
                try await socket.open()
                let now = await clock.now
                guard self.generation == generation, needed else { throw ClientWebSocketError.disconnected }
                closing = false
                openConnection = connection
                delay = 1
                lastActivity = now
                heartbeat = Task { await self.heartbeat(socket, connection: connection, openedAt: now) }
                await onOpen()
                while !Task.isCancelled && self.generation == generation {
                    let data = try await socket.receive()
                    let now = await clock.now
                    guard openConnection == connection, !closing, self.generation == generation else { break }
                    lastActivity = max(lastActivity, now)
                    guard data.count <= ClientWebSocketLimits.frameBytes else { throw ClientWebSocketError.frameTooLarge }
                    await route(data)
                }
            } catch {
                if self.generation == generation, case ClientWebSocketError.unauthorized = error { rejected = true }
                if case ClientWebSocketError.frameTooLarge = error { closing = true; await socket?.closeTooLarge() }
            }
            heartbeat?.cancel()
            pingTask?.cancel()
            pingTask = nil
            let opened = openConnection != nil
            openConnection = nil
            let old = socket
            socket = nil
            if opened { await onClose() }
            await old?.close()
            guard self.generation == generation, needed, !rejected, !Task.isCancelled else { break }
            do { try await clock.sleep(seconds: delay) } catch { break }
            delay = min(delay * 2, 30)
        }
    }

    private func heartbeat(_ socket: any ClientWebSocket, connection: UUID, openedAt: TimeInterval) async {
        let limit = ClientWebSocketLimits.heartbeatSeconds
        let interval = limit / 2
        var nextPing = openedAt + interval
        while !Task.isCancelled && !closing && openConnection == connection {
            let now = await clock.now
            guard !Task.isCancelled, !closing, openConnection == connection else { return }
            if now >= lastActivity + limit {
                closing = true
                await socket.close()
                return
            }
            if now >= nextPing {
                nextPing = now + interval
                if pingTask == nil {
                    pingTask = Task { [weak self] in
                        let pong = (try? await socket.ping()) != nil
                        await self?.pingFinished(connection, pong: pong)
                    }
                }
            }
            do { try await clock.sleep(seconds: min(nextPing, lastActivity + limit) - now) }
            catch { return }
        }
    }

    private func pingFinished(_ connection: UUID, pong: Bool) async {
        let now = await clock.now
        guard openConnection == connection else { return }
        pingTask = nil
        if pong { lastActivity = max(lastActivity, now) }
    }

    private func route(_ data: Data) async {
        guard let frame = try? JSONDecoder().decode(JSONValue.self, from: data), case let .object(object) = frame,
              case let .string(channel)? = object["channel"] else { return }
        if channel == "mcp" { await onMCPFrame(data); return }
        if channel == "transport", object["type"] == .string("error") { diagnostic("client_ws_transport_error"); return }
        let response: JSONValue = .object([
            "channel": .string("transport"), "type": .string("error"),
            "error": .object(["code": .string("unknown_channel"), "channel": .string(channel)])
        ])
        if let data = try? JSONEncoder().encode(response) { try? await send(data) }
    }
}

private final class URLSessionClientWebSocket: NSObject, ClientWebSocket, URLSessionWebSocketDelegate, @unchecked Sendable {
    private let request: URLRequest
    private let lock = NSLock()
    private var opening: CheckedContinuation<Void, Error>?
    private var session: URLSession?
    private var task: URLSessionWebSocketTask?
    private var closed = false

    init(request: URLRequest) { self.request = request }

    func open() async throws {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            lock.lock()
            guard !closed, self.task == nil else {
                lock.unlock()
                continuation.resume(throwing: ClientWebSocketError.disconnected)
                return
            }
            opening = continuation
            let session = URLSession(configuration: .ephemeral, delegate: self, delegateQueue: nil)
            self.session = session
            let task = session.webSocketTask(with: request)
            task.maximumMessageSize = ClientWebSocketLimits.frameBytes
            self.task = task
            lock.unlock()
            task.resume()
        }
    }

    func receive() async throws -> Data {
        guard let task = activeTask() else { throw ClientWebSocketError.disconnected }
        do {
            switch try await task.receive() {
            case let .string(text): return Data(text.utf8)
            case let .data(data): return data
            @unknown default: throw ClientWebSocketError.disconnected
            }
        } catch let error as URLError where error.code == .dataLengthExceedsMaximum {
            throw ClientWebSocketError.frameTooLarge
        }
    }

    func send(_ data: Data) async throws {
        guard let task = activeTask(), let text = String(data: data, encoding: .utf8) else { throw ClientWebSocketError.disconnected }
        try await task.send(.string(text))
    }

    func ping() async throws {
        guard let task = activeTask() else { throw ClientWebSocketError.disconnected }
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            task.sendPing { error in
                if let error { continuation.resume(throwing: error) } else { continuation.resume() }
            }
        }
    }

    func close() async {
        close(code: .goingAway)
    }

    func closeTooLarge() async {
        close(code: .messageTooBig)
    }

    private func activeTask() -> URLSessionWebSocketTask? {
        lock.lock(); defer { lock.unlock() }
        return task
    }

    private func close(code: URLSessionWebSocketTask.CloseCode) {
        lock.lock()
        closed = true
        let task = self.task
        let session = self.session
        let continuation = opening
        self.task = nil
        self.session = nil
        opening = nil
        lock.unlock()
        task?.cancel(with: code, reason: nil)
        session?.finishTasksAndInvalidate()
        continuation?.resume(throwing: ClientWebSocketError.disconnected)
    }

    func urlSession(_ session: URLSession, webSocketTask: URLSessionWebSocketTask, didOpenWithProtocol protocol: String?) {
        finishOpening(nil)
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        if (task.response as? HTTPURLResponse)?.statusCode == 401 { finishOpening(ClientWebSocketError.unauthorized) }
        else { finishOpening(error ?? ClientWebSocketError.disconnected) }
    }

    private func finishOpening(_ error: Error?) {
        lock.lock()
        let continuation = opening
        opening = nil
        lock.unlock()
        if let error { continuation?.resume(throwing: error) } else { continuation?.resume() }
    }
}
