package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/storage"
)

func fencedCalls(t *testing.T, now *time.Time, transport roundTripFunc) callHarness {
	t.Helper()
	cfg := callConfig()
	cfg.Now = func() time.Time { return *now }
	cfg.HTTPClient = &http.Client{Transport: transport}
	db, _ := newVoiceTestService(t, Config{})
	return newCallHarness(t, db, cfg)
}

func creationLookup(requestID, result string) (*http.Response, error) {
	return gatewayFixture(fmt.Sprintf(`{"request_id":%q,"outcome":%q}`, requestID, result)), nil
}

func fenceReply(requestID, result string) (*http.Response, error) {
	return gatewayFixture(fmt.Sprintf(`{"request_id":%q,"result":%q}`, requestID, result)), nil
}

func assertCreateHold(t *testing.T, db *storage.Store, id, wantState, wantCode, wantSource string, wantAudit int) {
	t.Helper()
	var state, code, source string
	var blocked bool
	var audit, event int
	err := db.DB().QueryRow(`SELECT s.state,s.last_error_code,p.source_session_id,p.blocked,
		(SELECT count(*) FROM voice_pool_reconciliations WHERE source_session_id=s.id AND generation=s.generation),
		(SELECT count(*) FROM events WHERE event_type='voice.pool.reconciled')
		FROM voice_sessions s CROSS JOIN voice_pool_state p WHERE s.id=? AND p.id=1`, id).Scan(&state, &code, &source, &blocked, &audit, &event)
	if err != nil || state != wantState || code != wantCode || source != wantSource || blocked != (wantSource != "") || audit != wantAudit || event != wantAudit {
		t.Fatalf("state=%q code=%q correct source=%v blocked=%v audit=%d events=%d err=%v", state, code, source == wantSource, blocked, audit, event, err)
	}
}

func TestUnknownCreateTimeoutReleasesOnlyMatchingSource(t *testing.T) {
	for _, changed := range []string{"none", "source", "generation", "request", "hold_reason"} {
		t.Run(changed, func(t *testing.T) {
			now := time.Date(2026, 11, 26, 1, 2, 3, 456, time.UTC)
			var start callStartResponse
			var db *storage.Store
			fences := 0
			h := fencedCalls(t, &now, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					return creationLookup(start.GatewayRequestID, "unknown")
				}
				if r.Method != http.MethodPost || r.URL.Path != "/base/v1/voice/requests/"+start.GatewayRequestID+"/fence" || r.Body != nil || r.Header.Get("Authorization") != "Bearer deployment-key" {
					t.Fatal("invalid fence request")
				}
				fences++
				switch changed {
				case "source":
					reserveRecoveryPool(t, db, "different", "negotiation_in_progress")
				case "generation":
					if _, err := db.DB().Exec(`UPDATE voice_sessions SET generation=generation+1 WHERE id=?`, start.SessionID); err != nil {
						t.Fatal(err)
					}
				case "request":
					if _, err := db.DB().Exec(`UPDATE voice_sessions SET gateway_request_id='replacement-request' WHERE id=?`, start.SessionID); err != nil {
						t.Fatal(err)
					}
				case "hold_reason":
					reserveRecoveryPool(t, db, start.SessionID, "another_reason")
				}
				return fenceReply(start.GatewayRequestID, fencedNoLiveMedia)
			})
			db = h.store
			start, _ = h.open(201)
			now = now.Add(5*time.Minute - time.Nanosecond)
			h.svc.attestStaleCreates(context.Background())
			assertCreateHold(t, db, start.SessionID, "allocating", "", start.SessionID, 0)
			if fences != 0 {
				t.Fatal("fenced before five minutes")
			}
			now = now.Add(time.Nanosecond)
			h.svc.attestStaleCreates(context.Background())
			if fences != 1 {
				t.Fatalf("fence calls at exact boundary: %d", fences)
			}
			if changed == "none" {
				h.svc.attestStaleCreates(context.Background())
				assertCreateHold(t, db, start.SessionID, "failed", "unresolved_timeout", "", 1)
				var reason, method string
				if err := db.DB().QueryRow(`SELECT reason,method FROM voice_pool_reconciliations`).Scan(&reason, &method); err != nil || reason != "unresolved_timeout" || method != "gateway_attestation" || fences != 1 {
					t.Fatalf("wrong or repeated audit: reason=%q method=%q fences=%d err=%v", reason, method, fences, err)
				}
			} else {
				source := start.SessionID
				if changed == "source" {
					source = "different"
				}
				assertCreateHold(t, db, start.SessionID, "allocating", "", source, 0)
			}
		})
	}
}

func TestCreateTimeoutDoesNotTreatOpenOrLookupFailureAsUnknown(t *testing.T) {
	for _, name := range []string{"lookup_open", "lookup_transport", "lookup_malformed", "lookup_duplicate_outcome", "lookup_duplicate_request", "lookup_trailing", "lookup_wrong_type", "lookup_null", "lookup_unknown_field", "missing_then_404", "fence_transport", "fence_malformed", "duplicate_result", "wrong_request", "busy", "open", "unknown", "released_without_evidence"} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC)
			var start callStartResponse
			fences, lookups := 0, 0
			notFound := func() (*http.Response, error) {
				resp := gatewayFixture(`{}`)
				resp.StatusCode = 404
				return resp, nil
			}
			h := fencedCalls(t, &now, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					lookups++
					switch name {
					case "lookup_open":
						return creationLookup(start.GatewayRequestID, "open")
					case "lookup_transport":
						return nil, errors.New("private synthetic transport failure")
					case "lookup_malformed":
						return gatewayFixture(`{`), nil
					case "lookup_duplicate_outcome":
						if lookups > 1 {
							return creationLookup(start.GatewayRequestID, "unknown")
						}
						return gatewayFixture(fmt.Sprintf(`{"request_id":%q,"outcome":"open","outcome":"unknown"}`, start.GatewayRequestID)), nil
					case "lookup_duplicate_request":
						return gatewayFixture(fmt.Sprintf(`{"request_id":"different","request_id":%q,"outcome":"unknown"}`, start.GatewayRequestID)), nil
					case "lookup_trailing":
						return gatewayFixture(fmt.Sprintf(`{"request_id":%q,"outcome":"unknown"} {}`, start.GatewayRequestID)), nil
					case "lookup_wrong_type":
						return gatewayFixture(fmt.Sprintf(`{"request_id":%q,"outcome":42}`, start.GatewayRequestID)), nil
					case "lookup_null":
						return gatewayFixture(fmt.Sprintf(`{"request_id":%q,"outcome":"unknown","evidence_id":null}`, start.GatewayRequestID)), nil
					case "lookup_unknown_field":
						return gatewayFixture(fmt.Sprintf(`{"request_id":%q,"outcome":"unknown","extra":true}`, start.GatewayRequestID)), nil
					case "missing_then_404":
						return notFound()
					default:
						return creationLookup(start.GatewayRequestID, "unknown")
					}
				}
				fences++
				switch name {
				case "missing_then_404":
					return notFound()
				case "fence_transport":
					return nil, errors.New("private synthetic fence failure")
				case "fence_malformed":
					return gatewayFixture(`{"result":"fenced_no_live_media"} trailing`), nil
				case "duplicate_result":
					return gatewayFixture(fmt.Sprintf(`{"request_id":%q,"result":"busy","result":"fenced_no_live_media"}`, start.GatewayRequestID)), nil
				case "wrong_request":
					return fenceReply("different-request", fencedNoLiveMedia)
				case "released_without_evidence":
					return fenceReply(start.GatewayRequestID, "released")
				default:
					return fenceReply(start.GatewayRequestID, name)
				}
			})
			start, _ = h.open(201)
			now = now.Add(unresolvedCreateTimeout)
			h.svc.attestStaleCreates(context.Background())
			assertCreateHold(t, h.store, start.SessionID, "allocating", "", start.SessionID, 0)
			if (fences == 0) != strings.HasPrefix(name, "lookup_") || fences > 1 {
				t.Fatalf("unexpected fence call count: %d", fences)
			}
		})
	}
}

func TestQuarantinedCreateTimeoutRequiresExactUnboundSource(t *testing.T) {
	for _, mode := range []string{"release", "busy", "transport", "source_changed", "quarantine_renewed", "bound"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC)
			var h callHarness
			var start callStartResponse
			lookups, fences := 0, 0
			h = fencedCalls(t, &now, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					lookups++
					return creationLookup(start.GatewayRequestID, "unknown")
				}
				fences++
				switch mode {
				case "busy":
					return fenceReply(start.GatewayRequestID, "busy")
				case "transport":
					return nil, errors.New("fixture transport error")
				case "source_changed":
					reserveRecoveryPool(t, h.store, "next-source", "negotiation_in_progress")
				case "quarantine_renewed":
					if _, err := h.store.DB().Exec(`UPDATE voice_sessions SET updated_at=? WHERE id=?`, now.Format(time.RFC3339Nano), start.SessionID); err != nil {
						t.Fatal(err)
					}
				}
				return fenceReply(start.GatewayRequestID, fencedNoLiveMedia)
			})
			start, _ = h.open(201)
			now = now.Add(time.Hour) // The age of the allocation is not the age of the quarantine.
			binding := ""
			if mode == "bound" {
				binding = "existing-binding"
			}
			if _, err := h.store.DB().Exec(`UPDATE voice_sessions SET state='quarantined',last_error_code='gateway_outcome_unknown',updated_at=?,gateway_session_id=? WHERE id=?`, now.Format(time.RFC3339Nano), binding, start.SessionID); err != nil {
				t.Fatal(err)
			}
			reserveRecoveryPool(t, h.store, start.SessionID, "gateway_outcome_unknown")
			now = now.Add(unresolvedCreateTimeout - time.Nanosecond)
			h.svc.attestStaleCreates(context.Background())
			if fences != 0 {
				t.Fatal("fenced a quarantine before five minutes")
			}
			now = now.Add(time.Nanosecond)
			h.svc.attestStaleCreates(context.Background())
			if mode == "bound" {
				if lookups != 0 || fences != 0 {
					t.Fatal("bound row entered pre-bind attestation")
				}
			} else if fences != 1 {
				t.Fatalf("fence count at timeout=%d", fences)
			}
			if mode == "release" {
				assertCreateHold(t, h.store, start.SessionID, "failed", "unresolved_timeout", "", 1)
				return
			}
			source := start.SessionID
			if mode == "source_changed" {
				source = "next-source"
			}
			assertCreateHold(t, h.store, start.SessionID, "quarantined", "gateway_outcome_unknown", source, 0)
		})
	}
}

func TestZoeTimeoutRequiresDurableGatewayFence(t *testing.T) {
	now := time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC)
	var start callStartResponse
	fences := 0
	h := fencedCalls(t, &now, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			response := gatewayFixture(`{}`)
			response.StatusCode = 404
			return response, nil
		}
		fences++
		if fences == 1 {
			return nil, errors.New("private synthetic fence acknowledgement lost")
		}
		return fenceReply(start.GatewayRequestID, fencedNoLiveMedia)
	})
	start, _ = h.open(201)
	now = now.Add(unresolvedCreateTimeout)
	h.svc.attestStaleCreates(context.Background())
	assertCreateHold(t, h.store, start.SessionID, "allocating", "", start.SessionID, 0)
	h.svc.attestStaleCreates(context.Background())
	assertCreateHold(t, h.store, start.SessionID, "failed", "unresolved_timeout", "", 1)
}

func TestTerminalCallStartReplayCannotAllocate(t *testing.T) {
	for _, state := range []string{"failed", "ended", "quarantined"} {
		t.Run(state, func(t *testing.T) {
			now := time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC)
			h := fencedCalls(t, &now, func(*http.Request) (*http.Response, error) {
				t.Fatal("terminal replay called the gateway")
				return nil, nil
			})
			start, _ := h.open(201)
			if _, err := h.store.DB().Exec(`UPDATE voice_sessions SET state=? WHERE id=?`, state, start.SessionID); err != nil {
				t.Fatal(err)
			}
			h.request("POST", callRoute, h.runner, h.instance, `{"request_id":"attempt","offer_sdp":"v=0"}`, 409, "voice_request_terminal")
			var count int
			if err := h.store.DB().QueryRow(`SELECT count(*) FROM voice_sessions`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("replay allocated a row: count=%d err=%v", count, err)
			}
		})
	}
}

func TestTimeoutAndLateEstablishedOrderings(t *testing.T) {
	for _, establishFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("establish_first_%t", establishFirst), func(t *testing.T) {
			now := time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC)
			var h callHarness
			var start callStartResponse
			h = fencedCalls(t, &now, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					return creationLookup(start.GatewayRequestID, "unknown")
				}
				if establishFirst {
					h.post(callRoute+"/"+start.SessionID+"/established", callEstablishedRequest{callBinding: testBinding(start)}, 200, "")
				}
				return fenceReply(start.GatewayRequestID, fencedNoLiveMedia)
			})
			start, _ = h.open(201)
			now = now.Add(unresolvedCreateTimeout)
			h.svc.attestStaleCreates(context.Background())
			if establishFirst {
				assertCreateHold(t, h.store, start.SessionID, "active", "", start.SessionID, 0)
				return
			}
			assertCreateHold(t, h.store, start.SessionID, "failed", "unresolved_timeout", "", 1)
			response := h.request("POST", callRoute, h.runner, h.instance, `{"request_id":"next","offer_sdp":"v=0"}`, 201, "")
			var next callStartResponse
			if err := json.Unmarshal(response.Body.Bytes(), &next); err != nil || next.SessionID == "" || next.SessionID == start.SessionID {
				t.Fatalf("next call did not allocate: %v", err)
			}
			h.post(callRoute+"/"+start.SessionID+"/established", callEstablishedRequest{callBinding: testBinding(start)}, 409, "voice_generation_stale")
			assertCreateHold(t, h.store, start.SessionID, "failed", "unresolved_timeout", next.SessionID, 1)
		})
	}
}

func TestCreateFenceErrorsAreRateLimitedAndContentFree(t *testing.T) {
	now := time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC)
	var start callStartResponse
	h := fencedCalls(t, &now, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return creationLookup(start.GatewayRequestID, "unknown")
		}
		return nil, errors.New("private credential https://sensitive.invalid 203.0.113.40:54433")
	})
	start, _ = h.open(201)
	now = now.Add(unresolvedCreateTimeout)
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	for _, advance := range []time.Duration{0, 0, time.Minute - 1, 1} {
		now = now.Add(advance)
		h.svc.attestStaleCreates(context.Background())
	}
	got := logs.String()
	if strings.Count(got, "voice create unresolved: error_class=fence_transport") != 2 {
		t.Fatal("expected one fence error per minute")
	}
	for _, secret := range []string{start.SessionID, start.GatewayRequestID, "private credential", "sensitive.invalid", "203.0.113.40", "54433"} {
		if strings.Contains(got, secret) {
			t.Fatal("fence error log contains sensitive data")
		}
	}
}
