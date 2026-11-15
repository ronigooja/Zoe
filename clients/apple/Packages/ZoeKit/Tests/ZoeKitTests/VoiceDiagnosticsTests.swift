import XCTest
@testable import ZoeKit
@testable import ZoeUI

final class VoiceDiagnosticsTests: XCTestCase {
    func testReasonCodesAndBoundedSummaries() {
        XCTAssertEqual(Set(VoiceEndReason.allCases.map(\.rawValue)), Set([
            "silence_timeout", "voice_end_event", "user_toggle_off", "media_loss_connection",
            "media_loss_audio", "media_loss_data_channel", "lease_renewal_failed", "heartbeat_failure",
            "app_terminate", "connection_cleared", "setup_failed", "other"
        ]))
        for reason in VoiceEndReason.allCases {
            XCTAssertEqual(reason.summary(duration: 39.8), "\(reason.rawValue);dur=39")
            for duration in [-1.0, 39, .infinity, .greatestFiniteMagnitude] {
                let summary = reason.summary(duration: duration)
                XCTAssertLessThanOrEqual(summary.count, 64)
                let event = DiagnosticEvent(clientRequestID: "r", phase: "voice_end", requestPath: "client", errorType: reason.isError ? "voice_lifecycle" : "", errorSummary: summary, installationID: "i", level: reason.isError ? .error : .info)
                XCTAssertEqual(event.errorSummary, summary)
                XCTAssertEqual(event.level, reason.isError ? "error" : "info")
                for sensitive in ["bearer", "transcript", "token", "https://", "secret"] {
                    XCTAssertFalse(summary.contains(sensitive))
                }
            }
        }
    }

    func testMediaLossReasonMappingDoesNotIncludeUserText() {
        XCTAssertEqual(VoiceMediaLoss.connection(.failed).diagnosticReason, .mediaLossConnection)
        XCTAssertEqual(VoiceMediaLoss.audio(.interrupted).diagnosticReason, .mediaLossAudio)
        XCTAssertEqual(VoiceMediaLoss.dataChannel(.closed).diagnosticReason, .mediaLossDataChannel)
        XCTAssertEqual(VoiceMediaLoss.leaseRenewalFailed("private transcript token").diagnosticReason, .leaseRenewalFailed)
    }
}
