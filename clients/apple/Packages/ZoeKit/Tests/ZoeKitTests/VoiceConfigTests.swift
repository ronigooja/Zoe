import XCTest
@testable import ZoeKit

final class VoiceConfigTests: XCTestCase {
    func testDecodesServerWebRTCKey() throws {
        let data = Data("""
        {
          "version": "v1",
          "gateway_dial_origin": "https://gateway.example:8443",
          "defaults": {"voice": "cove", "voice_mode": "wingman", "language_code": "auto"},
          "voices": [],
          "voice_modes": [],
          "languages": [],
          "webrtc": {
            "data_channel": {"label": "zoe", "negotiated": true, "id": 0},
            "ice_servers": [{"urls":["stun:ice.example:3478"]}],
            "receive_audio": true,
            "receive_video": false
          }
        }
        """.utf8)

        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        let config = try decoder.decode(VoiceConfig.self, from: data)

        XCTAssertEqual(config.gatewayOrigin?.absoluteString, "https://gateway.example:8443")
        XCTAssertEqual(config.webRTC.iceServers.first?.urls, ["stun:ice.example:3478"])
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

    func testRejectsNonOrigins() {
        for origin in ["", "//gateway.example", "http://gateway.example", "https:///gateway.example",
                       "https://gateway.example/", "https://gateway.example/a", "https://a@b.example",
                       "https://a:b@gateway.example", "https://gateway.example?", "https://gateway.example#",
                       "https://gateway.example:0", "https://gateway.example:65536", "https://gateway.example:",
                       "https://gateway.example%2Fother.example", "https://gateway.example\\other.example"] {
            XCTAssertNil(VoiceConfig.validateGatewayOrigin(origin), origin)
        }
        XCTAssertEqual(VoiceConfig.validateGatewayOrigin("HTTPS://GATEWAY.EXAMPLE:443")?.absoluteString, "https://gateway.example")
    }
}
