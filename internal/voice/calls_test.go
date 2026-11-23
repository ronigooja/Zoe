package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

const callRoute = "/runner/v1/voice/calls"

type callHarness struct {
	t                   *testing.T
	store               *storage.Store
	svc                 *Service
	handler             http.Handler
	instance, runner    string
	other, wrong, scope string
}

func newCallHarness(t *testing.T, db *storage.Store, cfg Config) callHarness {
	t.Helper()
	ctx := context.Background()
	needsRegistration := db == nil
	if db == nil {
		var err error
		db, err = storage.Open(ctx, storage.Config{Path: ":memory:"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := storage.RegisterCoreMigrations(db); err != nil {
			t.Fatal(err)
		}
	}
	ctrl, err := control.NewServer(db, control.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctrl.Shutdown)
	var svc *Service
	if needsRegistration {
		ready := &ReadyHandle{}
		cfg.IngestEnabled, cfg.Ready = true, ready
		if err := Register(ctrl, db, cfg); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		svc = ready.service
	} else {
		svc = newService(db.DB(), cfg)
		svc.uiEventServer = ctrl
		t.Cleanup(svc.Stop)
		if err := ctrl.RegisterRunnerRoutes(svc.registerCallRoutes); err != nil {
			t.Fatal(err)
		}
	}
	key := func() string {
		key, err := control.GenerateInstanceKey()
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	h := callHarness{t: t, store: db, svc: svc, instance: key(), runner: key(), other: key(), wrong: key(), scope: key()}
	if _, _, err := control.InitializeInstanceAuth(ctx, db, h.instance); err != nil {
		t.Fatal(err)
	}
	for _, auth := range []struct{ token, id, scope string }{
		{h.runner, "media", voiceRunnerScope}, {h.other, "other", voiceRunnerScope},
		{h.wrong, "wrong", "runner:observe"}, {h.scope, "scope", "runner:claim"},
	} {
		if err := ctrl.RegisterRunnerCredential(ctx, auth.token, auth.id, []string{auth.scope}); err != nil {
			t.Fatal(err)
		}
	}
	h.handler = ctrl.Handler()
	return h
}

func (h callHarness) request(method, path, bearer, instance, body string, status int, code string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if instance != "" {
		r.Header.Set("X-Zoe-Instance-Key", instance)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != status || (code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`)) {
		h.t.Fatalf("%s %s: status=%d body=%s, want %d %s", method, path, w.Code, w.Body.String(), status, code)
	}
	if instance != "" && strings.Contains(w.Body.String(), instance) {
		h.t.Fatal("echoed forwarded instance key")
	}
	return w
}

func (h callHarness) post(path string, payload any, status int, code string) *httptest.ResponseRecorder {
	h.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.request("POST", path, h.runner, "", string(body), status, code)
}

func (h callHarness) open(status int) (callStartResponse, string) {
	h.t.Helper()
	w := h.request("POST", callRoute, h.runner, h.instance, `{"request_id":"attempt","offer_sdp":"v=0"}`, status, "")
	var result callStartResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !validBindingID(result.SessionID) || result.Generation < 1 || !validBindingID(result.GatewayRequestID) || result.OpeningContext == nil || result.OpeningContext.Items == nil || result.OpeningContext.Kind != "full" || result.OpeningContextHash != openingContextHash(result.OpeningContext) {
		h.t.Fatalf("invalid start response: %s (%v)", w.Body.String(), err)
	}
	return result, w.Body.String()
}

func callConfig() Config {
	return Config{GatewayURL: "https://gateway.internal/base", GatewayAPIKey: "deployment-key", GatewayDialOrigin: "https://gateway.example:8443"}
}

func TestCallStartCredentialsAndRetiredRoutes(t *testing.T) {
	h := newCallHarness(t, nil, callConfig())
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	const body = `{"request_id":"attempt","offer_sdp":"v=0"}`
	for _, key := range []string{"", "invalid-private-token", "Bearer " + h.instance} {
		w := h.request("POST", callRoute, h.runner, key, body, 401, "unauthenticated")
		if !strings.Contains(w.Body.String(), `"credential":"instance_key"`) {
			t.Fatal(w.Body.String())
		}
	}
	w := h.request("POST", callRoute, "", h.instance, body, 401, "unauthenticated")
	if strings.Contains(w.Body.String(), "instance_key") {
		t.Fatal("runner failure was attributed to instance key")
	}
	for _, bad := range []string{h.scope, h.wrong} {
		h.request("POST", callRoute, bad, h.instance, body, 403, "forbidden")
	}
	newKey, err := control.GenerateInstanceKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := control.RotateInstanceKey(context.Background(), h.store, newKey); err != nil {
		t.Fatal(err)
	}
	w = h.request("POST", callRoute, h.runner, h.instance, body, 401, "unauthenticated")
	if !strings.Contains(w.Body.String(), `"credential":"instance_key"`) {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(logs.String(), h.instance) || strings.Contains(logs.String(), "invalid-private-token") {
		t.Fatal("forwarded key reached the logs")
	}
	h.instance = newKey
	started, _ := h.open(201)
	var saved string
	if err := h.store.DB().QueryRow(`SELECT request_id || call_start_response FROM voice_sessions WHERE id=?`, started.SessionID).Scan(&saved); err != nil || strings.Contains(saved, newKey) {
		t.Fatalf("start persistence check failed: %v", err)
	}
	h.request("GET", "/v1/voice/sessions/"+started.SessionID, newKey, "", "", 200, "")
	h.request("GET", "/v1/voice/pool/status", newKey, "", "", 200, "")
	h.request("POST", "/v1/voice/sessions", newKey, "", body, 404, "")
	for _, route := range []string{"connected", "heartbeat", "hangup"} {
		h.request("POST", "/v1/voice/sessions/"+started.SessionID+"/"+route, newKey, "", `{}`, 404, "")
	}
	for _, route := range []string{"/runner/v1/claims", callRoute + "/", callRoute + "/s/ended/extra", callRoute + "/s/heartbeat"} {
		h.request("POST", route, h.runner, "", `{}`, 403, "forbidden")
	}
	h.request("GET", callRoute, h.runner, "", "", 403, "forbidden")
	h.request("POST", "/runner/v1/voice/turns", h.runner, "", "{}", 400, "invalid_request")
}

func TestCallStartSettingsReplayAndConflicts(t *testing.T) {
	cfg := callConfig()
	cfg.InstructionText = "instructions 中文"
	cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/base/v1/voice/config" || r.Header.Get("Authorization") != "Bearer deployment-key" || r.Header.Get("X-Zoe-Instance-Key") != "" {
			t.Errorf("unexpected gateway config request %s %s", r.Method, r.URL)
		}
		return gatewayFixture(`{"version":"v1","defaults":{},"webrtc":{},"voices":[{"id":"cove"}],"voice_modes":[{"id":"wingman"}],"languages":[{"id":"auto"}]}`), nil
	})}
	h := newCallHarness(t, nil, cfg)
	for _, body := range []string{`{}`, `{"request_id":" ","offer_sdp":"sdp"}`, `{"request_id":"bad","offer_sdp":" "}`, `{"request_id":"bad","offer_sdp":"x","extra":true}`, `{"request_id":"bad","offer_sdp":"x","voice":"unsupported"}`, `{"request_id":"bad","offer_sdp":"x","voice_mode":"unsupported"}`, `{"request_id":"bad","offer_sdp":"x","language_code":"unsupported"}`, `{"request_id":"bad","offer_sdp":"` + strings.Repeat("x", 1<<20+1) + `"}`} {
		h.request("POST", callRoute, h.runner, h.instance, body, 400, "invalid_request")
	}
	w := h.request("GET", "/v1/voice/config", h.instance, "", "", 200, "")
	if !strings.Contains(w.Body.String(), `"gateway_dial_origin":"https://gateway.example:8443"`) {
		t.Fatal(w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"webrtc":{}`) || strings.Contains(w.Body.String(), `"ice_servers"`) {
		t.Fatal(w.Body.String())
	}
	a, first := h.open(201)
	if a.Instructions != cfg.InstructionText {
		t.Fatal("instructions not returned")
	}
	if _, replay := h.open(200); first != replay {
		t.Fatal("start response changed on replay")
	}
	h.request("POST", callRoute, h.runner, h.instance, `{"request_id":"attempt","offer_sdp":"changed"}`, 409, "idempotency_conflict")
	h.request("POST", callRoute, h.runner, h.instance, `{"request_id":"next","offer_sdp":"v=0"}`, 409, "voice_lease_busy")
	reserveRecoveryPool(t, h.store, a.SessionID, "upstream_termination_unverified")
	h.request("POST", callRoute, h.runner, h.instance, `{"request_id":"next","offer_sdp":"v=0"}`, 503, "voice_pool_quarantined")
	if _, replay := h.open(200); first != replay {
		t.Fatal("quarantine overrode replay")
	}
	for _, invalid := range []string{"", "http://host", "https://host/", "https://user@host", "https://host?", "https://host#", "https://host:65536"} {
		if validDialOrigin(invalid) != "" {
			t.Errorf("accepted invalid origin %q", invalid)
		}
	}
	none := newCallHarness(t, nil, Config{})
	none.request("GET", "/v1/voice/config", none.instance, "", "", 503, "voice_unavailable")
	limit := newCallHarness(t, nil, callConfig())
	limit.request("POST", callRoute, limit.runner, limit.instance, `{"request_id":"full","offer_sdp":"v`+strings.Repeat(`\u000a`, (1<<20)-1)+`"}`, 201, "")
}

func TestVoiceConfigGatewayFieldStrictness(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"nested fields pass through", `{"version":"v1","webrtc":{"ice_servers":[]}}`, 200, ""},
		{"unknown top-level field rejected", `{"version":"v1","webrtc":{},"extra":true}`, 502, "voice_config_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := callConfig()
			cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return gatewayFixture(tc.body), nil
			})}
			h := newCallHarness(t, nil, cfg)
			w := h.request("GET", "/v1/voice/config", h.instance, "", "", tc.status, tc.code)
			if tc.status == 200 && !strings.Contains(w.Body.String(), `"ice_servers":[]`) {
				t.Fatal(w.Body.String())
			}
		})
	}
}

func gatewayFixture(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func testBinding(a callStartResponse) callBinding {
	return callBinding{a.Generation, "media/+opaque", 3, "lease+& /opaque", 4}
}

func TestCallEstablishedAtomicReceiptAndReplay(t *testing.T) {
	for _, tc := range []struct{ name, want string }{{"strict", "consumed"}, {"missing", "reserved"}, {"string-silent", ""}, {"wrong-hash", ""}, {"wrong-version", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			r := checkpointRig(t)
			r.setup(t, "original")
			r.svc.consumeResultEvents(context.Background())
			r.exec(t, `UPDATE voice_result_deliveries SET subject_id='owner'; UPDATE tasks SET owner_subject_id='owner';`)
			h := newCallHarness(t, r.store, callConfig())
			a, raw := h.open(201)
			if len(a.OpeningContext.Items) != 1 || !strings.Contains(a.OpeningContext.Items[0].Text, "original") {
				t.Fatalf("pending result not included: %s", raw)
			}
			if _, replay := h.open(200); raw != replay {
				t.Fatal("opening manifest changed on replay")
			}
			var reserved int
			if err := h.store.DB().QueryRow(`SELECT count(*) FROM voice_result_opening WHERE session_id=? AND state='reserved'`, a.SessionID).Scan(&reserved); err != nil || reserved != 1 {
				t.Fatalf("replay reservation count=%d err=%v", reserved, err)
			}
			path := callRoute + "/" + a.SessionID + "/established"
			req := callEstablishedRequest{testBinding(a), appliedTestOpening(a.OpeningContext)}
			switch tc.name {
			case "strict":
				r.exec(t, `CREATE TRIGGER fail_receipt BEFORE INSERT ON voice_context_events BEGIN SELECT RAISE(ABORT, 'fixture'); END;`)
				h.post(path, req, 500, "internal")
				var bound, state, ack string
				var media, lease int64
				if err := h.store.DB().QueryRow(`SELECT state,gateway_session_id,gateway_media_generation,account_lease_generation,call_established_hash FROM voice_sessions WHERE id=?`, a.SessionID).Scan(&state, &bound, &media, &lease, &ack); err != nil || state != "allocating" || bound != "" || media != 0 || lease != 0 || ack != "" {
					t.Fatalf("partial bind after rollback state=%s bound=%s media=%d lease=%d ack=%s err=%v", state, bound, media, lease, ack, err)
				}
				r.exec(t, `DROP TRIGGER fail_receipt`)
			case "missing":
				req.OpeningContext = nil
			case "string-silent":
				req.OpeningContext.Silent = "true"
			case "wrong-hash":
				req.OpeningContext.ContentHash = "wrong"
			case "wrong-version":
				req.OpeningContext.Version++
			}
			stale := req
			stale.Generation++
			h.post(path, stale, 409, "voice_generation_stale")
			body, _ := json.Marshal(req)
			h.request("POST", path, h.other, "", string(body), 403, "forbidden")
			for i := 0; i < 2; i++ {
				h.post(path, req, 200, "")
			}
			changed := req
			changed.OpeningContext = &openingContextResult{Applied: false}
			h.post(path, changed, 409, "idempotency_conflict")
			var actual, state, media, lease string
			var n int
			wantEvents := 0
			if tc.want == "consumed" {
				wantEvents = 1
			}
			if err := h.store.DB().QueryRow(`SELECT s.state,s.gateway_session_id,s.account_lease_id,coalesce(o.state,''),(SELECT count(*) FROM voice_context_events) FROM voice_sessions s LEFT JOIN voice_result_opening o ON o.session_id=s.id WHERE s.id=?`, a.SessionID).Scan(&state, &media, &lease, &actual, &n); err != nil || state != "active" || media != req.GatewaySessionID || lease != req.AccountLeaseID || actual != tc.want || n != wantEvents {
				t.Fatalf("state=%s media=%s lease=%s reservation=%s events=%d err=%v", state, media, lease, actual, n, err)
			}
		})
	}
}

func cleanEvidence(b callBinding, at time.Time) *terminationEvidence {
	zero := 0
	return &terminationEvidence{GatewaySessionID: b.GatewaySessionID, MediaGeneration: b.MediaGeneration, AccountLeaseID: b.AccountLeaseID, AccountLeaseGeneration: b.AccountLeaseGeneration, AccountID: "private-opaque-alias", Terminated: true, EvidenceID: testCreationEvidenceID, ObservedAt: at.Format(time.RFC3339Nano), Reason: "gateway-close", PeerAClosed: true, PeerBClosed: true, DataChannelsClosed: true, TunnelsClosed: true, RetiredLegs: &zero, RetiredLegsUnclean: &zero}
}

func TestCallEndedEvidenceAndReconciliation(t *testing.T) {
	for _, mode := range []string{"verified", "unverified", "supplement", "correct-invalid", "lost-report", "lookup-on-report"} {
		t.Run(mode, func(t *testing.T) {
			cfg := callConfig()
			now := time.Date(2026, 11, 1, 1, 2, 3, 0, time.UTC)
			cfg.Now = func() time.Time { return now }
			lookup := "not_yet"
			var e *terminationEvidence
			cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				q := r.URL.Query()
				if r.Method != "GET" || r.URL.EscapedPath() != "/base/v1/voice/sessions/media%2F+opaque/termination-evidence" || r.Header.Get("Authorization") != "Bearer deployment-key" || q.Get("media_generation") != "3" || q.Get("account_lease_id") != e.AccountLeaseID || q.Get("account_lease_generation") != "4" {
					t.Errorf("unexpected evidence request: %s %s", r.Method, r.URL)
				}
				if lookup == "released" {
					wire, _ := json.Marshal(e)
					return gatewayFixture(fmt.Sprintf(`{"outcome":"released","termination_evidence":%s}`, wire)), nil
				}
				return gatewayFixture(fmt.Sprintf(`{"outcome":%q}`, lookup)), nil
			})}
			h := newCallHarness(t, nil, cfg)
			a, _ := h.open(201)
			p := callRoute + "/" + a.SessionID
			bind := callEstablishedRequest{callBinding: testBinding(a)}
			h.post(p+"/established", bind, 200, "")
			observed := now.Add(-time.Minute)
			closed := now.Add(-30 * time.Second).Format(time.RFC3339Nano)
			e = cleanEvidence(bind.callBinding, observed)
			req := callEndedRequest{callBinding: bind.callBinding, Reason: "user_hangup", ClosedAt: closed}
			if mode == "verified" {
				silence := req
				silence.Reason = "silence_timeout"
				h.post(p+"/ended", silence, 400, "invalid_request")
			}
			stale := req
			stale.MediaGeneration++
			h.post(p+"/ended", stale, 409, "voice_generation_stale")
			if mode == "lost-report" {
				h.svc.reconcileBoundCalls(context.Background())
				var active string
				if err := h.store.DB().QueryRow(`SELECT state FROM voice_sessions WHERE id=?`, a.SessionID).Scan(&active); err != nil || active != "active" {
					t.Fatalf("not_yet changed active state: %s %v", active, err)
				}
				lookup = "released"
				h.svc.reconcileBoundCalls(context.Background())
			}
			if mode == "unverified" || mode == "correct-invalid" {
				bad := *e
				bad.TunnelsClosed = false
				req.TerminationEvidence = &bad
				lookup = "unknown"
			}
			if mode == "verified" || mode == "lost-report" {
				req.TerminationEvidence = e
			}
			if mode == "lookup-on-report" {
				lookup = "released"
			}
			status := 200
			if mode == "unverified" || mode == "supplement" || mode == "correct-invalid" {
				status = 202
			}
			first := h.post(p+"/ended", req, status, "").Body.String()
			if again := h.post(p+"/ended", req, status, "").Body.String(); first != again {
				t.Fatal("exact replay changed result")
			}
			without := req
			without.TerminationEvidence = nil
			if mode == "unverified" || mode == "correct-invalid" {
				if retry := h.post(p+"/ended", without, 202, "").Body.String(); first != retry {
					t.Fatal("retry without invalid evidence changed quarantine result")
				}
			}
			if mode == "supplement" || mode == "correct-invalid" {
				req.TerminationEvidence = e
				verified := h.post(p+"/ended", req, 200, "").Body.String()
				if replay := h.post(p+"/ended", req, 200, "").Body.String(); verified != replay {
					t.Fatal("replaying verified supplement changed result")
				}
				h.post(p+"/ended", without, 200, "")
				status = 200
			}
			if mode == "verified" {
				h.post(p+"/ended", without, 409, "idempotency_conflict")
			}
			if status == 200 {
				other := *e
				other.ObservedAt = observed.Add(-time.Second).Format(time.RFC3339Nano)
				changed := req
				changed.TerminationEvidence = &other
				h.post(p+"/ended", changed, 409, "idempotency_conflict")
			}
			changed := req
			changed.Reason = "failure"
			h.post(p+"/ended", changed, 409, "idempotency_conflict")
			changed = req
			changed.ClosedAt = observed.Format(time.RFC3339Nano)
			h.post(p+"/ended", changed, 409, "idempotency_conflict")
			h.post(p+"/established", bind, 200, "")
			var state, ended, record string
			var blocked bool
			var evidenceCount int
			err := h.store.DB().QueryRow(`SELECT state,coalesce(ended_at,''),(SELECT blocked FROM voice_pool_state),(SELECT count(*) FROM voice_account_terminations),coalesce((SELECT gateway_evidence FROM voice_account_terminations),'') FROM voice_sessions WHERE id=?`, a.SessionID).Scan(&state, &ended, &blocked, &evidenceCount, &record)
			wantState, wantEnded, wantCount := "ended", closed, 1
			if status == 202 {
				wantState, wantEnded, wantCount = "quarantined", "", 0
			} else if mode == "lost-report" {
				wantEnded = e.ObservedAt
			}
			if err != nil || state != wantState || ended != wantEnded || evidenceCount != wantCount || blocked != (status == 202) || strings.Contains(record, e.AccountID) {
				t.Fatalf("state=%s ended=%s blocked=%v evidence=%d err=%v", state, ended, blocked, evidenceCount, err)
			}
			if status == 200 {
				h.post(p+"/ended", stale, 409, "voice_generation_stale")
				reserveRecoveryPool(t, h.store, "replacement", "negotiation_in_progress")
				w := h.post(p+"/ended", req, 200, "")
				if !strings.Contains(w.Body.String(), `"pool_blocked":true`) {
					t.Fatal("old reply cleared replacement fence")
				}
			}
		})
	}
}

func TestCallEndedFutureSkewAndReceiveTime(t *testing.T) {
	for _, mode := range []string{"direct", "supplement", "lookup"} {
		t.Run(mode, func(t *testing.T) {
			cfg := callConfig()
			receivedAt := time.Date(2026, 11, 1, 1, 2, 3, 0, time.UTC)
			now := receivedAt
			cfg.Now = func() time.Time { return now }
			var evidence *terminationEvidence
			cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if mode == "lookup" && now.After(receivedAt) {
					wire, _ := json.Marshal(evidence)
					return gatewayFixture(fmt.Sprintf(`{"outcome":"released","termination_evidence":%s}`, wire)), nil
				}
				return gatewayFixture(`{"outcome":"not_yet"}`), nil
			})}
			h := newCallHarness(t, nil, cfg)
			h.svc.Stop() // Drive reconciliation explicitly with the test clock.
			a, _ := h.open(201)
			p := callRoute + "/" + a.SessionID
			binding := testBinding(a)
			h.post(p+"/established", callEstablishedRequest{callBinding: binding}, 200, "")
			evidence = cleanEvidence(binding, now)
			req := callEndedRequest{callBinding: binding, Reason: "user_hangup", ClosedAt: now.Add(5*time.Minute + time.Nanosecond).Format(time.RFC3339Nano), TerminationEvidence: evidence}
			h.post(p+"/ended", req, 400, "invalid_request")
			req.ClosedAt = now.Add(5 * time.Minute).Format(time.RFC3339Nano)
			if mode != "direct" {
				req.TerminationEvidence = nil
				h.post(p+"/ended", req, 202, "")
				now = receivedAt.Add(6 * time.Minute)
				if mode == "lookup" {
					h.svc.reconcileBoundCalls(context.Background())
				} else {
					req.TerminationEvidence = evidence
				}
			}
			for i := 0; i < 2; i++ {
				h.post(p+"/ended", req, 200, "")
			}
			var ended, reported string
			if err := h.store.DB().QueryRow(`SELECT ended_at,call_closed_at FROM voice_sessions WHERE id=?`, a.SessionID).Scan(&ended, &reported); err != nil || ended != receivedAt.Format(time.RFC3339Nano) || reported != req.ClosedAt {
				t.Fatalf("ended_at=%s call_closed_at=%s err=%v", ended, reported, err)
			}
		})
	}
}

func TestEvidenceRejectsUntrustedFacts(t *testing.T) {
	now := time.Date(2026, 11, 1, 1, 2, 3, 0, time.UTC)
	b := callBinding{1, "gateway", 3, "lease", 4}
	c := lifecycleCall{callBinding: b}
	for name, change := range map[string]func(*terminationEvidence){
		"gateway":          func(e *terminationEvidence) { e.GatewaySessionID = "other" },
		"media generation": func(e *terminationEvidence) { e.MediaGeneration++ },
		"lease":            func(e *terminationEvidence) { e.AccountLeaseID = "other" },
		"lease generation": func(e *terminationEvidence) { e.AccountLeaseGeneration++ },
		"alias":            func(e *terminationEvidence) { e.AccountID = " " },
		"terminated":       func(e *terminationEvidence) { e.Terminated = false },
		"id":               func(e *terminationEvidence) { e.EvidenceID = "ev_0123" },
		"future":           func(e *terminationEvidence) { e.ObservedAt = now.Add(time.Second).Format(time.RFC3339) },
		"missing time":     func(e *terminationEvidence) { e.ObservedAt = "" },
		"reason":           func(e *terminationEvidence) { e.Reason = "" },
		"peer a":           func(e *terminationEvidence) { e.PeerAClosed = false },
		"peer b":           func(e *terminationEvidence) { e.PeerBClosed = false },
		"data channels":    func(e *terminationEvidence) { e.DataChannelsClosed = false },
		"tunnels":          func(e *terminationEvidence) { e.TunnelsClosed = false },
		"missing retired":  func(e *terminationEvidence) { e.RetiredLegs = nil },
		"unclean retired":  func(e *terminationEvidence) { *e.RetiredLegsUnclean = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			e := cleanEvidence(b, now)
			change(e)
			if e.matches(c, now) {
				t.Fatal("accepted incomplete, unclean or conflicting evidence")
			}
		})
	}
}

func TestReconciliationLogsStuckEndingWithoutMutation(t *testing.T) {
	cfg := callConfig()
	cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return gatewayFixture(`{"outcome":"not_yet"}`), nil
	})}
	h := newCallHarness(t, nil, cfg)
	a, _ := h.open(201)
	b := testBinding(a)
	h.post(callRoute+"/"+a.SessionID+"/established", callEstablishedRequest{callBinding: b}, 200, "")
	if _, err := h.store.DB().Exec(`UPDATE voice_sessions SET state='ending' WHERE id=?`, a.SessionID); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	h.svc.reconcileBoundCalls(context.Background())
	h.post(callRoute+"/missing/ended", callEndedRequest{callBinding: b, Reason: "failure", ClosedAt: time.Now().UTC().Format(time.RFC3339Nano)}, 404, "not_found")
	if !strings.Contains(logs.String(), "termination unresolved: state=ending outcome=not_yet") || !strings.Contains(logs.String(), "references an unknown session") {
		t.Fatal("missing explicit lifecycle fault logs")
	}
	var state string
	var blocked bool
	if err := h.store.DB().QueryRow(`SELECT state,(SELECT blocked FROM voice_pool_state) FROM voice_sessions WHERE id=?`, a.SessionID).Scan(&state, &blocked); err != nil || state != "ending" || !blocked {
		t.Fatalf("lookup changed stuck ending: %s blocked=%v err=%v", state, blocked, err)
	}
}

func TestReconciliationLogsWithoutConfiguredGateway(t *testing.T) {
	h := newCallHarness(t, nil, callConfig())
	a, _ := h.open(201)
	h.post(callRoute+"/"+a.SessionID+"/established", callEstablishedRequest{callBinding: testBinding(a)}, 200, "")
	now := time.Now().UTC()
	if _, err := h.store.DB().Exec(`UPDATE voice_sessions SET state='ending' WHERE id=?`, a.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().Exec(`INSERT INTO voice_sessions(id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at,gateway_request_id,lifecycle_runner_id) VALUES('allocating-call','other','device','request',X'01',1,'allocating',?,?,'private-gateway-request','media')`,
		now.Add(-3*time.Minute).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	cfg := Config{GatewayURL: "http://invalid.example", GatewayAPIKey: "private-key", Now: func() time.Time { return now }, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("called gateway without valid configuration")
		return nil, nil
	})}}
	svc := newService(h.store.DB(), cfg)
	t.Cleanup(svc.Stop)
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	pass := func() {
		svc.reconcileBoundCalls(context.Background())
		svc.attestStaleCreates(context.Background())
	}
	pass()
	pass()
	check := func(want int) {
		t.Helper()
		got := logs.String()
		if strings.Count(got, "level=error voice session "+a.SessionID+" termination unresolved: state=ending outcome=unknown") != want ||
			strings.Count(got, "level=error voice session allocating-call establishment unresolved: outcome=unknown") != want ||
			strings.Contains(got, "private-") || strings.Contains(got, "invalid.example") {
			t.Fatal("missing, repeated or unsafe reconciliation logs:", got)
		}
	}
	check(1)
	now = now.Add(time.Minute)
	pass()
	check(2)
}
