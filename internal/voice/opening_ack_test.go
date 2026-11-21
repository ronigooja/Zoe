package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		{"applied", `{"applied":true}`, "unverified", 17},
		{"silent-without-binding", `{"applied":true,"silent":true}`, "unverified", 17},
		{"strict", "", "delivered", 17},
		{"wrong-version", "", "unverified", 17},
		{"wrong-hash", "", "unverified", 17},
		{"unverified", `{"applied":true,"silent":"unverified"}`, "unverified", 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/voice/sessions" {
					t.Errorf("unexpected live context request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotImplemented)
					return
				}
				var request gatewaySessionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.OpeningContext == nil || request.OpeningContext.Kind != "full" {
					t.Errorf("default AVAS create omitted full opening context: %+v, %v", request, err)
				}
				var acknowledgement any = json.RawMessage(tc.acknowledgement)
				if tc.acknowledgement == "" {
					receipt := appliedTestOpening(request.OpeningContext)
					if tc.name == "wrong-version" {
						receipt.Version++
					} else if tc.name == "wrong-hash" {
						receipt.ContentHash = "wrong"
					}
					acknowledgement = receipt
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"answer_sdp": "answer", "voice_session_id": "gateway", "opening_context": acknowledgement, "context_chars": tc.chars})
			}))
			defer gateway.Close()
			store, svc := newVoiceTestService(t, Config{GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client()})
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

func appliedTestOpening(opening *openingContext) *openingContextResult {
	return &openingContextResult{Applied: true, Silent: true, Version: opening.Version, ContentHash: openingContextHash(opening)}
}

func acknowledgeTestOpening(r *deliveryRig, session string, opening *openingContext, receipt *openingContextResult) error {
	ctx := context.Background()
	tx, err := r.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.svc.acknowledgeOpeningResults(ctx, tx, openingTestOwner, session, opening, receipt); err != nil {
		return err
	}
	return tx.Commit()
}

func TestOpeningReceiptEventAtomicAndIdempotent(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "full")
	r.seedTask(t, "task-2", "user-1")
	r.seedEntry(t, "task-2", "entry-2", "second result")
	r.seedEvent(t, 2, "task-2", "run-2", "entry-2")
	r.exec(t, `UPDATE tasks SET summary_result='sent summary' WHERE id='task-1'`)
	r.svc.consumeResultEvents(context.Background())
	r.seedCall(t, "opening", "user-1", 1)
	opening := reserveTestOpening(t, r, "opening")
	r.exec(t, `UPDATE tasks SET summary_result='changed after send' WHERE id='task-1'`)
	r.exec(t, `CREATE TRIGGER fail_second BEFORE INSERT ON voice_context_events WHEN NEW.source_id='entry-2' BEGIN SELECT RAISE(ABORT, 'fixture'); END;`)
	receipt := appliedTestOpening(opening)
	if err := acknowledgeTestOpening(r, "opening", opening, receipt); err == nil {
		t.Fatal("event failure did not abort acknowledgement")
	}
	var reserved, events int
	if err := r.store.DB().QueryRow(`SELECT (SELECT count(*) FROM voice_result_opening WHERE state='reserved'),(SELECT count(*) FROM voice_context_events)`).Scan(&reserved, &events); err != nil || reserved != 2 || events != 0 {
		t.Fatalf("reserved=%d events=%d err=%v", reserved, events, err)
	}
	r.exec(t, `DROP TRIGGER fail_second`)
	for i := 0; i < 2; i++ {
		if err := acknowledgeTestOpening(r, "opening", opening, receipt); err != nil {
			t.Fatal(err)
		}
	}
	var consumed int
	var text string
	if err := r.store.DB().QueryRow(`SELECT (SELECT count(*) FROM voice_result_opening WHERE state='consumed' AND consumed_at IS NOT NULL),(SELECT count(*) FROM voice_context_events),(SELECT text FROM voice_context_events WHERE source_kind='task_result' AND source_id='entry-1' AND session_id='opening' AND role='background')`).Scan(&consumed, &events, &text); err != nil || consumed != 2 || events != 2 || text != opening.Items[0].Text {
		t.Fatalf("consumed=%d events=%d text=%q err=%v", consumed, events, text, err)
	}
}

func TestOpeningReservationCreateReleaseRules(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantState, wantReservation string
		status                                 int
	}{
		{"rejected", "", "failed", "", http.StatusBadRequest},
		{"attested-never-established", "", "failed", "", http.StatusBadGateway},
		{"unknown", "", "quarantined", "reserved", http.StatusServiceUnavailable},
		{"invalid-response", `{"answer_sdp":"answer"}`, "quarantined", "reserved", http.StatusOK},
		{"receipt-wrong", `{"answer_sdp":"answer","voice_session_id":"gateway","opening_context":{"applied":true,"silent":true,"version":1,"content_hash":"wrong"}}`, "active", "", http.StatusOK},
		{"receipt-missing", `{"answer_sdp":"answer","voice_session_id":"gateway"}`, "active", "reserved", http.StatusOK},
		{"receipt-strict", "", "active", "consumed", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := checkpointRig(t)
			r.setup(t, "original")
			r.exec(t, `UPDATE event_counters SET current_cursor=1 WHERE workspace_id='default'`)
			r.svc.consumeResultEvents(context.Background())
			gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet {
					if tc.name == "attested-never-established" {
						_ = json.NewEncoder(w).Encode(gatewayOutcomeResponse{RequestID: req.Header.Get("X-Request-ID"), Outcome: "never_established"})
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					return
				}
				var request gatewaySessionRequest
				var state, text string
				if err := r.store.DB().QueryRow(`SELECT state,text FROM voice_result_opening`).Scan(&state, &text); err != nil || state != "reserved" {
					t.Errorf("pre-call reservation: state=%q err=%v", state, err)
				}
				if err := json.NewDecoder(req.Body).Decode(&request); err != nil || request.OpeningContext == nil || len(request.OpeningContext.Items) != 1 || request.OpeningContext.Items[0].Text != text {
					t.Errorf("wire request differs from manifest: %+v, %v", request.OpeningContext, err)
					return
				}
				w.WriteHeader(tc.status)
				if tc.name == "receipt-strict" {
					_ = json.NewEncoder(w).Encode(gatewaySessionResponse{AnswerSDP: "answer", VoiceSessionID: "gateway", OpeningContext: appliedTestOpening(request.OpeningContext)})
				} else {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer gateway.Close()
			svc := newService(r.store.DB(), Config{GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client()})
			defer svc.Stop()
			_, createErr := svc.create(context.Background(), openingTestOwner, createRequest{RequestID: "test", OfferSDP: "offer"})
			if (createErr == nil) != (tc.wantState == "active") {
				t.Fatalf("create error=%v", createErr)
			}
			var state, reservation string
			if err := r.store.DB().QueryRow(`SELECT v.state,coalesce(o.state,'') FROM voice_sessions v LEFT JOIN voice_result_opening o ON o.session_id=v.id`).Scan(&state, &reservation); err != nil || state != tc.wantState || reservation != tc.wantReservation {
				t.Fatalf("state=%q reservation=%q query err=%v create err=%v", state, reservation, err, createErr)
			}
			if tc.name == "receipt-missing" {
				r.exec(t, `UPDATE voice_sessions SET state='ended'`)
				var n int
				if err := r.store.DB().QueryRow(`SELECT count(*) FROM voice_result_opening`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("terminal release: count=%d err=%v", n, err)
				}
			}
		})
	}
}

func TestRejectLegacyVoiceRouteBeforeRegistration(t *testing.T) {
	for _, route := range []string{"wm", "bogus"} {
		err := Register(nil, nil, Config{UpstreamRoute: route})
		if err == nil || !strings.Contains(err.Error(), "AVAS only") {
			t.Fatalf("route %q: expected actionable AVAS-only error, got %v", route, err)
		}
	}
}
