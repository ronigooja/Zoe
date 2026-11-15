package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPoolStatusRecoveryRequiresVoiceOpen(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("voice_open=%v", allowed), func(t *testing.T) {
			var calls atomic.Int64
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				body := fmt.Sprintf(`{"request_id":"auth-request","outcome":"released","evidence_id":%q}`, testCreationEvidenceID)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			scopes := []string{"voice:read"}
			if allowed {
				scopes = append(scopes, "voice:open")
			}
			server, store, key := handoffHTTPServerWithScopes(t, Config{GatewayURL: "https://gateway.test", GatewayAPIKey: "key", HTTPClient: client}, scopes)
			seedRecoverySession(t, store, "auth", "quarantined")
			if _, err := store.DB().Exec(`UPDATE voice_sessions SET last_error_code='gateway_outcome_unknown' WHERE id='auth'`); err != nil {
				t.Fatal(err)
			}
			reserveRecoveryPool(t, store, "auth", "gateway_outcome_unknown")
			req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/voice/pool/status", nil)
			req.Header.Set("Authorization", "Bearer "+key)
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var result struct {
				Blocked bool `json:"blocked"`
			}
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil || response.StatusCode != 200 {
				t.Fatalf("status=%d decode=%v", response.StatusCode, err)
			}
			var state string
			var blocked, audits, events int
			if err := store.DB().QueryRow(`SELECT state,(SELECT blocked FROM voice_pool_state),(SELECT COUNT(*) FROM voice_pool_reconciliations),(SELECT COUNT(*) FROM events WHERE event_type='voice.pool.reconciled') FROM voice_sessions WHERE id='auth'`).Scan(&state, &blocked, &audits, &events); err != nil {
				t.Fatal(err)
			}
			if allowed {
				if result.Blocked || state != "failed" || blocked != 0 || audits != 1 || events != 1 || calls.Load() != 1 {
					t.Fatalf("blocked=%v state=%s dbblocked=%d audits=%d events=%d calls=%d", result.Blocked, state, blocked, audits, events, calls.Load())
				}
			} else if !result.Blocked || state != "quarantined" || blocked != 1 || audits != 0 || events != 0 || calls.Load() != 0 {
				t.Fatalf("read-only poll mutated recovery: blocked=%v state=%s dbblocked=%d audits=%d events=%d calls=%d", result.Blocked, state, blocked, audits, events, calls.Load())
			}
		})
	}
}

func TestGatewayOutcomeBodyBoundsAndTrailingData(t *testing.T) {
	valid := fmt.Sprintf(`{"request_id":"body-request","outcome":"released","evidence_id":%q}`, testCreationEvidenceID)
	for _, tc := range []struct {
		name, body string
		recover    bool
	}{
		{"valid", valid + " \n\t", true},
		{"exact_limit", valid + strings.Repeat(" ", (64<<10)-len(valid)), true},
		{"oversized_whitespace", valid + strings.Repeat(" ", (64<<10)+1-len(valid)), false},
		{"garbage_beyond_limit", valid + strings.Repeat(" ", (64<<10)-len(valid)) + "garbage", false},
		{"trailing_garbage", valid + "garbage", false},
		{"second_object", valid + "{}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, svc := newVoiceTestService(t, Config{GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
			seedRecoverySession(t, store, "body", "quarantined")
			reserveRecoveryPool(t, store, "body", "gateway_outcome_unknown")
			if _, err := store.DB().Exec(`UPDATE voice_sessions SET last_error_code='gateway_outcome_unknown' WHERE id='body'`); err != nil {
				t.Fatal(err)
			}
			svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			if err := svc.recoverQuarantinedCreate(context.Background()); err != nil {
				t.Fatal(err)
			}
			var state string
			var blocked, audits int
			if err := store.DB().QueryRow(`SELECT state,(SELECT blocked FROM voice_pool_state),(SELECT COUNT(*) FROM voice_pool_reconciliations) FROM voice_sessions`).Scan(&state, &blocked, &audits); err != nil {
				t.Fatal(err)
			}
			if tc.recover {
				if state != "failed" || blocked != 0 || audits != 1 {
					t.Fatalf("state=%s blocked=%d audits=%d", state, blocked, audits)
				}
			} else if state != "quarantined" || blocked != 1 || audits != 0 {
				t.Fatalf("state=%s blocked=%d audits=%d", state, blocked, audits)
			}
		})
	}
}
