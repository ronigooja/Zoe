import XCTest
@testable import ZoeKit
@testable import ZoeUI

private final class SettingsSilenceClock: SilenceClock {
    var date = Date(timeIntervalSince1970: 0)
    func now() -> Date { date }
    func advance(_ seconds: Double) { date = date.addingTimeInterval(seconds) }
}

@MainActor
final class SilenceSettingsTests: XCTestCase {
    func testDisabledNeverTimesOutAndActivityRestartsConfiguredWindow() {
        let clock = SettingsSilenceClock()
        let controller = SilenceController(timeoutSeconds: 25, isEnabled: false, clock: clock)
        controller.noteVoiceActivity()
        clock.advance(10_000)
        XCTAssertEqual(controller.tick(), .none)
        controller.isEnabled = true
        controller.noteVoiceActivity()
        clock.advance(24)
        XCTAssertEqual(controller.tick(), .none)
        controller.noteVoiceActivity()
        clock.advance(24)
        XCTAssertEqual(controller.tick(), .none)
        clock.advance(1)
        XCTAssertEqual(controller.tick(), .timedOut)
        XCTAssertEqual(controller.tick(), .none)
        controller.reset()
        clock.advance(100)
        XCTAssertEqual(controller.tick(), .none)
        controller.noteVoiceActivity()
        clock.advance(25)
        XCTAssertEqual(controller.tick(), .timedOut)
    }

    func testDefaultsPersistenceClampingAndImmediateApplication() {
        let name = "SilenceSettingsTests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: name)!
        defer { defaults.removePersistentDomain(forName: name) }
        let model = ZoeAppModel(defaults: defaults)
        XCTAssertTrue(model.silenceHangupEnabled)
        XCTAssertEqual(model.silenceTimeoutSeconds, 60)
        model.setSilenceHangupEnabled(false)
        model.setSilenceTimeoutSeconds(1)
        XCTAssertFalse(model.silence.isEnabled)
        XCTAssertEqual(model.silence.timeoutSeconds, 10)
        model.setSilenceTimeoutSeconds(9999)
        XCTAssertEqual(model.silence.timeoutSeconds, 3600)
        model.setSilenceTimeoutSeconds(.nan)
        model.setSilenceTimeoutSeconds(.infinity)
        XCTAssertEqual(model.silenceTimeoutSeconds, 3600)
        let restored = ZoeAppModel(defaults: defaults)
        XCTAssertFalse(restored.silenceHangupEnabled)
        XCTAssertEqual(restored.silenceTimeoutSeconds, 3600)
        defaults.set(2, forKey: "zoe.voice.silenceTimeoutSeconds")
        XCTAssertEqual(ZoeAppModel(defaults: defaults).silenceTimeoutSeconds, 10)
        defaults.set(4000, forKey: "zoe.voice.silenceTimeoutSeconds")
        XCTAssertEqual(ZoeAppModel(defaults: defaults).silenceTimeoutSeconds, 3600)
    }

    func testConnectStartsFreshWindowAndInboundTranscriptResetsIt() async throws {
        let clock = SettingsSilenceClock()
        let name = "SilenceSettingsTests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: name)!
        defer { defaults.removePersistentDomain(forName: name) }
        let model = ZoeAppModel(defaults: defaults, silenceClock: clock)
        clock.advance(100)
        XCTAssertEqual(model.silence.tick(), .none)
        let api = try APIClient(serverURL: URL(string: "https://silence.example")!)
        let session = VoiceSessionResponse(voiceSessionId: "silence-test", generation: 1, state: "active", answerSdp: "")
        let epoch = model.installVoiceSessionForTesting(session, api: api)
        var traces: [(String, String)] = []
        model.voiceDiagnosticObserverForTesting = { traces.append(($0, $1)) }
        clock.advance(59)
        XCTAssertEqual(model.silence.tick(), .none)
        await model.receiveVoiceDataForTesting(Data(#"{"type":"input_audio_transcription.completed","transcript":"hello"}"#.utf8), mediaEpoch: epoch)
        XCTAssertTrue(traces.contains { $0.0 == "silence_reset" && $0.1.contains("last=input_audio_transcription.completed") })
        await model.receiveVoiceDataForTesting(Data(#"{"type":"unrecognized","payload":"sk-secret-payload"}"#.utf8), mediaEpoch: epoch)
        XCTAssertTrue(model.voiceCensusSummaryForTesting.contains("unrecognized:1:ignored"))
        XCTAssertFalse(model.voiceCensusSummaryForTesting.contains("sk-secret-payload"))
        clock.advance(59)
        XCTAssertEqual(model.silence.tick(), .none)
        let chat = #"{"type":"chat_message_delta","payload":{"delta":{"v":{"message":{"author":{"role":"user"},"content":{"parts":[{"transcript":"hello again"}]}}}}}}"#
        let wrapped = try JSONSerialization.data(withJSONObject: ["type": "data_message", "data": chat])
        await model.receiveVoiceDataForTesting(wrapped, mediaEpoch: epoch)
        clock.advance(59)
        XCTAssertEqual(model.silence.tick(), .none)
        clock.advance(1)
        XCTAssertEqual(model.silence.tick(), .timedOut)
        _ = model.installVoiceSessionForTesting(session, api: api)
        XCTAssertEqual(model.silence.tick(), .none)
        clock.advance(60)
        XCTAssertEqual(model.silence.tick(), .timedOut)
    }

    func testActivityEventShapes() throws {
        for role in ["user", "assistant"] {
            let event: [String: Any] = ["type": "chat_message_delta", "payload": ["delta": ["v": ["message": ["author": ["role": role], "content": ["parts": [["text": "hello"]]]]]]]]
            XCTAssertTrue(ZoeAppModel.isVoiceActivity(event))
        }
        for json in [
            #"{"type":"user_audio_turn.final","turn_id":"turn-1","revision":1,"transcript":"hello"}"#,
            #"{"type":"assistant.output","turn_id":"turn-1","revision":1,"transcript":"hello"}"#,
            #"{"type":"input_audio_transcription.completed","transcript":"hello"}"#
        ] {
            let event = try JSONSerialization.jsonObject(with: Data(json.utf8)) as! [String: Any]
            XCTAssertTrue(ZoeAppModel.isVoiceActivity(event))
        }
        XCTAssertFalse(ZoeAppModel.isVoiceActivity(["type": "voice.end"]))
        XCTAssertFalse(ZoeAppModel.isVoiceActivity(["type": "input_audio_transcription.completed", "transcript": " "]))
        XCTAssertFalse(ZoeAppModel.isVoiceActivity(["type": "chat_message_delta", "transcript": "ignored"]))
    }
}
