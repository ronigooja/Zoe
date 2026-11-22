import XCTest
@testable import ZoeKit
@testable import ZoeUI

@MainActor
final class VoicePreferenceTests: XCTestCase {
    func testPreferencePersistenceScopeAndRemovedVoiceFallback() throws {
        let name = "VoicePreferenceTests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: name)!
        defer { defaults.removePersistentDomain(forName: name) }
        defaults.set("https://voice.example", forKey: "zoe.instance.serverAddress")
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        var config = try decoder.decode(VoiceConfig.self, from: Data("""
        {"version":"v1","gateway_dial_origin":"https://gateway.example","defaults":{"voice":"default","voice_mode":"","language_code":"auto"},
         "voices":[{"id":"chosen","name":"Chosen"}],"voice_modes":[],"languages":[],
         "webrtc":{"data_channel":{"label":"zoe","negotiated":true,"id":0},
         "ice_servers":[],"receive_audio":true,"receive_video":false}}
        """.utf8))
        let model = ZoeAppModel(defaults: defaults)
        model.installVoiceConfigForTesting(config)
        model.setPreferredVoice("chosen")
        XCTAssertEqual(model.effectiveVoice, "chosen")
        let restored = ZoeAppModel(defaults: defaults)
        restored.installVoiceConfigForTesting(config)
        XCTAssertEqual(restored.preferredVoice, "chosen")
        XCTAssertEqual(restored.effectiveVoice, "chosen")
        config.voices = []
        restored.installVoiceConfigForTesting(config)
        XCTAssertEqual(restored.effectiveVoice, "default")
        XCTAssertEqual(restored.preferredVoice, "chosen")
        restored.setPreferredVoice("invalid")
        XCTAssertEqual(restored.preferredVoice, "chosen")
        defaults.set("https://other.example", forKey: "zoe.instance.serverAddress")
        XCTAssertNil(ZoeAppModel(defaults: defaults).preferredVoice)
        defaults.set("https://voice.example/", forKey: "zoe.instance.serverAddress")
        let original = ZoeAppModel(defaults: defaults)
        XCTAssertEqual(original.preferredVoice, "chosen")
        original.setPreferredVoice(nil)
        XCTAssertNil(ZoeAppModel(defaults: defaults).preferredVoice)
    }
}
