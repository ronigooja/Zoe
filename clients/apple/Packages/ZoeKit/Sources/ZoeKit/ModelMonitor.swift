import Foundation

public enum ModelMonitorStatus: Equatable, Sendable {
    case idle, connecting, subscribing, live, reconnecting, ended, unauthorized
}

public enum ModelMonitorEventKind: String, Equatable, Sendable {
    case request, response, error
    case toolCall = "tool_call"
    case toolResult = "tool_result"
}

public struct ModelMonitorEvent: Equatable, Sendable {
    public let component: String
    public let callID: String
    public let time: Date?
    public let kind: ModelMonitorEventKind
    public let payload: JSONValue?
    public let truncated: Bool

    /// Pretty-printed payload clipped to `limit` characters for display.
    public func payloadText(limit: Int) -> String? {
        guard let payload else { return nil }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        guard let data = try? encoder.encode(payload) else { return nil }
        let text = String(decoding: data, as: UTF8.self)
        return text.count > limit ? String(text.prefix(limit)) + "…" : text
    }
}

public struct ModelMonitorEntry: Identifiable, Equatable, Sendable {
    public enum Kind: Equatable, Sendable {
        case event(ModelMonitorEvent)
        /// Events may be missing before this point: source loss, writer contention, or reconnect.
        case gap
    }
    public let id: Int
    public let kind: Kind
}

public enum ModelMonitorUpdate: Equatable, Sendable {
    case status(ModelMonitorStatus)
    case entry(ModelMonitorEntry)
    /// A non-terminal server error; the subscription continues.
    case serverError
    case reset
}

/// Bounded, component-filterable view buffer. Gaps are kept regardless of the filter.
public struct ModelMonitorBuffer: Equatable, Sendable {
    public static let defaultCapacity = 500
    public let capacity: Int
    public private(set) var entries: [ModelMonitorEntry] = []
    /// Number of oldest entries removed to keep the buffer within `capacity`.
    public private(set) var droppedCount = 0

    public init(capacity: Int = ModelMonitorBuffer.defaultCapacity) {
        self.capacity = max(1, capacity)
    }

    public mutating func append(_ entry: ModelMonitorEntry) {
        entries.append(entry)
        let excess = entries.count - capacity
        if excess > 0 {
            entries.removeFirst(excess)
            droppedCount += excess
        }
    }

    public mutating func clear() {
        entries.removeAll()
        droppedCount = 0
    }

    public func visible(components: Set<String>) -> [ModelMonitorEntry] {
        entries.filter { entry in
            switch entry.kind {
            case .gap: true
            case let .event(event): components.contains(event.component)
            }
        }
    }
}

/// Live-only model conversation monitor on the shared client socket.
/// Its demand exists only while subscribed, and it never affects other channels' demand.
public actor ModelMonitorChannel: ClientWebSocketChannel {
    public static let channel = "model_monitor"
    public static let components = [
        "register", "compact", "steward", "ui_controller", "task_summary",
        "worker", "worker_rotation", "memory_write", "memory_ingestion",
    ]

    private let transport: ClientWebSocketTransport
    private var continuation: AsyncStream<ModelMonitorUpdate>.Continuation?
    private var wanted = false
    private var linkOpen = false
    private var subscriptionID: String?
    private var lastSequence: Int64 = 0
    private var nextEntryID = 0
    private(set) var status: ModelMonitorStatus = .idle
    private var lifecycle: Task<Void, Never>?

    public init(transport: ClientWebSocketTransport) {
        self.transport = transport
    }

    /// Registers the channel on the shared socket. Registration delivers the current open or rejected state.
    public static func attach(to transport: ClientWebSocketTransport) async -> ModelMonitorChannel {
        let channel = ModelMonitorChannel(transport: transport)
        await transport.register(channel: ModelMonitorChannel.channel, handler: channel)
        return channel
    }

    /// Begins a monitoring session and returns its update stream. A previous stream is finished first, so stale updates never reach the new consumer.
    /// Stream creation and start are one serialized step, so a queued stop cannot finish the new stream.
    public func begin() async -> AsyncStream<ModelMonitorUpdate> {
        let stream = AsyncStream.makeStream(of: ModelMonitorUpdate.self)
        await serialized { await self.performBegin(stream.continuation) }
        return stream.stream
    }

    private func performBegin(_ next: AsyncStream<ModelMonitorUpdate>.Continuation) async {
        continuation?.finish()
        continuation = next
        await performStart()
    }

    /// Unsubscribes, drops this channel's demand, and finishes the update stream.
    public func stop() async {
        await serialized { await self.performStop() }
    }

    /// Explicit user retry after a terminal `subscription_ended`. Nothing resubscribes automatically after that.
    public func retry() async {
        await serialized { await self.performRetry() }
    }

    public func opened() async {
        await serialized { await self.performOpened() }
    }

    private func performStart() async {
        wanted = true
        setStatus(await transport.isRejected ? .unauthorized : .connecting)
        // Demand is held even when rejected, so a credential change can reconnect the socket for this channel.
        await transport.setNeeded(true, demand: Self.channel)
        #if DEBUG
        await startGateForTesting?()
        #endif
        await subscribe()
    }

    #if DEBUG
    /// Test-only barrier between demand and subscribe, used to force the overlapping-stop interleaving deterministically.
    var startGateForTesting: (@Sendable () async -> Void)?
    func setStartGateForTesting(_ gate: (@Sendable () async -> Void)?) { startGateForTesting = gate }
    #endif

    private func performStop() async {
        wanted = false
        let id = subscriptionID
        clearSubscription()
        setStatus(.idle)
        continuation?.yield(.reset)
        continuation?.finish()
        continuation = nil
        if let id, let data = try? frame("unsubscribe", id) { try? await transport.send(data) }
        await transport.setNeeded(false, demand: Self.channel)
    }

    private func performRetry() async {
        guard wanted, status == .ended else { return }
        setStatus(.connecting)
        await transport.setNeeded(true, demand: Self.channel)
        await subscribe()
    }

    private func performOpened() async {
        linkOpen = true
        // Credentials changed after a 401 stop; the transport reconnected, so the monitor may resubscribe.
        if status == .unauthorized, wanted { setStatus(.connecting) }
        await subscribe()
    }

    /// Runs each operation after the previous one finishes, so a stop cannot interleave with a start.
    private func serialized(_ operation: @escaping @Sendable () async -> Void) async {
        // Operations are chained, so each one sees the state left by the previous operation.
        let previous = lifecycle
        let task = Task {
            await previous?.value
            await operation()
        }
        lifecycle = task
        await task.value
    }

    public func closed() async {
        linkOpen = false
        let hadSubscription = subscriptionID != nil
        clearSubscription()
        guard wanted else { return }
        if hadSubscription { emit(.gap) }
        if status != .ended { setStatus(.reconnecting) }
    }

    public func rejected() async {
        linkOpen = false
        clearSubscription()
        guard wanted else { return }
        setStatus(.unauthorized)
    }

    public func received(_ data: Data) async {
        guard let frame = try? JSONDecoder().decode(WireFrame.self, from: data), frame.channel == Self.channel,
              let id = subscriptionID, frame.subscriptionID == id else { return }
        switch frame.type {
        case "subscribed":
            setStatus(.live)
        case "event":
            guard let sequence = frame.sequence, sequence > lastSequence else { return }
            if sequence > lastSequence + 1 { emit(.gap) }
            lastSequence = sequence
            if let wire = frame.event, let event = ModelMonitorEvent(wire: wire) { emit(.event(event)) }
            setStatus(.live)
        case "gap":
            if let sequence = frame.sequence, sequence > lastSequence { lastSequence = sequence }
            emit(.gap)
        case "error":
            if frame.error?.code == "subscription_ended" {
                await endSubscription()
            } else {
                continuation?.yield(.serverError)
            }
        default:
            break
        }
    }

    private func subscribe() async {
        guard wanted, linkOpen, subscriptionID == nil, status != .ended, status != .unauthorized else { return }
        let id = UUID().uuidString
        subscriptionID = id
        lastSequence = 0
        setStatus(.subscribing)
        do {
            try await transport.send(try frame("subscribe", id))
        } catch {
            guard subscriptionID == id else { return }
            clearSubscription()
            setStatus(.connecting)
        }
    }

    private func endSubscription() async {
        clearSubscription()
        setStatus(.ended)
        // An ended subscription must not keep the shared socket open.
        await transport.setNeeded(false, demand: Self.channel)
    }

    private func clearSubscription() {
        subscriptionID = nil
        lastSequence = 0
    }

    private func emit(_ kind: ModelMonitorEntry.Kind) {
        nextEntryID += 1
        continuation?.yield(.entry(ModelMonitorEntry(id: nextEntryID, kind: kind)))
    }

    private func setStatus(_ next: ModelMonitorStatus) {
        guard status != next else { return }
        status = next
        continuation?.yield(.status(next))
    }

    private func frame(_ type: String, _ id: String) throws -> Data {
        try JSONEncoder().encode(OutboundFrame(channel: Self.channel, type: type, subscriptionID: id))
    }
}

fileprivate struct WireFrame: Decodable {
    let channel: String
    let type: String
    let subscriptionID: String?
    let sequence: Int64?
    let event: WireEvent?
    let error: WireError?

    enum CodingKeys: String, CodingKey {
        case channel, type, sequence, event, error
        case subscriptionID = "subscription_id"
    }
}

fileprivate struct WireEvent: Decodable {
    let component: String
    let callID: String
    let time: String
    let type: String
    let payload: JSONValue?
    let truncated: Bool?

    enum CodingKeys: String, CodingKey {
        case component, time, type, payload, truncated
        case callID = "call_id"
    }
}

fileprivate struct WireError: Decodable {
    let code: String
}

fileprivate struct OutboundFrame: Encodable {
    let channel: String
    let type: String
    let subscriptionID: String

    enum CodingKeys: String, CodingKey {
        case channel, type
        case subscriptionID = "subscription_id"
    }
}

fileprivate extension ModelMonitorEvent {
    init?(wire: WireEvent) {
        guard let kind = ModelMonitorEventKind(rawValue: wire.type) else { return nil }
        self.component = wire.component
        self.callID = wire.callID
        self.time = Self.parseTime(wire.time)
        self.kind = kind
        self.payload = wire.payload
        self.truncated = wire.truncated ?? false
    }

    static func parseTime(_ text: String) -> Date? {
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = fractional.date(from: text) { return date }
        let plain = ISO8601DateFormatter()
        plain.formatOptions = [.withInternetDateTime]
        return plain.date(from: text)
    }
}
