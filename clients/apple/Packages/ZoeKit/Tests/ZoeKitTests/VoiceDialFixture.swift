import Foundation
@testable import ZoeKit

func voiceDialFixture(_ id: String, generation: Int64, handle: String? = nil) throws -> VoiceDialResponse {
    let data = try JSONSerialization.data(withJSONObject: [
        "voice_session_id": id, "generation": generation,
        "gateway_session_id": handle ?? "gateway-\(id)", "answer_sdp": "v=0"
    ] as [String: Any])
    let decoder = JSONDecoder()
    decoder.keyDecodingStrategy = .convertFromSnakeCase
    return try decoder.decode(VoiceDialResponse.self, from: data)
}
