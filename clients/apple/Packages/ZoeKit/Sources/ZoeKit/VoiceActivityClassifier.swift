import Foundation

public enum VoiceActivityClassifier {
    public static func isActivity(type: String) -> Bool {
        // In a real 17-minute silent call, session.output_audio.delta remained a
        // continuous stream while idle. turn.delta idle behavior is unverified.
        // Neither those events nor continuous microphone frames prove speech.
        switch type {
        case "output_transcript.added", "assistant.output": return true
        default: return false
        }
    }

    public static func isActivity(event: [String: Any]) -> Bool {
        let type = event["type"] as? String ?? ""
        if isActivity(type: type) { return true }
        let payload = (event["payload"] as? [String: Any]) ?? event
        if type == "input_transcript.added" {
            let item = payload["item"] as? [String: Any]
            let text = (item?["text"] as? String) ?? (payload["text"] as? String)
            return isNonblank(text)
        }
        if type.contains("input_audio") && type.contains("transcription") {
            let text = (event["transcript"] as? String) ?? (event["text"] as? String) ?? (payload["transcript"] as? String) ?? (payload["text"] as? String)
            return isNonblank(text)
        }
        guard type == "chat_message_delta",
              let delta = payload["delta"] as? [String: Any],
              let version = delta["v"] as? [String: Any],
              let message = version["message"] as? [String: Any],
              let author = message["author"] as? [String: Any],
              let role = author["role"] as? String, role == "user" || role == "assistant",
              let content = message["content"] as? [String: Any],
              let parts = content["parts"] as? [[String: Any]] else { return false }
        return parts.contains { part in
            if role == "user", part["content_type"] as? String == "audio_transcription", part["direction"] as? String == "out" { return false }
            let text = (part["transcript"] as? String) ?? (part["text"] as? String) ?? (part["content"] as? String)
            return isNonblank(text)
        }
    }

    private static func isNonblank(_ text: String?) -> Bool {
        text?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false
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
