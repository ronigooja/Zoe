import Foundation
import CryptoKit

public struct VoiceActivityClassifier {
    // Retain only hashes, scoped to one media epoch. On saturation, do not evict
    // old keys and let a replay restart the idle clock.
    private var seen = Set<Data>()
    private static let keyLimit = 16_384
    public init() {}

    public mutating func accept(_ event: [String: Any]) -> Bool {
        guard let activity = Self.content(event) else { return false }
        let contentKey = Self.fingerprint(["content", activity.role, activity.turn ?? "", activity.text])
        let eventKey = activity.eventID.map { Self.fingerprint(["event", $0]) }
        if let eventKey, seen.contains(eventKey) { return false }
        let isNew = !seen.contains(contentKey)
        let keysNeeded = (isNew ? 1 : 0) + (eventKey == nil ? 0 : 1)
        guard seen.count + keysNeeded <= Self.keyLimit else { return false }
        if let eventKey { seen.insert(eventKey) }
        if isNew { seen.insert(contentKey) }
        return isNew
    }

    public static func isActivity(event: [String: Any]) -> Bool { content(event) != nil }

    private typealias Activity = (role: String, turn: String?, eventID: String?, text: String)

    private static func content(_ event: [String: Any]) -> Activity? {
        let type = event["type"] as? String ?? ""
        let payload = (event["payload"] as? [String: Any]) ?? event
        let item = (payload["item"] as? [String: Any]) ?? (event["item"] as? [String: Any]) ?? [:]
        let eventID = firstString("event_id", in: [event, payload]) ?? firstString("id", in: [event, payload])
        let turn = firstString("turn_id", in: [event, payload, item]) ?? firstString("bidi_turn_id", in: [event, payload, item])
        let itemID = firstString("item_id", in: [event, payload]) ?? firstString("id", in: [item])
        let isInput = type == "input_transcript.added" || (type.contains("input_audio") && type.contains("transcription"))
        if isInput || type == "output_transcript.added" || type == "assistant.output" {
            guard let text = text(in: item) ?? text(in: payload) ?? text(in: event) else { return nil }
            return (isInput ? "user" : "assistant", turn ?? itemID, eventID, text)
        }
        guard type == "chat_message_delta",
              let delta = payload["delta"] as? [String: Any],
              let version = delta["v"] as? [String: Any],
              let message = version["message"] as? [String: Any],
              let author = message["author"] as? [String: Any],
              let role = author["role"] as? String, role == "user" || role == "assistant",
              let content = message["content"] as? [String: Any],
              let parts = content["parts"] as? [[String: Any]] else { return nil }
        let text = parts.compactMap { part -> String? in
            if role == "user", part["content_type"] as? String == "audio_transcription", part["direction"] as? String == "out" { return nil }
            return Self.text(in: part)
        }.joined(separator: " ")
        guard !text.isEmpty else { return nil }
        return (role, turn ?? firstString("id", in: [message]), eventID, text)
    }

    private static func text(in fields: [String: Any]) -> String? {
        for key in ["transcript", "text", "content"] {
            if let text = firstString(key, in: [fields]) { return text }
        }
        return nil
    }

    private static func firstString(_ key: String, in objects: [[String: Any]]) -> String? {
        for object in objects {
            if let text = object[key] as? String {
                let clean = text.trimmingCharacters(in: .whitespacesAndNewlines)
                if !clean.isEmpty { return clean }
            }
        }
        return nil
    }

    private static func fingerprint(_ fields: [String]) -> Data {
        let framed = fields.map { "\($0.utf8.count):\($0)" }.joined()
        return Data(SHA256.hash(data: Data(framed.utf8)))
    }
}

public struct VoiceActivityCoalescer {
    private var lastReset: Date?
    public init() {}
    public mutating func shouldReset(now: Date) -> Bool {
        guard lastReset.map({ now.timeIntervalSince($0) >= 1 }) ?? true else { return false }
        lastReset = now
        return true
    }
}
