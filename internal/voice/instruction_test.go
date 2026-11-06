package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func TestInstructionConfiguration(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"", DefaultInstructionText}, {"off", ""}, {"custom rule", "custom rule"}, {strings.Repeat("a", 6000), strings.Repeat("a", 6000)}} {
		t.Setenv("ZOE_VOICE_INSTRUCTION_TEXT", tc.input)
		got, err := ResolveInstructionText(os.Getenv("ZOE_VOICE_INSTRUCTION_TEXT"))
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

func TestInstructionConnectedHTTPRetriesOnce(t *testing.T) {
	var pushes int
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer gateway.Close()
	cfg := Config{InstructionText: "callback rule", GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client()}
	ts, store, key := handoffHTTPServerWithConfig(t, cfg)
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gateway-callback',gateway_media_generation=7 WHERE id='handoff-session'`); err != nil {
		t.Fatal(err)
	}
	newService(store.DB(), cfg).deliverInstruction(context.Background(), "handoff-session", false)
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
	if pushes != 2 {
		t.Fatalf("connected pushes=%d want initial plus one retry", pushes)
	}
}

func TestInstructionNetworkFailureAndVersionChange(t *testing.T) {
	var pushes int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/voice/sessions" {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"voice_session_id":"network-gateway","voice_session_generation":2,"answer_sdp":"answer"}`))}, nil
		}
		pushes++
		return nil, errors.New("private network error must not be logged")
	})}
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	_, svc := newVoiceTestService(t, Config{InstructionText: "network rule", GatewayURL: "https://gateway.test", GatewayAPIKey: "key", HTTPClient: client})
	created, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user", DeviceID: "device"}, createRequest{RequestID: "network", OfferSDP: "offer"})
	if err != nil {
		t.Fatal(err)
	}
	svc.deliverInstruction(context.Background(), created.SessionID, true)
	svc.deliverInstruction(context.Background(), created.SessionID, true)
	if pushes != 2 {
		t.Fatalf("network pushes=%d want=2", pushes)
	}
	svc.recall.InstructionText = "new network rule"
	svc.deliverInstruction(context.Background(), created.SessionID, false)
	if pushes != 3 {
		t.Fatalf("changed version pushes=%d want=3", pushes)
	}
	if strings.Contains(logs.String(), "network rule") || strings.Contains(logs.String(), "private network error") {
		t.Fatal("private text leaked to logs")
	}
}

func TestInstructionCustomOffFailureRetryAndGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		fail       bool
	}{{name: "custom", text: "private custom rule"}, {name: "off", text: "off"}, {name: "failure", text: "private failed rule", fail: true}} {
		t.Run(tc.name, func(t *testing.T) {
			var pushes int
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/voice/sessions" {
					_ = json.NewEncoder(w).Encode(map[string]any{"voice_session_id": "gateway-custom", "voice_session_generation": 3, "answer_sdp": "answer"})
					return
				}
				if r.URL.Path != "/v1/voice/sessions/gateway-custom/context" || r.Method != http.MethodPost {
					t.Errorf("unexpected gateway request %s %s", r.Method, r.URL.Path)
				}
				pushes++
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["text"] != InstructionPrefix+tc.text {
					t.Errorf("custom text differs: error=%v", err)
				}
				if tc.fail {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = w.Write([]byte(tc.text))
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer gateway.Close()
			store, svc := newVoiceTestService(t, Config{InstructionText: tc.text, GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client(), UpstreamRoute: "avas"})
			p := contracts.Principal{WorkspaceID: "default", SubjectID: "user", DeviceID: "device"}
			created, err := svc.create(context.Background(), p, createRequest{RequestID: "custom", OfferSDP: "offer"})
			if err != nil || created.AnswerSDP != "answer" {
				t.Fatalf("instruction push blocked call: %v", err)
			}
			want := 1
			if tc.text == "off" {
				want = 0
			}
			if pushes != want {
				t.Fatalf("pushes=%d want=%d", pushes, want)
			}
			if tc.text == "off" {
				return
			}
			var delivery string
			if err := store.DB().QueryRow(`SELECT instruction_delivery FROM voice_instruction_deliveries WHERE session_id=?`, created.SessionID).Scan(&delivery); err != nil {
				t.Fatal(err)
			}
			if tc.fail && delivery != "failed" || !tc.fail && delivery != "delivered" {
				t.Fatalf("delivery=%s", delivery)
			}
			for i := 0; i < 2; i++ {
				svc.deliverInstruction(context.Background(), created.SessionID, true)
			}
			if tc.fail {
				want++
			}
			if pushes != want {
				t.Fatalf("after repeated connected callbacks pushes=%d want=%d", pushes, want)
			}
			if strings.Contains(logs.String(), tc.text) || strings.Contains(logs.String(), InstructionPrefix) {
				t.Fatal("logs expose instruction text")
			}
			if tc.fail && !strings.Contains(logs.String(), "status=502") {
				t.Fatal("missing safe gateway status log")
			}
			if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_media_generation=4 WHERE id=?`, created.SessionID); err != nil {
				t.Fatal(err)
			}
			svc.deliverInstruction(context.Background(), created.SessionID, false)
			svc.deliverInstruction(context.Background(), created.SessionID, false)
			if pushes != want+1 {
				t.Fatalf("new media generation pushes=%d want=%d", pushes, want+1)
			}
		})
	}
}
