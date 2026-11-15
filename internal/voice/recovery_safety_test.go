package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/storage"
)

// awaitRecoveryTest uses a generous deadlock watchdog, not a latency assertion.
func awaitRecoveryTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("recovery test synchronization timed out")
		var zero T
		return zero
	}
}

func seedRecoverySession(t *testing.T, store *storage.Store, id, state string) {
	t.Helper()
	at := time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,gateway_request_id,created_at,updated_at) VALUES(?,'default','owner','device',?,X'01',1,?,?,?,?)`, id, id, state, id+"-request", at, at); err != nil {
		t.Fatal(err)
	}
}

func reserveRecoveryPool(t *testing.T, store *storage.Store, id, reason string) {
	t.Helper()
	if _, err := store.DB().Exec(`UPDATE voice_pool_state SET blocked=1,source_session_id=?,reason_code=?`, id, reason); err != nil {
		t.Fatal(err)
	}
}

func TestFailedGatewayCreateAfterReaperPreservesNewPoolSource(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
	seedRecoverySession(t, store, "A", "allocating")
	reserveRecoveryPool(t, store, "A", "negotiation_in_progress")
	svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"request_id":"A-request","outcome":"never_established"}`))}, nil
	})}
	svc.attestStaleCreates(context.Background())
	seedRecoverySession(t, store, "B", "allocating")
	reserveRecoveryPool(t, store, "B", "negotiation_in_progress")
	var before, after string
	if err := store.DB().QueryRow(`SELECT updated_at FROM voice_sessions WHERE id='A'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	_ = svc.failedGatewayCreate(context.Background(), "A", "A-request")
	var source, reason, state string
	var blocked, audit int
	if err := store.DB().QueryRow(`SELECT source_session_id,reason_code,blocked,(SELECT state FROM voice_sessions WHERE id='B'),(SELECT updated_at FROM voice_sessions WHERE id='A'),(SELECT COUNT(*) FROM voice_pool_reconciliations) FROM voice_pool_state`).Scan(&source, &reason, &blocked, &state, &after, &audit); err != nil {
		t.Fatal(err)
	}
	if source != "B" || reason != "negotiation_in_progress" || blocked != 1 || state != "allocating" || before != after || audit != 1 {
		t.Fatalf("source=%s reason=%s blocked=%d B=%s changedA=%v audit=%d", source, reason, blocked, state, before != after, audit)
	}
}

func TestMarkSessionNoOpFencesAllCallers(t *testing.T) {
	for _, target := range []string{"active", "failed", "quarantined"} {
		for _, fence := range []string{"state", "generation", "pool_source", "terminal", "terminal_expected"} {
			t.Run(target+"/"+fence, func(t *testing.T) {
				store, svc := newVoiceTestService(t, Config{})
				seedRecoverySession(t, store, "A", "allocating")
				reserveRecoveryPool(t, store, "A", "negotiation_in_progress")
				generation, expected := int64(1), "allocating"
				switch fence {
				case "state":
					expected = "ending"
				case "generation":
					generation = 2
				case "pool_source":
					reserveRecoveryPool(t, store, "B", "active_session")
				case "terminal", "terminal_expected":
					if _, err := store.DB().Exec(`UPDATE voice_sessions SET state='failed'`); err != nil {
						t.Fatal(err)
					}
					if fence == "terminal_expected" {
						expected = "failed"
					}
				}
				var before, after string
				const snapshot = `SELECT json_array(s.state,s.generation,s.last_error_code,s.updated_at,s.gateway_session_id,p.blocked,p.source_session_id,p.reason_code,p.updated_at) FROM voice_sessions s CROSS JOIN voice_pool_state p WHERE s.id='A'`
				if err := store.DB().QueryRow(snapshot).Scan(&before); err != nil {
					t.Fatal(err)
				}
				_ = svc.markSession(context.Background(), "A", generation, expected, target, "gateway_release_unknown", []byte("answer"), "media")
				if err := store.DB().QueryRow(snapshot).Scan(&after); err != nil {
					t.Fatal(err)
				}
				if before != after {
					t.Fatalf("stale transition changed data: %s -> %s", before, after)
				}
			})
		}
	}
}

func TestGatewayCreateResolutionGenerationStateFences(t *testing.T) {
	for _, fence := range []string{"generation", "state"} {
		t.Run(fence, func(t *testing.T) {
			store, svc := newVoiceTestService(t, Config{})
			seedRecoverySession(t, store, "A", "allocating")
			reserveRecoveryPool(t, store, "A", "negotiation_in_progress")
			generation, state := int64(1), "allocating"
			if fence == "generation" {
				generation = 2
			} else {
				state = "quarantined"
			}
			if err := svc.resolveGatewayCreateOutcome(context.Background(), "A", generation, state, "A-request", gatewayCreationOutcome{Outcome: "released", EvidenceID: testCreationEvidenceID}); err != nil {
				t.Fatal(err)
			}
			var actual string
			var blocked, audit int
			if err := store.DB().QueryRow(`SELECT state,(SELECT blocked FROM voice_pool_state),(SELECT COUNT(*) FROM voice_pool_reconciliations) FROM voice_sessions`).Scan(&actual, &blocked, &audit); err != nil {
				t.Fatal(err)
			}
			if actual != "allocating" || blocked != 1 || audit != 0 {
				t.Fatalf("state=%s blocked=%d audit=%d", actual, blocked, audit)
			}
		})
	}
}

func TestQueryGatewayOutcomeHonorsCancellationAndTotalBudget(t *testing.T) {
	for _, kind := range []string{"already_cancelled", "cancel_during_wait", "total_budget"} {
		t.Run(kind, func(t *testing.T) {
			_, svc := newVoiceTestService(t, Config{GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			var transportErr error
			var remaining time.Duration
			svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				deadline, ok := r.Context().Deadline()
				if !ok {
					return nil, fmt.Errorf("outcome request has no deadline")
				}
				remaining = time.Until(deadline)
				if kind == "cancel_during_wait" {
					cancel()
				}
				<-r.Context().Done()
				transportErr = r.Context().Err()
				return nil, transportErr
			})}
			if kind == "already_cancelled" {
				cancel()
			}
			done := make(chan gatewayCreationOutcome, 1)
			go func() { done <- svc.queryGatewayOutcome(ctx, "req") }()
			got := awaitRecoveryTest(t, done)
			if got.Outcome != "" || (kind == "already_cancelled" && calls != 0) {
				t.Fatalf("outcome=%+v calls=%d", got, calls)
			}
			if kind == "cancel_during_wait" && (calls != 1 || transportErr != context.Canceled) {
				t.Fatalf("cancellation not propagated: calls=%d transport=%v", calls, transportErr)
			}
			if kind == "total_budget" && (calls == 0 || transportErr != context.DeadlineExceeded || remaining > 800*time.Millisecond) {
				t.Fatalf("deadline not enforced: calls=%d transport=%v remaining=%s", calls, transportErr, remaining)
			}
		})
	}
}

func TestNextStartRecoveryHonorsRequestCancellation(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{GatewayURL: "https://gateway.test", GatewayAPIKey: "key"})
	seedRecoverySession(t, store, "A", "quarantined")
	reserveRecoveryPool(t, store, "A", "gateway_outcome_unknown")
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET last_error_code='gateway_outcome_unknown'`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	observed := make(chan error, 1)
	var calls atomic.Int64
	svc.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-r.Context().Done()
		observed <- r.Context().Err()
		return nil, r.Context().Err()
	})}
	done := make(chan error, 1)
	go func() { done <- svc.recoverQuarantinedCreate(ctx) }()
	awaitRecoveryTest(t, entered)
	cancel()
	if err := awaitRecoveryTest(t, done); err != nil {
		t.Fatal(err)
	}
	if err := awaitRecoveryTest(t, observed); err != context.Canceled || calls.Load() != 1 {
		t.Fatalf("request cancellation not propagated: transport=%v calls=%d", err, calls.Load())
	}
	var state string
	var blocked int
	if err := store.DB().QueryRow(`SELECT state,(SELECT blocked FROM voice_pool_state) FROM voice_sessions`).Scan(&state, &blocked); err != nil || state != "quarantined" || blocked != 1 {
		t.Fatalf("state=%s blocked=%d err=%v", state, blocked, err)
	}
}

func TestPoolStatusPollRecoversAndRateLimitsConcurrentRequests(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	var calls atomic.Int64
	var released atomic.Bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		outcome := "unknown"
		if released.Load() {
			outcome = "released"
		}
		body := fmt.Sprintf(`{"request_id":"poll-request","outcome":%q,"evidence_id":%q}`, outcome, testCreationEvidenceID)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	server, store, key := handoffHTTPServerWithConfig(t, Config{GatewayURL: "https://gateway.test", GatewayAPIKey: "key", HTTPClient: client, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	seedRecoverySession(t, store, "poll", "quarantined")
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET last_error_code='gateway_outcome_unknown' WHERE id='poll'`); err != nil {
		t.Fatal(err)
	}
	reserveRecoveryPool(t, store, "poll", "gateway_outcome_unknown")
	poll := func() bool {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/voice/pool/status", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Error(err)
			return true
		}
		defer response.Body.Close()
		var result struct {
			Blocked bool `json:"blocked"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil || response.StatusCode != http.StatusOK {
			t.Errorf("status=%d decode=%v", response.StatusCode, err)
		}
		return result.Blocked
	}
	if !poll() || calls.Load() != 1 {
		t.Fatal("first unknown status lookup did not retain quarantine")
	}
	released.Store(true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); poll() }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("cooldown lookup count=%d", calls.Load())
	}
	clock.Add(int64(10 * time.Second))
	if poll() || calls.Load() != 2 {
		t.Fatalf("healed poll blocked or calls=%d", calls.Load())
	}
	var reason string
	if err := store.DB().QueryRow(`SELECT reason FROM voice_pool_reconciliations WHERE source_session_id='poll'`).Scan(&reason); err != nil || !strings.Contains(reason, testCreationEvidenceID) {
		t.Fatalf("audit reason=%s err=%v", reason, err)
	}
}

func TestPoolStatusRecoveryRacingCreatePreservesNewSource(t *testing.T) {
	statusEntered, releaseStatus := make(chan struct{}), make(chan struct{})
	createEntered, releaseCreate := make(chan struct{}), make(chan struct{})
	var statusOnce, createOnce sync.Once
	defer statusOnce.Do(func() { close(releaseStatus) })
	defer createOnce.Do(func() { close(releaseCreate) })
	var lookups atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"voice_session_id":"B-media","answer_sdp":"v=0"}`
		if r.Method == http.MethodGet {
			if lookups.Add(1) == 1 {
				close(statusEntered)
				<-releaseStatus
			}
			body = fmt.Sprintf(`{"request_id":"A-request","outcome":"released","evidence_id":%q}`, testCreationEvidenceID)
		} else {
			close(createEntered)
			<-releaseCreate
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	server, store, key := handoffHTTPServerWithConfig(t, Config{GatewayURL: "https://gateway.test", GatewayAPIKey: "key", HTTPClient: client})
	// The shared HTTP fixture seeds an unrelated active owner session.
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET state='ended' WHERE id='handoff-session'`); err != nil {
		t.Fatal(err)
	}
	seedRecoverySession(t, store, "A", "quarantined")
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET last_error_code='gateway_outcome_unknown' WHERE id='A'`); err != nil {
		t.Fatal(err)
	}
	reserveRecoveryPool(t, store, "A", "gateway_outcome_unknown")
	type result struct {
		status int
		body   []byte
		err    error
	}
	request := func(method, path, body string, done chan<- result) {
		req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		done <- result{resp.StatusCode, data, err}
	}
	statusDone, createDone := make(chan result, 1), make(chan result, 1)
	go request(http.MethodGet, "/v1/voice/pool/status", "", statusDone)
	awaitRecoveryTest(t, statusEntered)
	go request(http.MethodPost, "/v1/voice/sessions", `{"request_id":"B","offer_sdp":"v=0"}`, createDone)
	select {
	case <-createEntered:
	case early := <-createDone:
		t.Fatalf("create returned before gateway synchronization: status=%d body=%s err=%v", early.status, early.body, early.err)
	case <-time.After(10 * time.Second):
		t.Fatal("create gateway synchronization timed out")
	}
	var sourceBefore string
	if err := store.DB().QueryRow(`SELECT source_session_id FROM voice_pool_state`).Scan(&sourceBefore); err != nil {
		t.Fatal(err)
	}
	statusOnce.Do(func() { close(releaseStatus) })
	statusResult := awaitRecoveryTest(t, statusDone)
	if statusResult.err != nil || statusResult.status != 200 {
		t.Fatalf("status poll=%+v", statusResult)
	}
	var pool struct {
		Blocked bool   `json:"blocked"`
		Reason  string `json:"reason_code"`
	}
	if err := json.Unmarshal(statusResult.body, &pool); err != nil {
		t.Fatal(err)
	}
	var source, state string
	var blocked, audits, events int
	const snapshot = `SELECT p.source_session_id,p.blocked,s.state,(SELECT COUNT(*) FROM voice_pool_reconciliations WHERE source_session_id='A'),(SELECT COUNT(*) FROM events WHERE event_type='voice.pool.reconciled') FROM voice_pool_state p JOIN voice_sessions s ON s.id=p.source_session_id`
	if err := store.DB().QueryRow(snapshot).Scan(&source, &blocked, &state, &audits, &events); err != nil {
		t.Fatal(err)
	}
	if !pool.Blocked || pool.Reason != "negotiation_in_progress" || source != sourceBefore || source == "A" || state != "allocating" || blocked != 1 || audits != 1 || events != 1 || lookups.Load() != 2 {
		t.Fatalf("stale status released/replaced B: pool=%+v source=%s before=%s state=%s blocked=%d audits=%d events=%d lookups=%d", pool, source, sourceBefore, state, blocked, audits, events, lookups.Load())
	}
	createOnce.Do(func() { close(releaseCreate) })
	created := awaitRecoveryTest(t, createDone)
	if created.err != nil || created.status < 200 || created.status >= 300 {
		t.Fatalf("create status=%d body=%s err=%v", created.status, created.body, created.err)
	}
	if err := store.DB().QueryRow(snapshot).Scan(&source, &blocked, &state, &audits, &events); err != nil {
		t.Fatal(err)
	}
	if source != sourceBefore || blocked != 1 || state != "active" || audits != 1 || events != 1 {
		t.Fatalf("final source=%s state=%s blocked=%d audits=%d events=%d", source, state, blocked, audits, events)
	}
}
