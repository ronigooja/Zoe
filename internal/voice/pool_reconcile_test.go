package voice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

func operatorPayload(id string) reconcilePoolPayload {
	return reconcilePoolPayload{SessionID: id, Generation: 1, Reason: "Verified out-of-band media termination", ConfirmedOutOfBandTermination: true}
}

func TestPoolReconcileRefusals(t *testing.T) {
	for _, name := range []string{"scope", "missing_session", "generation_zero", "unconfirmed", "short_reason", "long_reason", "allocating", "active", "ending", "other_active", "not_blocked", "wrong_source", "wrong_generation", "failed", "absent_source"} {
		t.Run(name, func(t *testing.T) {
			store, svc := newVoiceTestService(t, Config{})
			seedRecoverySession(t, store, "source", "quarantined")
			reserveRecoveryPool(t, store, "source", "upstream_termination_unverified")
			p := operatorPayload("source")
			scopes := []string{"voice:pool:reconcile"}
			status, code := 409, "voice_reconciliation_stale"
			exec := func(q string) {
				t.Helper()
				if _, err := store.DB().Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "scope":
				scopes, status, code = []string{"voice:open", "client:command"}, 403, "forbidden"
			case "missing_session":
				p.SessionID, status, code = "", 400, "invalid_request"
			case "generation_zero":
				p.Generation, status, code = 0, 400, "invalid_request"
			case "unconfirmed":
				p.ConfirmedOutOfBandTermination, status, code = false, 400, "invalid_request"
			case "short_reason":
				p.Reason, status, code = "  too short  ", 400, "invalid_request"
			case "long_reason":
				p.Reason, status, code = strings.Repeat("x", 501), 400, "invalid_request"
			case "allocating", "active", "ending":
				exec(`UPDATE voice_sessions SET state='` + name + `'`)
				code = "voice_sessions_active"
			case "other_active":
				seedRecoverySession(t, store, "other", "active")
				code = "voice_sessions_active"
			case "not_blocked":
				exec(`UPDATE voice_pool_state SET blocked=0`)
				code = "voice_pool_not_blocked"
			case "wrong_source":
				p.SessionID = "other"
			case "wrong_generation":
				p.Generation = 2
			case "failed":
				exec(`UPDATE voice_sessions SET state='failed'`)
			case "absent_source":
				exec(`DELETE FROM voice_sessions`)
			}
			tx, err := store.DB().BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, events, err := svc.commandReconcilePool(context.Background(), tx, control.CommandContext{Principal: contracts.Principal{SubjectID: "operator", Scopes: scopes}, AcceptedAt: time.Now()}, contracts.CommandRequest{Payload: mustJSON(p)})
			_ = tx.Rollback()
			var api *control.APIError
			if !errors.As(err, &api) || api.Status != status || api.Code != code || len(events) != 0 {
				t.Fatalf("error=%v events=%v, want %d %s", err, events, status, code)
			}
			var audits int
			if err := store.DB().QueryRow(`SELECT count(*) FROM voice_pool_reconciliations`).Scan(&audits); err != nil || audits != 0 {
				t.Fatalf("audits=%d err=%v", audits, err)
			}
		})
	}
}

func TestPoolReconcileHTTPAndReplay(t *testing.T) {
	for _, scope := range []string{"voice:pool:reconcile", "client:admin", "voice:open"} {
		for _, state := range []string{"quarantined", "ended"} {
			t.Run(scope+"/"+state, func(t *testing.T) {
				server, store, key := handoffHTTPServerWithScopes(t, Config{}, []string{"client:command", scope})
				if _, err := store.DB().Exec(`UPDATE voice_sessions SET state=? WHERE id='handoff-session'`, state); err != nil {
					t.Fatal(err)
				}
				reserveRecoveryPool(t, store, "handoff-session", "upstream_termination_unverified")
				for i := 0; i < 2; i++ {
					status, _, body := handoffCommand(t, server.Client(), server.URL, key, "attestation", "voice.pool.reconcile", operatorPayload("handoff-session"))
					if scope == "voice:open" {
						if status != 403 {
							t.Fatalf("status=%d body=%s", status, body)
						}
						continue
					}
					if status != 200 {
						t.Fatalf("status=%d body=%s", status, body)
					}
				}
				var blocked, audits, events, evidence int
				var actual string
				err := store.DB().QueryRow(`SELECT state,(SELECT blocked FROM voice_pool_state),(SELECT count(*) FROM voice_pool_reconciliations WHERE method='operator_attestation'),(SELECT count(*) FROM events WHERE event_type='voice.pool.reconciled'),(SELECT count(*) FROM voice_account_terminations) FROM voice_sessions`).Scan(&actual, &blocked, &audits, &events, &evidence)
				want, hold := 1, 0
				if scope == "voice:open" {
					want, hold = 0, 1
				}
				if err != nil || actual != state || blocked != hold || audits != want || events != want || evidence != 0 {
					t.Fatalf("state=%s blocked=%d audits=%d events=%d evidence=%d err=%v", actual, blocked, audits, events, evidence, err)
				}
			})
		}
	}
}

func TestPoolReconcileFencesLateEndAndLookup(t *testing.T) {
	cfg := callConfig()
	cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return gatewayFixture(`{"outcome":"unknown"}`), nil })}
	h := newCallHarness(t, nil, cfg)
	a, _ := h.open(201)
	b := testBinding(a)
	path := callRoute + "/" + a.SessionID
	h.post(path+"/established", callEstablishedRequest{callBinding: b}, 200, "")
	report := callEndedRequest{callBinding: b, Reason: "user_hangup", ClosedAt: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)}
	h.post(path+"/ended", report, 202, "")
	payload, _ := json.Marshal(operatorPayload(a.SessionID))
	tx, err := h.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = h.svc.commandReconcilePool(context.Background(), tx, control.CommandContext{Principal: contracts.Principal{SubjectID: "operator", Scopes: []string{"voice:pool:reconcile"}}, AcceptedAt: time.Now()}, contracts.CommandRequest{Payload: payload})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []bool{false, true} {
		if replacement {
			if _, err := h.store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,source_session_id='replacement',reason_code='active_session'`); err != nil {
				t.Fatal(err)
			}
		}
		h.post(path+"/ended", report, 409, "voice_generation_stale")
		report.TerminationEvidence = cleanEvidence(b, time.Now().UTC().Add(-time.Minute))
		h.post(path+"/ended", report, 409, "voice_generation_stale")
		_, err := h.svc.endFromLookup(context.Background(), a.SessionID, b, report.TerminationEvidence)
		var api *control.APIError
		if !errors.As(err, &api) || api.Code != "voice_generation_stale" {
			t.Fatalf("lookup=%v", err)
		}
		var state, source string
		var blocked bool
		var evidence int
		if err := h.store.DB().QueryRow(`SELECT state,(SELECT source_session_id FROM voice_pool_state),(SELECT blocked FROM voice_pool_state),(SELECT count(*) FROM voice_account_terminations) FROM voice_sessions WHERE id=?`, a.SessionID).Scan(&state, &source, &blocked, &evidence); err != nil {
			t.Fatal(err)
		}
		wantSource := ""
		if replacement {
			wantSource = "replacement"
		}
		if state != "quarantined" || source != wantSource || blocked != replacement || evidence != 0 {
			t.Fatalf("state=%s source=%s blocked=%v evidence=%d", state, source, blocked, evidence)
		}
	}
}
