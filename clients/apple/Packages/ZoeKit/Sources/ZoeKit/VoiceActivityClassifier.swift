import Foundation

public enum VoiceActivityClassifier {
    public static func isActivity(type: String) -> Bool {
        // In a real 17-minute silent call, session.output_audio.delta remained a
        // continuous stream while idle. turn.delta idle behavior is unverified.
        // Neither those events nor continuous microphone frames prove speech.
        // The caller must still validate user_audio_turn.final and legacy payloads.
        switch type {
        case "output_transcript.added", "user_audio_turn.final", "assistant.output": return true
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
