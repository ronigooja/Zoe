package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func TestInstructionConfiguration(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"", DefaultInstructionText}, {"off", ""}, {"custom rule", "custom rule"}, {strings.Repeat("a", 6000), strings.Repeat("a", 6000)}} {
		got, err := ResolveInstructionText(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("configuration resolution differs: error=%v", err)
		}
	}
	for _, input := range []string{strings.Repeat("a", 6001), strings.Repeat("界", 2001), string([]byte{0xff})} {
		if _, err := ResolveInstructionText(input); err == nil || !strings.Contains(err.Error(), "ZOE_VOICE_INSTRUCTION_TEXT") {
			t.Fatal("invalid instruction not rejected with named configuration error")
		}
		if err := Register(nil, nil, Config{InstructionText: input}); err == nil || !strings.Contains(err.Error(), "ZOE_VOICE_INSTRUCTION_TEXT") {
			t.Fatal("registration did not validate startup configuration")
		}
	}
}

// Decode the wire body rather than the production request type so this test
// compiles on the baseline and fails for the missing create-session field.
func TestInstructionCreateSessionWireAndReconnect(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"default", "", DefaultInstructionText},
		{"custom", "规则: \"请委托\"\n保留换行与$符号", "规则: \"请委托\"\n保留换行与$符号"},
		{"off", "off", ""},
		{"oversize-defense", strings.Repeat("界", 2001), ""},
		{"invalid-defense", string([]byte{0xff}), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates, pushes int
			gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/voice/sessions" {
					pushes++
					t.Errorf("unexpected post-create instruction request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotImplemented)
					return
				}
				creates++
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				var got string
				wire, present := body["instructions"]
				if present {
					if err := json.Unmarshal(wire, &got); err != nil {
						t.Error(err)
					}
				}
				if got != tc.want || present != (tc.want != "") {
					t.Errorf("create-session instructions=%q present=%t; want=%q present=%t", got, present, tc.want, tc.want != "")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"voice_session_id": fmt.Sprintf("gateway-%d", creates), "voice_session_generation": creates, "answer_sdp": "answer"})
			}))
			defer gateway.Close()
			store, svc := newVoiceTestService(t, Config{InstructionText: "off", GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client(), UpstreamRoute: "avas"})
			// Bypass startup only to exercise the defensive invalid-config path.
			// The helper otherwise replaces an empty instruction with off.
			svc.recall.InstructionText = tc.input
			p := contracts.Principal{WorkspaceID: "default", SubjectID: "user", DeviceID: "device"}
			for generation := 1; generation <= 2; generation++ {
				req := createRequest{RequestID: fmt.Sprintf("media-%d", generation), OfferSDP: "offer"}
				created, err := svc.create(context.Background(), p, req)
				if err != nil || created.AnswerSDP != "answer" {
					t.Fatalf("instruction handling blocked session creation: %v", err)
				}
				if _, err := svc.create(context.Background(), p, req); err != nil {
					t.Fatal(err)
				}
				// Simulate the preceding media session's verified end before
				// reconnecting through the same production create path.
				if _, err := store.DB().Exec(`UPDATE voice_sessions SET state='ended' WHERE id=?`, created.SessionID); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=0,reason_code='',source_session_id='' WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			}
			if creates != 2 || pushes != 0 {
				t.Fatalf("creates=%d pushes=%d; want 2 creates, no instruction context pushes", creates, pushes)
			}
			var rows int
			if err := store.DB().QueryRow(`SELECT count(*) FROM voice_instruction_deliveries`).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("separate delivery evidence written: rows=%d error=%v", rows, err)
			}
		})
	}
}

func TestInstructionConnectedDoesNotPush(t *testing.T) {
	var pushes int
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes++
		w.WriteHeader(http.StatusNotImplemented)
	}))
	defer gateway.Close()
	cfg := Config{InstructionText: "callback rule", GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client()}
	ts, store, key := handoffHTTPServerWithConfig(t, cfg)
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gateway-callback',gateway_media_generation=7 WHERE id='handoff-session'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/voice/sessions/handoff-session/connected", strings.NewReader(`{"generation":1}`))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("connected status=%d", resp.StatusCode)
		}
	}
	if pushes != 0 {
		t.Fatalf("connected sent %d obsolete instruction pushes", pushes)
	}
}
