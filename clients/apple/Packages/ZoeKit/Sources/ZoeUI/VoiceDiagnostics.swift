import Foundation
import ZoeKit

enum VoiceEndReason: String, CaseIterable {
    case voiceEndEvent = "voice_end_event"
    case userToggleOff = "user_toggle_off"
    case mediaLossConnection = "media_loss_connection"
    case mediaLossAudio = "media_loss_audio"
    case mediaLossDataChannel = "media_loss_data_channel"
    case appTerminate = "app_terminate"
    case connectionCleared = "connection_cleared"
    case setupFailed = "setup_failed"
    case other

    var isError: Bool {
        switch self {
        case .mediaLossConnection, .mediaLossAudio, .mediaLossDataChannel, .setupFailed: true
        default: false
        }
    }

    func summary(duration: TimeInterval) -> String {
        let seconds = duration.isFinite ? Int(min(max(0, duration), Double(Int32.max))) : 0
        return "\(rawValue);dur=\(seconds)"
    }
}

extension VoiceMediaLoss {
    var diagnosticReason: VoiceEndReason {
        switch self {
        case .connection: .mediaLossConnection
        case .audio: .mediaLossAudio
        case .dataChannel: .mediaLossDataChannel
        }
    }
}
