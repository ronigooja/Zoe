package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func assistantObservationRequest(t *testing.T, s *Server, run, id, kind string, result json.RawMessage) *httptest.ResponseRecorder {
	t.Helper()
	attempt, fence := "budget-attempt", uint64(91)
	if run != "budget-run" {
		attempt, fence = "budget-attempt-2", 92
	}
	o := contracts.RunnerObservation{ObservationID: id, RunID: run, AttemptID: attempt, Fence: fence, Kind: kind, Result: result, Error: "boom"}
	b, _ := json.Marshal(o)
	r := httptest.NewRequest(http.MethodPost, "/runner/v1/runs/"+run+"/observations", bytes.NewReader(b))
	r.SetPathValue("run_id", run)
	r.Header.Set("Idempotency-Key", id)
	r = r.WithContext(context.WithValue(r.Context(), runnerPrincipalKey{}, contracts.RunnerPrincipal{RunnerID: "budget-runner", WorkspaceIDs: []string{"default"}, Scopes: []string{"runner:observe"}}))
	w := httptest.NewRecorder()
	s.handleObservation(w, r)
	return w
}

func assistantCounts(t *testing.T, s *Server, task, run string) (entries, events int, entryID, role string, version, cursor int64) {
	t.Helper()
	if err := s.store.DB().QueryRow(`SELECT count(*) FROM task_entries WHERE task_id=? AND kind='assistant_message'`, task).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if err := s.store.DB().QueryRow(`SELECT count(*) FROM events WHERE aggregate_id=? AND event_type='task.assistant_message' AND json_extract(payload,'$.payload.run_id')=?`, task, run).Scan(&events); err != nil {
		t.Fatal(err)
	}
	_ = s.store.DB().QueryRow(`SELECT id FROM task_entries WHERE task_id=? AND kind='assistant_message'`, task).Scan(&entryID)
	var payload []byte
	_ = s.store.DB().QueryRow(`SELECT payload FROM events WHERE aggregate_id=? AND event_type='task.assistant_message' AND json_extract(payload,'$.payload.run_id')=?`, task, run).Scan(&payload)
	var envelope struct {
		Payload struct{ EntryID, Role string } `json:"payload"`
	}
	_ = json.Unmarshal(payload, &envelope)
	p := envelope.Payload
	role = p.Role
	_ = s.store.DB().QueryRow(`SELECT version FROM tasks WHERE id=?`, task).Scan(&version)
	_ = s.store.DB().QueryRow(`SELECT current_cursor FROM event_counters WHERE workspace_id='default'`).Scan(&cursor)
	return
}

func TestObservationResultWithAssistantOutputStoresOneEntryAndEvent(t *testing.T) {
	s, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
	defer closeStore()
	result := json.RawMessage(`{"assistant_output":"hello","assistant_message":{"id":"m"},"role":"steward"}`)
	if w := assistantObservationRequest(t, s, "budget-run", "result-1", "result", result); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var content string
	if err := s.store.DB().QueryRow(`SELECT content FROM task_entries WHERE task_id='budget-task'`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "hello" {
		t.Fatalf("content=%q", content)
	}
	entries, events, id, role, taskVersion, cursor := assistantCounts(t, s, "budget-task", "budget-run")
	if entries != 1 || events != 1 || id == "" || role != "worker" {
		t.Fatalf("entries=%d events=%d id=%q role=%q", entries, events, id, role)
	}
	if taskVersion != 2 || cursor < 2 {
		t.Fatalf("task version=%d cursor=%d", taskVersion, cursor)
	}
	var aggregateVersion int64
	if err := s.store.DB().QueryRow(`SELECT aggregate_version FROM events WHERE event_type='task.assistant_message'`).Scan(&aggregateVersion); err != nil {
		t.Fatal(err)
	}
	if aggregateVersion != taskVersion {
		t.Fatalf("event aggregate_version=%d task version=%d", aggregateVersion, taskVersion)
	}
}

func TestObservationAssistantOutputDeduplicatesRetriesAndSameRunText(t *testing.T) {
	s, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
	defer closeStore()
	r := json.RawMessage(`{"assistant_output":"same"}`)
	for _, id := range []string{"same-1", "same-1", "same-2"} {
		if w := assistantObservationRequest(t, s, "budget-run", id, "result", r); w.Code != 200 {
			t.Fatalf("%s status=%d body=%s", id, w.Code, w.Body)
		}
	}
	e, v, _, _, _, _ := assistantCounts(t, s, "budget-task", "budget-run")
	if e != 1 || v != 1 {
		t.Fatalf("entries=%d events=%d", e, v)
	}
}

func TestObservationSameAssistantSentenceFromDifferentRunsStoresTwoEntries(t *testing.T) {
	s, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
	defer closeStore()
	if _, err := s.store.DB().Exec(`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,fence,runner_id,authorized_device_id,command_id,goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at,budget_snapshot_json) SELECT 'budget-run-2',workspace_id,task_id,'budget-attempt-2','active',92,runner_id,authorized_device_id,'budget-command-2',goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at,budget_snapshot_json FROM runs WHERE id='budget-run'`); err != nil {
		t.Fatal(err)
	}
	r := json.RawMessage(`{"assistant_output":"same"}`)
	if w := assistantObservationRequest(t, s, "budget-run", "run1", "result", r); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := assistantObservationRequest(t, s, "budget-run-2", "run2", "result", r); w.Code != 200 {
		t.Fatal(w.Code)
	}
	var entries, events int
	if err := s.store.DB().QueryRow(`SELECT count(*) FROM task_entries WHERE task_id='budget-task' AND kind='assistant_message'`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if err := s.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='task.assistant_message'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if entries != 2 || events != 2 {
		t.Fatalf("entries=%d events=%d", entries, events)
	}
}

func TestObservationWhitespaceAndEmptyResultStoreNoAssistantEntry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result json.RawMessage
	}{
		{"whitespace", json.RawMessage(`{"assistant_output":" \n\t"}`)}, {"empty", json.RawMessage(`{}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
			defer closeStore()
			if w := assistantObservationRequest(t, s, "budget-run", tc.name, "result", tc.result); w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			e, v, _, _, _, _ := assistantCounts(t, s, "budget-task", "budget-run")
			if e != 0 || v != 0 {
				t.Fatalf("entries=%d events=%d", e, v)
			}
		})
	}
}

func TestObservationAssistantOutputTruncatesAtTwentyThousandBytes(t *testing.T) {
	s, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
	defer closeStore()
	result, _ := json.Marshal(map[string]string{"assistant_output": strings.Repeat("x", 20001)})
	if w := assistantObservationRequest(t, s, "budget-run", "long", "result", result); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var content string
	if err := s.store.DB().QueryRow(`SELECT content FROM task_entries`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if len(content) != 20000 {
		t.Fatalf("length=%d", len(content))
	}
}

func TestFailureObservationWithAndWithoutPartialOutput(t *testing.T) {
	s, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
	defer closeStore()
	if w := assistantObservationRequest(t, s, "budget-run", "failure-2", "failure", nil); w.Code != 200 {
		t.Fatalf("empty failure status=%d body=%s", w.Code, w.Body)
	}
	var status string
	if err := s.store.DB().QueryRow(`SELECT status FROM runs WHERE id='budget-run'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("run status=%q", status)
	}
	if w := assistantObservationRequest(t, s, "budget-run", "failure-1", "failure", json.RawMessage(`{"assistant_output":"partial"}`)); w.Code != 200 {
		t.Fatalf("partial status=%d body=%s", w.Code, w.Body)
	}
	e, v, _, _, _, _ := assistantCounts(t, s, "budget-task", "budget-run")
	if e != 1 || v != 1 {
		t.Fatalf("entries=%d events=%d", e, v)
	}
}

func TestEnqueueEventNotificationIgnoresAssistantMessage(t *testing.T) {
	s, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
	defer closeStore()
	tx, err := s.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = enqueueEventNotification(context.Background(), tx, contracts.Event{Type: "task.assistant_message", WorkspaceID: "default", Payload: json.RawMessage(`{"task_id":"budget-task"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM notifications`).Scan(&count); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("notifications=%d", count)
	}
}
