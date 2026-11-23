package control

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/storage"
)

func budgetTestServer(t *testing.T, limits contracts.RunBudgetSnapshot) (*Server, func()) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = store.DB().Exec(`INSERT INTO actors(id,subject_id,name,status,created_at) VALUES('budget-device','budget-owner','test','approved',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(`INSERT INTO tasks(id,title,goal,source,profile_id,status,version,created_at,updated_at,accepted_at,created_by_device_id,command_id) VALUES('budget-task','budget','budget','user','worker','active',1,?,?,?,?,?)`, now, now, now, "budget-device", "budget-command"); err != nil {
		t.Fatal(err)
	}
	budgetRaw, err := json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(`INSERT INTO runs(id,task_id,attempt_id,status,fence,runner_id,authorized_device_id,command_id,goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at,budget_snapshot_json) VALUES('budget-run','budget-task','budget-attempt','active',91,'budget-runner','budget-device','budget-command','budget','worker','runtime','model','worker','opencode',?,?)`, now, budgetRaw); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store}
	return server, func() { _ = store.Close() }
}

func budgetPrincipalRequest(handler func(http.ResponseWriter, *http.Request), body any, runID string) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/runner/v1/runs/"+runID+"/budget/reservations", bytes.NewReader(encoded))
	r.SetPathValue("run_id", runID)
	p := contracts.RunnerPrincipal{RunnerID: "budget-runner", Scopes: []string{"runner:claim"}}
	r = r.WithContext(context.WithValue(r.Context(), runnerPrincipalKey{}, p))
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func TestBudgetReserveIsIdempotentAcrossConcurrentRetries(t *testing.T) {
	server, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{MaxModelCalls: 2, MaxInputUnits: 100, MaxOutputUnits: 100, MaxToolSteps: 10, MaxArtifactBytes: 1000})
	defer closeStore()
	request := contracts.RunBudgetReserveRequest{AttemptID: "budget-attempt", Fence: 91, ReservationID: "provider-call-1", Usage: contracts.RunBudgetUsage{ModelCalls: 1, InputUnits: 40}}
	const workers = 12
	var wg sync.WaitGroup
	wg.Add(workers)
	results := make(chan *httptest.ResponseRecorder, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			results <- budgetPrincipalRequest(server.handleRunBudgetReserve, request, "budget-run")
		}()
	}
	wg.Wait()
	close(results)
	for response := range results {
		if response.Code != 200 {
			t.Fatalf("reserve status=%d body=%s", response.Code, response.Body.String())
		}
		var receipt contracts.RunBudgetReceipt
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || !receipt.Allowed || receipt.Consumed.ModelCalls != 1 || receipt.Consumed.InputUnits != 40 {
			t.Fatalf("unexpected idempotent receipt=%+v err=%v", receipt, err)
		}
	}
	var rows int
	if err := server.store.DB().QueryRow(`SELECT count(*) FROM run_budget_reservations WHERE run_id='budget-run'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("ledger reservations=%d err=%v", rows, err)
	}
}

func TestBudgetReservationReplayRechecksCurrentRunControl(t *testing.T) {
	server, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{MaxModelCalls: 2, MaxInputUnits: 100})
	defer closeStore()
	request := contracts.RunBudgetReserveRequest{AttemptID: "budget-attempt", Fence: 91, ReservationID: "provider-call-1", Usage: contracts.RunBudgetUsage{ModelCalls: 1}}
	first := budgetPrincipalRequest(server.handleRunBudgetReserve, request, "budget-run")
	if first.Code != http.StatusOK {
		t.Fatalf("initial reserve status=%d body=%s", first.Code, first.Body.String())
	}
	if _, err := server.store.DB().Exec(`UPDATE runs SET desired_control='stop' WHERE id='budget-run'`); err != nil {
		t.Fatal(err)
	}
	replay := budgetPrincipalRequest(server.handleRunBudgetReserve, request, "budget-run")
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	var receipt contracts.RunBudgetReceipt
	if err := json.Unmarshal(replay.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Allowed || !receipt.StopRequested || receipt.Consumed.ModelCalls != 1 {
		t.Fatalf("replay ignored current stop or changed ledger: %+v", receipt)
	}
	var count int
	if err := server.store.DB().QueryRow(`SELECT count(*) FROM run_budget_reservations WHERE run_id='budget-run'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("replay reservation count=%d err=%v", count, err)
	}
}

func TestBudgetReservationRejectsUsageOverflow(t *testing.T) {
	server, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{MaxInputUnits: 100, MaxModelCalls: 1})
	defer closeStore()
	first := contracts.RunBudgetReserveRequest{AttemptID: "budget-attempt", Fence: 91, ReservationID: "provider-call-1", Usage: contracts.RunBudgetUsage{InputUnits: 40}}
	if response := budgetPrincipalRequest(server.handleRunBudgetReserve, first, "budget-run"); response.Code != http.StatusOK {
		t.Fatalf("first reserve status=%d body=%s", response.Code, response.Body.String())
	}
	second := contracts.RunBudgetReserveRequest{AttemptID: "budget-attempt", Fence: 91, ReservationID: "provider-call-2", Usage: contracts.RunBudgetUsage{InputUnits: math.MaxInt64}}
	response := budgetPrincipalRequest(server.handleRunBudgetReserve, second, "budget-run")
	if response.Code != http.StatusOK {
		t.Fatalf("overflow reserve status=%d body=%s", response.Code, response.Body.String())
	}
	var receipt contracts.RunBudgetReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Allowed || !receipt.StopRequested || receipt.Consumed.InputUnits != 40 {
		t.Fatalf("overflow reservation was not rejected conservatively: %+v", receipt)
	}
}

func TestUnknownBudgetSettlementKeepsReservationAndExhaustionRequestsStop(t *testing.T) {
	server, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{MaxModelCalls: 1, MaxInputUnits: 100, MaxOutputUnits: 100, MaxToolSteps: 10, MaxArtifactBytes: 1000})
	defer closeStore()
	reserve := contracts.RunBudgetReserveRequest{AttemptID: "budget-attempt", Fence: 91, ReservationID: "provider-call-1", Usage: contracts.RunBudgetUsage{ModelCalls: 1, InputUnits: 40}}
	if response := budgetPrincipalRequest(server.handleRunBudgetReserve, reserve, "budget-run"); response.Code != 200 {
		t.Fatalf("reserve status=%d body=%s", response.Code, response.Body.String())
	}
	settlement := contracts.RunBudgetSettleRequest{AttemptID: "budget-attempt", Fence: 91, SettlementID: "provider-call-1-outcome", Outcome: "unknown"}
	settleReq, _ := json.Marshal(settlement)
	r := httptest.NewRequest("POST", "/runner/v1/runs/budget-run/budget/reservations/provider-call-1/settlement", bytes.NewReader(settleReq))
	r.SetPathValue("run_id", "budget-run")
	r.SetPathValue("reservation_id", "provider-call-1")
	r = r.WithContext(context.WithValue(r.Context(), runnerPrincipalKey{}, contracts.RunnerPrincipal{RunnerID: "budget-runner", Scopes: []string{"runner:claim"}}))
	w := httptest.NewRecorder()
	server.handleRunBudgetSettle(w, r)
	if w.Code != 200 {
		t.Fatalf("unknown settle status=%d body=%s", w.Code, w.Body.String())
	}
	second := contracts.RunBudgetReserveRequest{AttemptID: "budget-attempt", Fence: 91, ReservationID: "provider-call-2", Usage: contracts.RunBudgetUsage{ModelCalls: 1}}
	response := budgetPrincipalRequest(server.handleRunBudgetReserve, second, "budget-run")
	if response.Code != 200 {
		t.Fatalf("over-budget reserve status=%d body=%s", response.Code, response.Body.String())
	}
	var receipt contracts.RunBudgetReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Allowed || !receipt.StopRequested || receipt.Status != "rejected" {
		t.Fatalf("over-budget receipt=%+v", receipt)
	}
	var desired string
	if err := server.store.DB().QueryRow(`SELECT desired_control FROM runs WHERE id='budget-run'`).Scan(&desired); err != nil || desired != "stop" {
		t.Fatalf("desired_control=%q err=%v", desired, err)
	}
	var consumed contracts.RunBudgetUsage
	if err := json.Unmarshal(mustJSON(budgetConsumedForTest(t, server, "budget-run")), &consumed); err != nil || consumed.ModelCalls != 1 {
		t.Fatalf("unknown reservation was released: %+v err=%v", consumed, err)
	}
}

func budgetConsumedForTest(t *testing.T, s *Server, runID string) contracts.RunBudgetUsage {
	t.Helper()
	tx, err := s.store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	usage, err := budgetConsumedTx(httptest.NewRequest("GET", "/", nil), tx, runID)
	if err != nil {
		t.Fatal(err)
	}
	return usage
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
