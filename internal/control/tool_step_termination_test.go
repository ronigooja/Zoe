package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func TestToolStepLimitTerminatesRunWaitingForAction(t *testing.T) {
	server, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{MaxToolSteps: 1})
	defer closeStore()
	now := time.Now().UTC()
	if _, err := server.store.DB().Exec(`UPDATE runs SET runtime_session_id='budget-session' WHERE id='budget-run'`); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.DB().Exec(`INSERT INTO pending_runtime_actions(id,task_id,run_id,fence,request_id,kind,question,action_digest,expires_at,created_at) VALUES('action','budget-task','budget-run',91,'request','worker_question','Question?','digest',?,?)`, now.Add(time.Minute).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	o := contracts.RunnerObservation{
		ObservationID:    "terminal",
		RunID:            "budget-run",
		AttemptID:        "budget-attempt",
		Fence:            91,
		Kind:             "runtime_terminated",
		RuntimeSessionID: "budget-session",
		Error:            "tool_step_limit",
		TerminationProof: &contracts.TerminationProof{Source: "runner_host_session", Method: "session_interrupted", RuntimeSessionID: "budget-session", CleanupComplete: true, VerifiedAt: now},
	}
	body, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/runner/v1/runs/budget-run/observations", bytes.NewReader(body))
	r.SetPathValue("run_id", o.RunID)
	r.Header.Set("Idempotency-Key", o.ObservationID)
	p := contracts.RunnerPrincipal{RunnerID: "budget-runner", Scopes: []string{"runner:observe"}}
	r = r.WithContext(context.WithValue(r.Context(), runnerPrincipalKey{}, p))
	w := httptest.NewRecorder()
	server.handleObservation(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("termination response status=%d", w.Code)
	}
	var runStatus, taskStatus string
	if err := server.store.DB().QueryRow(`SELECT r.status,t.status FROM runs r JOIN tasks t ON t.id=r.task_id WHERE r.id='budget-run'`).Scan(&runStatus, &taskStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "failed" || taskStatus != "failed" {
		t.Fatalf("termination left run=%s task=%s", runStatus, taskStatus)
	}
}
