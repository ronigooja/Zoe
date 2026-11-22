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
