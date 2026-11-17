import XCTest
@testable import ZoeKit

final class VoiceConfigTests: XCTestCase {
    func testDecodesServerWebRTCKey() throws {
        let data = Data("""
        {
          "version": "v1",
          "defaults": {"voice": "cove", "voice_mode": "wingman", "language_code": "auto"},
          "voices": [],
          "voice_modes": [],
          "languages": [],
          "webrtc": {
            "data_channel": {"label": "zoe", "negotiated": true, "id": 0},
            "ice_servers": [],
            "receive_audio": true,
            "receive_video": false
          }
        }
        """.utf8)

        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        let config = try decoder.decode(VoiceConfig.self, from: data)

        XCTAssertEqual(config.webRTC.dataChannel.label, "zoe")
        XCTAssertTrue(config.webRTC.receiveAudio)
        XCTAssertFalse(config.webRTC.receiveVideo)
        XCTAssertEqual(config.resolvedVoice(preferred: nil), "cove")
        XCTAssertEqual(config.resolvedVoice(preferred: "removed"), "cove")
        var available = config
        available.voices = [VoiceChoice(id: "selected", name: "Selected")]
        XCTAssertEqual(available.resolvedVoice(preferred: "selected"), "selected")
        available.voices = []
        XCTAssertEqual(available.resolvedVoice(preferred: "selected"), "cove")
    }
}
