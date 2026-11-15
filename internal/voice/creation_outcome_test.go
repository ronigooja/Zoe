package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

const testCreationEvidenceID = "ev_0123456789abcdef0123456789abcdef"

func TestVoiceCreateGatewayAttestationReleasedReleasesPool(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
	posts := 0
	svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, ""
		if r.Method == http.MethodPost {
			posts++
			status = http.StatusBadGateway
			if posts > 1 {
				status, body = http.StatusOK, `{"voice_session_id":"retry-media","answer_sdp":"v=0"}`
			}
		} else {
			body = fmt.Sprintf(`{"request_id":%q,"outcome":"released","evidence_id":%q}`, r.Header.Get("X-Request-ID"), testCreationEvidenceID)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}
	_, err := svc.create(context.Background(), p, createRequest{RequestID: "released", OfferSDP: "v=0"})
	var apiErr *control.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != "voice_gateway_negotiation_failed" {
		t.Fatalf("create error=%v", err)
	}
	var state, code, reason string
	var blocked, events int
	if err := store.DB().QueryRow(`SELECT state,last_error_code,(SELECT blocked FROM voice_pool_state),(SELECT reason FROM voice_pool_reconciliations WHERE method='gateway_attestation'),(SELECT COUNT(*) FROM events WHERE event_type='voice.pool.reconciled' AND CAST(payload AS TEXT) LIKE ?) FROM voice_sessions WHERE request_id='released'`, "%"+testCreationEvidenceID+"%").Scan(&state, &code, &blocked, &reason, &events); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code != "gateway_negotiation_released" || blocked != 0 || reason != "gateway outcome released evidence_id="+testCreationEvidenceID || events != 1 {
		t.Fatalf("state=%s code=%s blocked=%d reason=%s events=%d", state, code, blocked, reason, events)
	}
	if result, err := svc.create(context.Background(), p, createRequest{RequestID: "retry", OfferSDP: "v=0"}); err != nil || result.State != "active" {
		t.Fatalf("retry state=%s err=%v", result.State, err)
	}
}

func TestVoiceCreateReleasedInvalidAttestationPreservesQuarantine(t *testing.T) {
	for _, kind := range []string{"missing_evidence", "short_evidence", "uppercase_evidence", "trailing_evidence", "mismatched_response", "mismatched_persisted", "bound", "open", "unknown", "not_found", "malformed", "trailing_json"} {
		t.Run(kind, func(t *testing.T) {
			store, svc := newVoiceTestService(t, Config{GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
			svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, body := http.StatusBadGateway, ""
				if r.Method == http.MethodGet {
					requestID, outcome, evidence := r.Header.Get("X-Request-ID"), "released", testCreationEvidenceID
					switch kind {
					case "missing_evidence":
						evidence = ""
					case "short_evidence":
						evidence = "ev_0123"
					case "uppercase_evidence":
						evidence = strings.ToUpper(evidence)
					case "trailing_evidence":
						evidence += "\n"
					case "mismatched_response":
						requestID = "other"
					case "mismatched_persisted":
						if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_request_id='changed-request'`); err != nil {
							t.Fatal(err)
						}
					case "bound":
						if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='bound-media'`); err != nil {
							t.Fatal(err)
						}
					case "open", "unknown":
						outcome = kind
					}
					status, body = http.StatusOK, fmt.Sprintf(`{"request_id":%q,"outcome":%q,"evidence_id":%q}`, requestID, outcome, evidence)
					if kind == "not_found" {
						status = http.StatusNotFound
					} else if kind == "malformed" {
						body = `{`
					} else if kind == "trailing_json" {
						body += `{}`
					}
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			_, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "invalid", OfferSDP: "v=0"})
			var apiErr *control.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != "voice_outcome_unknown" {
				t.Fatalf("create error=%v", err)
			}
			var state string
			var blocked, audit int
			if err := store.DB().QueryRow(`SELECT state,(SELECT blocked FROM voice_pool_state),(SELECT COUNT(*) FROM voice_pool_reconciliations) FROM voice_sessions`).Scan(&state, &blocked, &audit); err != nil {
				t.Fatal(err)
			}
			if state != "quarantined" || blocked != 1 || audit != 0 {
				t.Fatalf("state=%s blocked=%d audit=%d", state, blocked, audit)
			}
		})
	}
}

func TestVoiceCreateNextStartRecoversReleased(t *testing.T) {
	testReleasedCreateRecovery(t, false)
}

func TestGatewayAttestationSweepRecoversReleased(t *testing.T) {
	testReleasedCreateRecovery(t, true)
}

func testReleasedCreateRecovery(t *testing.T, sweep bool) {
	t.Helper()
	for _, state := range []string{"quarantined", "allocating"} {
		if !sweep && state == "allocating" {
			continue
		}
		t.Run(state, func(t *testing.T) {
			now := time.Now().UTC()
			store, svc := newVoiceTestService(t, Config{Now: func() time.Time { return now }, GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
			at := now.Add(-3 * time.Minute).Format(time.RFC3339Nano)
			if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,last_error_code,gateway_request_id,created_at,updated_at) VALUES('old','default','user-1','device-1','old',X'01',1,?,'gateway_outcome_unknown','old-request',?,?)`, state, at, at); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,reason_code='gateway_outcome_unknown',source_session_id='old'`); err != nil {
				t.Fatal(err)
			}
			gets, posts := 0, 0
			svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"voice_session_id":"new-media","answer_sdp":"v=0"}`
				if r.Method == http.MethodGet {
					gets++
					if r.URL.Path != "/v1/voice/requests/old-request/outcome" {
						t.Fatalf("outcome path=%s", r.URL.Path)
					}
					body = fmt.Sprintf(`{"request_id":"old-request","outcome":"released","evidence_id":%q}`, testCreationEvidenceID)
				} else {
					posts++
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			if sweep {
				svc.attestStaleCreates(context.Background())
				svc.attestStaleCreates(context.Background())
			} else {
				result, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1"}, createRequest{RequestID: "next", OfferSDP: "v=0"})
				if err != nil || result.State != "active" || posts != 1 {
					t.Fatalf("next state=%s posts=%d err=%v", result.State, posts, err)
				}
			}
			var oldState, code, reason string
			var audit, blocked int
			if err := store.DB().QueryRow(`SELECT state,last_error_code,(SELECT COUNT(*) FROM voice_pool_reconciliations),(SELECT reason FROM voice_pool_reconciliations),(SELECT blocked FROM voice_pool_state) FROM voice_sessions WHERE id='old'`).Scan(&oldState, &code, &audit, &reason, &blocked); err != nil {
				t.Fatal(err)
			}
			if oldState != "failed" || code != "gateway_negotiation_released" || audit != 1 || gets != 1 || !strings.Contains(reason, testCreationEvidenceID) || (sweep && blocked != 0) {
				t.Fatalf("old=%s code=%s audit=%d gets=%d reason=%s blocked=%d", oldState, code, audit, gets, reason, blocked)
			}
		})
	}
}
