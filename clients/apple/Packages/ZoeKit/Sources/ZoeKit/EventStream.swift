import Foundation

public enum ServerPush: Sendable {
    case event(EventEnvelope)
    case snapshotRequired(reason: String)
}

/// Splits SSE bytes without dropping empty lines. An unterminated tail is not emitted.
struct SSELineSplitter {
    private var lineBytes: [UInt8] = []
    private var previousWasCarriageReturn = false

    mutating func consume(byte: UInt8) -> String? {
        if previousWasCarriageReturn {
            previousWasCarriageReturn = false
            if byte == 0x0A { return nil }
        }
        if byte == 0x0D || byte == 0x0A {
            previousWasCarriageReturn = byte == 0x0D
            let line = String(decoding: lineBytes, as: UTF8.self)
            lineBytes.removeAll(keepingCapacity: true)
            return line
        }
        lineBytes.append(byte)
        return nil
    }
}

/// Small, testable parser for the SSE subset used by Zoe (`id`, `event`, and `data`).
public struct SSEParser {
    private var id: String?
    private var name: String?
    private var dataLines: [String] = []

    public init() {}

    public mutating func consume(line: String, decoder: JSONDecoder = ZoeJSON.decoder) throws -> ServerPush? {
        if line.isEmpty { return try dispatch(decoder: decoder) }
        if line.hasPrefix(":") { return nil }
        let parts = line.split(separator: ":", maxSplits: 1, omittingEmptySubsequences: false)
        let field = String(parts[0])
        var value = parts.count > 1 ? String(parts[1]) : ""
        if value.first == " " { value.removeFirst() }
        switch field {
        case "id":
            if !value.contains("\0") { id = value }
        case "event": name = value
        case "data": dataLines.append(value)
        default: break
        }
        return nil
    }

    public mutating func finish(decoder: JSONDecoder = ZoeJSON.decoder) throws -> ServerPush? {
        // An incomplete SSE record at EOF has not been dispatched by the server.
        id = nil
        name = nil
        dataLines.removeAll(keepingCapacity: true)
        return nil
    }

    private mutating func dispatch(decoder: JSONDecoder) throws -> ServerPush? {
        defer { id = nil; name = nil; dataLines.removeAll(keepingCapacity: true) }
        guard !dataLines.isEmpty else { return nil }
        let type = name ?? "message"
        let data = Data(dataLines.joined(separator: "\n").utf8)
        if type == "snapshot_required" {
            let failure = (try? decoder.decode(SnapshotRequiredBody.self, from: data))
            return .snapshotRequired(reason: failure?.reason ?? "The event cursor is no longer available.")
        }
        let envelope = try decoder.decode(EventEnvelope.self, from: data)
        if let id, !id.isEmpty, envelope.cursor != id {
            throw ZoeError.invalidResponse
        }
        guard envelope.schemaVersion == 1 else {
            throw ZoeError.http(status: 200, code: "unsupported_event_schema", message: "The server sent an unsupported event format (schema \(envelope.schemaVersion)).", details: nil)
        }
        return .event(envelope)
    }
}

private struct SnapshotRequiredBody: Decodable {
    var reason: String
}

public enum ZoeJSON {
    public static var decoder: JSONDecoder {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        return decoder
    }

    public static var encoder: JSONEncoder {
        let encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        return encoder
    }
}

public struct EventStreamClient: Sendable {
    private let api: APIClient

    public init(api: APIClient) { self.api = api }

    /// Streams until the server closes the connection or the caller cancels.
    @discardableResult
    public func run(after cursor: String?, onConnected: @Sendable () async -> Void = {}, onPush: @Sendable (ServerPush) async throws -> Void) async throws -> String? {
        let bytes = try await api.events(after: cursor)
        await onConnected()
        var parser = SSEParser()
        var splitter = SSELineSplitter()
        var latestCursor = cursor
        for try await byte in bytes {
            guard let line = splitter.consume(byte: byte) else { continue }
            if let push = try parser.consume(line: line) {
                switch push {
                case let .event(envelope): latestCursor = envelope.cursor
                case .snapshotRequired: break
                }
                try await onPush(push)
                if case .snapshotRequired = push { return nil }
            }
        }
        if let push = try parser.finish() {
            if case let .event(envelope) = push { latestCursor = envelope.cursor }
            try await onPush(push)
        }
        return latestCursor
    }
}
