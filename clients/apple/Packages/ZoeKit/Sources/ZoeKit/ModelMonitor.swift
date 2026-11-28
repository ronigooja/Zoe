import CryptoKit
import Foundation

/// One message of a request snapshot as the source certified it.
public struct ModelMonitorMessage: Equatable, Sendable {
    public let role: String
    public let text: String

    public init(role: String, text: String) {
        self.role = role
        self.text = text
    }

    public static func == (lhs: Self, rhs: Self) -> Bool {
        lhs.role.utf8.elementsEqual(rhs.role.utf8) && lhs.text.utf8.elementsEqual(rhs.text.utf8)
    }
}

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
    /// Conversation stated by the source. Events are grouped only by component and this ID; without an ID nothing is grouped.
    public let conversationID: String?
    /// Affirmative fresh-context marker. `false` only means not known to be fresh, never that the context continued.
    public let conversationStart: Bool
    /// The source could not observe all content. Distinct from `truncated`, which means Zoe shortened the payload.
    public let incomplete: Bool
    /// Request input as the source certified it. Used only to derive `appendedMessages`; the payload stays the displayed original.
    public let requestMessages: [ModelMonitorMessage]?
    /// Messages added since the previous complete snapshot in the same conversation. Set by `ModelMonitorBuffer` only for an exact ordered prefix.
    public internal(set) var appendedMessages: [ModelMonitorMessage]?

    public init(component: String, callID: String, time: Date?, kind: ModelMonitorEventKind, payload: JSONValue?, truncated: Bool,
                conversationID: String? = nil, conversationStart: Bool = false, incomplete: Bool = false,
                requestMessages: [ModelMonitorMessage]? = nil) {
        self.component = component
        self.callID = callID
        self.time = time
        self.kind = kind
        self.payload = payload
        self.truncated = truncated
        self.conversationID = conversationID
        self.conversationStart = conversationStart
        self.incomplete = incomplete
        self.requestMessages = requestMessages
    }

    /// Complete pretty-printed payload. Display code decides what to preview; this value is never clipped.
    public var formattedPayload: String? {
        guard let payload else { return nil }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        guard let data = try? encoder.encode(payload) else { return nil }
        return String(decoding: data, as: UTF8.self)
    }

    /// Only OpenCode context text shows JSON embedded in prose as structure; other components keep the generic display.
    public var rendersEmbeddedJSON: Bool { component == "opencode_context" }
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
    /// Latest complete request snapshot per conversation group. Kept only while the entry that carried it is retained.
    private var baselines: [ConversationKey: ConversationBaseline] = [:]

    private struct ConversationKey: Hashable, Sendable {
        let component: String
        let id: String
    }

    private struct ConversationBaseline: Equatable, Sendable {
        let entryID: Int
        let messages: [ModelMonitorMessage]
    }

    public init(capacity: Int = ModelMonitorBuffer.defaultCapacity) {
        self.capacity = max(1, capacity)
    }

    public mutating func append(_ entry: ModelMonitorEntry) {
        let excess = entries.count + 1 - capacity
        if excess > 0 {
            entries.removeFirst(excess)
            droppedCount += excess
            // History never outlives the entry that carried it, so trimming ends the baseline.
            let retained = Set(entries.map(\.id))
            baselines = baselines.filter { retained.contains($0.value.entryID) }
        }
        let annotated = annotate(entry)
        entries.append(annotated)
    }

    public mutating func clear() {
        entries.removeAll()
        droppedCount = 0
        baselines.removeAll()
    }

    /// Derives `appendedMessages` from the group's baseline. Only an exact ordered prefix of a complete snapshot qualifies; otherwise nothing is shown as delta.
    private mutating func annotate(_ entry: ModelMonitorEntry) -> ModelMonitorEntry {
        guard case let .event(event) = entry.kind else {
            // A gap may hide events in any conversation, so no history survives it.
            baselines.removeAll()
            return entry
        }
        guard let conversation = event.conversationID, !conversation.isEmpty else { return entry }
        // The component is part of the group, so the same ID in another component is never joined.
        let key = ConversationKey(component: event.component, id: conversation)
        if event.incomplete || event.truncated || event.kind == .error {
            baselines[key] = nil
            return entry
        }
        // Replies and tool activity do not change the request input baseline, so interleaved activity keeps the group.
        guard event.kind == .request else { return entry }
        guard let snapshot = event.requestMessages else {
            baselines[key] = nil
            return entry
        }
        var annotated = event
        if !event.conversationStart, let base = baselines[key], snapshot.count >= base.messages.count,
           Array(snapshot.prefix(base.messages.count)) == base.messages {
            annotated.appendedMessages = Array(snapshot.dropFirst(base.messages.count))
        }
        baselines[key] = ConversationBaseline(entryID: entry.id, messages: snapshot)
        return ModelMonitorEntry(id: entry.id, kind: .event(annotated))
    }

    public func visibleEntries(component: String?) -> [ModelMonitorEntry] {
        entries.filter { Self.includes($0, component: component) }
    }

    /// `nil` selects Overview, which omits `ModelMonitorChannel.overviewExcludedComponents`. Gaps are kept in every view.
    public static func includes(_ entry: ModelMonitorEntry, component: String?) -> Bool {
        switch entry.kind {
        case .gap: return true
        case let .event(event):
            if let component { return event.component == component }
            return !ModelMonitorChannel.overviewExcludedComponents.contains(event.component)
        }
    }
}

/// Routes entries to separate bounded buffers. Isolated components (OpenCode context) cannot evict, count against, or relabel the shared
/// Overview and component views. Gaps go to every buffer because a gap may hide events in any view.
public struct ModelMonitorFeed: Equatable, Sendable {
    private var shared: ModelMonitorBuffer
    private var isolated: ModelMonitorBuffer

    public init(capacity: Int = ModelMonitorBuffer.defaultCapacity) {
        shared = ModelMonitorBuffer(capacity: capacity)
        isolated = ModelMonitorBuffer(capacity: capacity)
    }

    public mutating func append(_ entry: ModelMonitorEntry) {
        switch entry.kind {
        case .gap:
            shared.append(entry)
            isolated.append(entry)
        case let .event(event):
            if ModelMonitorChannel.overviewExcludedComponents.contains(event.component) {
                isolated.append(entry)
            } else {
                shared.append(entry)
            }
        }
    }

    public mutating func clear() {
        shared.clear()
        isolated.clear()
    }

    /// `component` is the selected tab: `nil` for Overview, otherwise the component name.
    public func visibleEntries(component: String?) -> [ModelMonitorEntry] {
        buffer(for: component).visibleEntries(component: component)
    }

    /// Trimmed-entry count for the selected view only.
    public func droppedCount(component: String?) -> Int {
        buffer(for: component).droppedCount
    }

    /// Labels come from the selected view's buffer, so another buffer's calls cannot change them.
    public func callLabels(component: String?) -> [String: String] {
        ModelMonitorCallID.shortLabels(for: buffer(for: component).entries.compactMap { entry in
            if case let .event(event) = entry.kind { event.callID } else { nil }
        })
    }

    private func buffer(for component: String?) -> ModelMonitorBuffer {
        if let component, ModelMonitorChannel.overviewExcludedComponents.contains(component) { return isolated }
        return shared
    }
}

/// Auto-follow state for the selected tab. Pure so the rules can be unit-tested without a view.
/// Only a measurement at a stable content height, not caused by an append, is treated as a user scroll.
public struct ModelMonitorFollow: Equatable, Sendable {
    public static let bottomThreshold = 8.0
    public private(set) var isAtBottom = true
    private var contentHeight: Double?
    private var appendPending = false
    /// Set by a programmatic jump: scroll-to-end animates through positions that are not user scrolls, so those are ignored until the end is reached.
    private var awaitingBottom = false

    public init() {}

    /// Starts following from the end: tab switch or empty tab, and first visible entry.
    public mutating func reset() {
        isAtBottom = true
        contentHeight = nil
        appendPending = false
        awaitingBottom = false
    }

    /// Programmatic jump to the end. Stays paused-free until a measurement reaches the bottom.
    public mutating func jump() {
        reset()
        awaitingBottom = true
    }

    /// A visible entry was appended. The next layout change is content growth, not a user scroll.
    public mutating func noteAppend() {
        appendPending = true
    }

    /// `distance` is the gap between the viewport bottom and the content bottom; zero or less means at the end.
    public mutating func measure(contentHeight height: Double, distanceFromBottom distance: Double) {
        let changed = contentHeight != height
        contentHeight = height
        if changed || appendPending {
            appendPending = false
            return
        }
        let atEnd = distance <= Self.bottomThreshold
        if awaitingBottom {
            if atEnd { awaitingBottom = false }
            isAtBottom = true
            return
        }
        isAtBottom = atEnd
    }
}

/// Display labels for call IDs. Labels are unique across the given IDs, so the same call shows the same label in every view.
public enum ModelMonitorCallID {
    public static func shortLabels(for ids: [String], minimum: Int = 8) -> [String: String] {
        let sorted = Array(Set(ids)).sorted()
        var labels: [String: String] = [:]
        for (index, id) in sorted.enumerated() {
            // In sorted order, a prefix shared with any other ID is also shared with an adjacent ID.
            let before = index > 0 ? commonPrefixLength(sorted[index - 1], id) : 0
            let after = index + 1 < sorted.count ? commonPrefixLength(id, sorted[index + 1]) : 0
            let length = max(minimum, max(before, after) + 1)
            labels[id] = length >= id.count ? id : String(id.prefix(length))
        }
        return labels
    }

    private static func commonPrefixLength(_ a: String, _ b: String) -> Int {
        zip(a, b).prefix(while: { $0 == $1 }).count
    }
}

/// Metadata-only client diagnostics for the monitor connection. Repeated reasons coalesce within a fixed interval, so the diagnostic queue stays bounded.
/// Suppressed occurrences are reported as `coalesced=N` on the next summary or at the end of a quiet interval.
public struct ModelMonitorDiagnostics: Sendable {
    public enum Reason: String, CaseIterable, Sendable {
        case connected, disconnected
        case sequenceJump = "sequence_jump"
        case serverGap = "server_gap"
    }

    public static let coalescingInterval: TimeInterval = 10
    private var reportedAt: [Reason: Date] = [:]
    private var coalesced: [Reason: Int] = [:]
    private var latestFields: [Reason: [String]] = [:]

    public init() {}

    /// Returns one summary line to record, or nil when the reason was reported within the coalescing interval.
    public mutating func note(_ reason: Reason, now: Date, correlationHash: String? = nil, expected: Int64? = nil, actual: Int64? = nil) -> String? {
        var fields = ["reason=\(reason.rawValue)"]
        if let correlationHash { fields.append("correlation_hash=\(correlationHash)") }
        if let expected { fields.append("expected=\(expected)") }
        if let actual { fields.append("actual=\(actual)") }
        if let last = reportedAt[reason], now.timeIntervalSince(last) < Self.coalescingInterval {
            coalesced[reason, default: 0] += 1
            latestFields[reason] = fields
            return nil
        }
        let suppressed = coalesced[reason] ?? 0
        coalesced[reason] = nil
        latestFields[reason] = nil
        reportedAt[reason] = now
        if suppressed > 0 { fields.append("coalesced=\(suppressed)") }
        return fields.joined(separator: ";")
    }

    mutating func flush() -> [String] {
        let lines = Reason.allCases.compactMap { reason -> String? in
            guard let count = coalesced[reason], let fields = latestFields[reason] else { return nil }
            return (fields + ["coalesced=\(count)"]).joined(separator: ";")
        }
        coalesced.removeAll()
        latestFields.removeAll()
        return lines
    }

    /// Same format as server logs: the first 8 bytes of SHA-256 over the subscription ID, as 16 lowercase hex characters.
    public static func correlationHash(subscriptionID: String) -> String {
        SHA256.hash(data: Data(subscriptionID.utf8)).prefix(8).map { String(format: "%02x", $0) }.joined()
    }
}

/// Readable display text for an event payload. Display only: copy keeps `ModelMonitorEvent.formattedPayload`.
/// Strings keep their decoded characters, so quotes and newlines show as text and backslashes are not unescaped.
/// A string that is entirely a JSON object or array is shown as structure, up to `maxNestedDepth` levels and `maxNestedBytes` each.
public enum ModelMonitorPayloadText {
    public static let maxNestedDepth = 2
    public static let maxNestedBytes = 65_536
    private static let indent = "  "

    /// `embeddedJSON` also renders complete JSON objects and arrays found inside string leaves. The default is the generic renderer, unchanged.
    public static func render(_ payload: JSONValue, embeddedJSON: Bool = false) -> String {
        let (value, depth) = resolve(payload, depth: 0)
        return lines(value, depth: depth, embeddedJSON: embeddedJSON).joined(separator: "\n")
    }

    /// Display text for one message. Without `embeddedJSON` the text is returned exactly as written.
    public static func renderText(_ text: String, embeddedJSON: Bool) -> String {
        embeddedJSON ? Embedded.expand(text) : text
    }

    /// Decodes a string that holds encoded JSON containers, within the depth limit. Returns the value and the depth reached.
    private static func resolve(_ value: JSONValue, depth: Int) -> (JSONValue, Int) {
        guard case let .string(text) = value, depth < maxNestedDepth, let nested = decodedNested(text) else {
            return (value, depth)
        }
        return resolve(nested, depth: depth + 1)
    }

    /// `value` is already resolved. Non-empty containers always start a block; scalars and empty containers may share a line with their key.
    private static func lines(_ value: JSONValue, depth: Int, embeddedJSON: Bool) -> [String] {
        switch value {
        case let .string(text):
            let shown = embeddedJSON ? Embedded.expand(text) : text
            return shown.components(separatedBy: .newlines)
        case let .object(object):
            if object.isEmpty { return ["{}"] }
            var result: [String] = []
            for (key, child) in object.sorted(by: { $0.key < $1.key }) {
                let (resolved, childDepth) = resolve(child, depth: depth)
                let childLines = lines(resolved, depth: childDepth, embeddedJSON: embeddedJSON)
                if childLines.count == 1 && !isNonEmptyContainer(resolved) {
                    result.append("\(key): \(childLines[0])")
                } else {
                    result.append("\(key):")
                    result += childLines.map { indent + $0 }
                }
            }
            return result
        case let .array(items):
            if items.isEmpty { return ["[]"] }
            var result: [String] = []
            for item in items {
                let (resolved, itemDepth) = resolve(item, depth: depth)
                let itemLines = lines(resolved, depth: itemDepth, embeddedJSON: embeddedJSON)
                result += itemLines.enumerated().map { index, line in (index == 0 ? "- " : indent) + line }
            }
            return result
        case let .integer(number): return [String(number)]
        case let .number(number): return [String(number)]
        case let .bool(flag): return [flag ? "true" : "false"]
        case .null: return ["null"]
        }
    }

    private static func isNonEmptyContainer(_ value: JSONValue) -> Bool {
        switch value {
        case let .object(object): !object.isEmpty
        case let .array(items): !items.isEmpty
        default: false
        }
    }

    /// Only text that is wholly a JSON object or array is decoded; other text stays as written.
    private static func decodedNested(_ text: String) -> JSONValue? {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard trimmed.utf8.count <= maxNestedBytes, trimmed.hasPrefix("{") || trimmed.hasPrefix("[") else { return nil }
        guard let decoded = try? JSONDecoder().decode(JSONValue.self, from: Data(trimmed.utf8)) else { return nil }
        switch decoded {
        case .object, .array: return decoded
        default: return nil
        }
    }

    /// OpenCode context text: finds complete JSON objects and arrays inside prose and renders each as structure.
    /// Conservative: a span counts only if it decodes on its own, objects have members, and arrays hold an object or array.
    /// Pairing is one linear, string-aware pass, so malformed candidates are never rescanned. Each candidate is decoded at most once,
    /// and all work is capped by `maxInputBytes`, `maxCandidateBytes` and `maxDecodeBytes`; anything beyond a cap stays as written.
    private enum Embedded {
        static let maxInputBytes = 1_048_576
        static let maxCandidateBytes = 65_536
        static let maxDecodeBytes = 1_048_576

        static func expand(_ text: String) -> String {
            let bytes = Array(text.utf8)
            guard bytes.count <= maxInputBytes, bytes.contains(where: isOpener) else { return text }
            let closes = pairs(bytes)
            var output: [UInt8] = []
            output.reserveCapacity(bytes.count)
            var decodeBudget = maxDecodeBytes
            var index = 0
            while index < bytes.count {
                let end = closes[index]
                if isOpener(bytes[index]), end >= 0, end - index < maxCandidateBytes, end - index + 1 <= decodeBudget {
                    decodeBudget -= end - index + 1
                    if let value = decode(bytes[index...end]) {
                        let rendered = ModelMonitorPayloadText.lines(value, depth: 0, embeddedJSON: false).joined(separator: "\n")
                        output += Array(rendered.utf8)
                        index = end + 1
                        continue
                    }
                }
                output.append(bytes[index])
                index += 1
            }
            // Cuts happen only at ASCII brackets, so the output remains valid UTF-8.
            return String(decoding: output, as: UTF8.self)
        }

        private static func isOpener(_ byte: UInt8) -> Bool { byte == 0x7B || byte == 0x5B }

        /// For each opener, the index of the bracket that closes it, or -1. Quotes and escapes inside strings are ignored; a mismatched closer is skipped.
        static func pairs(_ bytes: [UInt8]) -> [Int] {
            var closes = [Int](repeating: -1, count: bytes.count)
            var open: [Int] = []
            var inString = false
            var escaped = false
            for (index, byte) in bytes.enumerated() {
                if inString {
                    if escaped {
                        escaped = false
                    } else if byte == 0x5C {
                        escaped = true
                    } else if byte == 0x22 {
                        inString = false
                    }
                    continue
                }
                switch byte {
                case 0x22:
                    inString = true
                case 0x7B, 0x5B:
                    open.append(index)
                case 0x7D, 0x5D:
                    guard let top = open.last, bytes[top] == (byte == 0x7D ? 0x7B : 0x5B) else { continue }
                    open.removeLast()
                    closes[top] = index
                default:
                    break
                }
            }
            return closes
        }

        private static func decode(_ slice: ArraySlice<UInt8>) -> JSONValue? {
            guard let value = try? JSONDecoder().decode(JSONValue.self, from: Data(slice)) else { return nil }
            switch value {
            case let .object(object):
                return object.isEmpty ? nil : value
            case let .array(items):
                let holdsContainer = items.contains { item in
                    if case .object = item { return true }
                    if case .array = item { return true }
                    return false
                }
                return holdsContainer ? value : nil
            default:
                return nil
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
        "worker", "worker_rotation", "memory_write", "memory_ingestion", "recall",
        "opencode_context",
    ]
    /// Components shown only in their own tab: excluded from Overview, its unseen counts, and its follow behavior.
    public static let overviewExcludedComponents: Set<String> = ["opencode_context"]

    private let transport: ClientWebSocketTransport
    /// Receives one metadata-only summary line per emitted diagnostic. Callers route it to the existing diagnostics path.
    private let diagnostic: @Sendable (String) -> Void
    private var diagnostics = ModelMonitorDiagnostics()
    private var diagnosticFlush: Task<Void, Never>?
    private var continuation: AsyncStream<ModelMonitorUpdate>.Continuation?
    private var wanted = false
    private var linkOpen = false
    private var subscriptionID: String?
    private var lastSequence: Int64 = 0
    private var nextEntryID = 0
    private(set) var status: ModelMonitorStatus = .idle
    private var lifecycle: Task<Void, Never>?

    public init(transport: ClientWebSocketTransport, diagnostic: @escaping @Sendable (String) -> Void = { _ in }) {
        self.transport = transport
        self.diagnostic = diagnostic
    }

    /// Registers the channel on the shared socket. Registration delivers the current open or rejected state.
    public static func attach(to transport: ClientWebSocketTransport, diagnostic: @escaping @Sendable (String) -> Void = { _ in }) async -> ModelMonitorChannel {
        let channel = ModelMonitorChannel(transport: transport, diagnostic: diagnostic)
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
        diagnosticFlush?.cancel()
        flushDiagnostics()
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
        if wanted { diagnose(.connected, subscriptionID: nil) }
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
        #if DEBUG
        // Fires after this operation is chained behind the previous one, before it awaits.
        await enqueuedObserverForTesting?()
        #endif
        await task.value
    }

    #if DEBUG
    /// Test-only: called after a lifecycle operation is chained onto the queue, so tests can detect the overlap without polling.
    var enqueuedObserverForTesting: (@Sendable () async -> Void)?
    func setEnqueuedObserverForTesting(_ observer: (@Sendable () async -> Void)?) { enqueuedObserverForTesting = observer }
    #endif

    public func closed() async {
        linkOpen = false
        let previousID = subscriptionID
        clearSubscription()
        guard wanted else { return }
        diagnose(.disconnected, subscriptionID: previousID)
        if previousID != nil { emit(.gap) }
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
            if sequence > lastSequence + 1 {
                diagnose(.sequenceJump, subscriptionID: id, expected: lastSequence + 1, actual: sequence)
                emit(.gap)
            }
            lastSequence = sequence
            if let wire = frame.event, let event = ModelMonitorEvent(wire: wire) { emit(.event(event)) }
            setStatus(.live)
        case "gap":
            diagnose(.serverGap, subscriptionID: id, actual: frame.sequence)
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

    /// Metadata only: reason, correlation hash of the subscription ID, and sequence numbers. Never payload or message text.
    private func diagnose(_ reason: ModelMonitorDiagnostics.Reason, subscriptionID id: String?, expected: Int64? = nil, actual: Int64? = nil) {
        let hash = id.map(ModelMonitorDiagnostics.correlationHash(subscriptionID:))
        if let line = diagnostics.note(reason, now: Date(), correlationHash: hash, expected: expected, actual: actual) {
            diagnostic(line)
        } else if diagnosticFlush == nil {
            diagnosticFlush = Task { [weak self] in
                do { try await Task.sleep(for: .seconds(ModelMonitorDiagnostics.coalescingInterval)) }
                catch { return }
                await self?.flushDiagnostics()
            }
        }
    }

    private func flushDiagnostics() {
        diagnosticFlush = nil
        for line in diagnostics.flush() { diagnostic(line) }
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
        // Only subscribe carries the capability flag; unsubscribe stays the bare frame.
        let includesOpenCode = type == "subscribe" ? true : nil
        return try JSONEncoder().encode(OutboundFrame(channel: Self.channel, type: type, subscriptionID: id, includeOpenCodeContext: includesOpenCode))
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
    /// Optional fields from newer sources. Older frames omit them and decode unchanged.
    let conversationID: String?
    let conversationStart: Bool?
    let incomplete: Bool?
    let requestMessages: LossyMessages?

    enum CodingKeys: String, CodingKey {
        case component, time, type, payload, truncated, incomplete
        case callID = "call_id"
        case conversationID = "conversation_id"
        case conversationStart = "conversation_start"
        case requestMessages = "request_messages"
    }
}

fileprivate struct WireMessage: Decodable {
    let role: String
    let text: String
}

/// A malformed snapshot is dropped, not the event: without a certain snapshot no delta is derived, and the payload still shows.
fileprivate struct LossyMessages: Decodable {
    let values: [WireMessage]?

    init(from decoder: Decoder) throws {
        values = try? decoder.singleValueContainer().decode([WireMessage].self)
    }
}

fileprivate struct WireError: Decodable {
    let code: String
}

fileprivate struct OutboundFrame: Encodable {
    let channel: String
    let type: String
    let subscriptionID: String
    /// Set only on subscribe. Servers that predate OpenCode context omit it, and old clients never request it.
    let includeOpenCodeContext: Bool?

    enum CodingKeys: String, CodingKey {
        case channel, type
        case subscriptionID = "subscription_id"
        case includeOpenCodeContext = "include_opencode_context"
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
        self.conversationID = wire.conversationID
        self.conversationStart = kind == .request && wire.conversationStart == true
        self.incomplete = wire.incomplete ?? false
        self.requestMessages = wire.requestMessages?.values?.map { ModelMonitorMessage(role: $0.role, text: $0.text) }
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
