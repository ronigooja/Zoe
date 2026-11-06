package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

// This test deliberately uses the old API too, so it demonstrates the missing
// call-start delivery on the pre-change baseline rather than a compilation error.
func TestInstructionDefaultBeforeSDPAndCreateReplay(t *testing.T) {
	const expected = "Standing rule for you, do not read this aloud or acknowledge it: 你是语音入口。问候、感谢、闲聊、观点这类不需要查资料的话,你自己直接回答。其余一律委托,不要自己回答:需要查资料、联网、操作文件或设备的请求;问之前交办的事做到哪了、好了没有;问以前的通话里说过什么、做过什么;让你记住或回忆我的个人信息。委托之后等结果再说,不要自己编结果,也不要说没有记录。"
	var pushes, creates int
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/voice/sessions":
			creates++
			_ = json.NewEncoder(w).Encode(map[string]any{"voice_session_id": "gateway-instruction", "voice_session_generation": 3, "answer_sdp": "answer"})
		case "/v1/voice/sessions/gateway-instruction/context":
			pushes++
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || body["text"] != expected {
				t.Errorf("instruction body differs from standing rule; decode error=%v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected gateway path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()
	_, svc := newVoiceTestService(t, Config{GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client(), UpstreamRoute: "avas"})
	// newVoiceTestService isolates unrelated legacy tests from instruction pushes.
	// Use the production default here without depending on new configuration fields.
	svc.recall = Config{UpstreamRoute: "avas"}
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "instruction-user", DeviceID: "device"}
	req := createRequest{RequestID: "instruction-request", OfferSDP: "offer"}
	created, err := svc.create(context.Background(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	if pushes != 1 || created.AnswerSDP != "answer" {
		t.Fatalf("instruction pushes before SDP return=%d, want 1; answer=%q", pushes, created.AnswerSDP)
	}
	if _, err := svc.create(context.Background(), p, req); err != nil {
		t.Fatal(err)
	}
	if pushes != 1 || creates != 1 {
		t.Fatalf("replay pushes=%d creates=%d, want one each", pushes, creates)
	}
}
