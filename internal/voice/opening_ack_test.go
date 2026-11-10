package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func TestVoiceAVASCreateAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name, acknowledgement, want string
		chars                       int
	}{
		{"missing", "null", "unverified", 0},
		{"unsupported", `{"supported":false}`, "unsupported", 0},
		{"applied", `{"applied":true}`, "delivered", 17},
		{"silent-applied", `{"applied":true,"silent":true}`, "delivered", 17},
		{"unverified", `{"applied":true,"silent":"unverified"}`, "unverified", 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/voice/sessions" {
					t.Errorf("unexpected live context request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotImplemented)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"answer_sdp": "answer", "voice_session_id": "gateway", "opening_context": json.RawMessage(tc.acknowledgement), "context_chars": tc.chars})
			}))
			defer gateway.Close()
			store, svc := newVoiceTestService(t, Config{GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client(), UpstreamRoute: "avas", ContextAssembler: func(context.Context, contracts.Principal, string, string, string, int) (contracts.MemoryContext, error) {
				return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Subject: "user", Key: "rule", Content: "context"}}}, nil
			}})
			created, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user", DeviceID: "device"}, createRequest{RequestID: "ack", OfferSDP: "offer"})
			if err != nil {
				t.Fatal(err)
			}
			var delivery string
			var chars int
			if err := store.DB().QueryRow(`SELECT opening_context_delivery,gateway_context_chars FROM voice_sessions WHERE id=?`, created.SessionID).Scan(&delivery, &chars); err != nil {
				t.Fatal(err)
			}
			if delivery != tc.want || chars != tc.chars || created.OpeningContextDelivery != tc.want || created.GatewayContextChars != tc.chars {
				t.Fatalf("acknowledgement delivery=%q chars=%d response=%+v; want %q chars=%d", delivery, chars, created, tc.want, tc.chars)
			}
		})
	}
}
