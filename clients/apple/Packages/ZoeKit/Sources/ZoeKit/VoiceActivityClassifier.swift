import Foundation

public enum VoiceActivityClassifier {
    public static func isActivity(type: String) -> Bool {
        // Real 98-second call census: assistant output is activity; continuous
        // session.input_audio.append microphone frames are not evidence of speech.
        // The caller must still validate user_audio_turn.final and legacy payloads.
        switch type {
        case "session.output_audio.delta", "output_transcript.added", "turn.delta",
             "user_audio_turn.final", "assistant.output": return true
        default: return false
        }
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
