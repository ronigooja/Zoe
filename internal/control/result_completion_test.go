package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func TestRuntimeResultCompletion(t *testing.T) {
	for _, scenario := range []string{"normal", "worker_question", "permission_requested", "failure", "stop", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			s, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
			defer closeStore()
			s.cfg.WaitLimit = time.Hour
			exec := func(q string) {
				t.Helper()
				if _, err := s.store.DB().Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			exec(`UPDATE runs SET runtime_session_id='session' WHERE id='budget-run'`)
			observe := func(kind string, result json.RawMessage) *httptest.ResponseRecorder {
				obs := contracts.RunnerObservation{ObservationID: kind, RunID: "budget-run", AttemptID: "budget-attempt", Fence: 91, Kind: kind, Result: result, RuntimeSessionID: "session"}
				if kind == "runtime_terminated" {
					obs.TerminationProof = &contracts.TerminationProof{Source: "runner_host_session", Method: "session_interrupted", RuntimeSessionID: "session", CleanupComplete: true, VerifiedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
				}
				body, _ := json.Marshal(obs)
				req := httptest.NewRequest("POST", "/runner/v1/runs/budget-run/observations", bytes.NewReader(body))
				req.SetPathValue("run_id", "budget-run")
				req.Header.Set("Idempotency-Key", kind)
				req = req.WithContext(context.WithValue(req.Context(), runnerPrincipalKey{}, contracts.RunnerPrincipal{RunnerID: "budget-runner", WorkspaceIDs: []string{"default"}, Scopes: []string{"runner:observe"}}))
				w := httptest.NewRecorder()
				s.handleObservation(w, req)
				return w
			}
			check := func(w *httptest.ResponseRecorder) {
				t.Helper()
				if w.Code != 200 {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
			}
			check(observe("result", json.RawMessage(`{"assistant_output":"Done."}`)))
			wantTask, wantRun, wantInteraction := "complete", "verified", "none"
			switch scenario {
			case "worker_question", "permission_requested":
				check(observe(scenario, json.RawMessage(`{"request_id":"pending","question":"Which one?","action":"edit"}`)))
				wantTask, wantRun, wantInteraction = "awaiting_user", "active", "awaiting_message"
				if scenario == "permission_requested" {
					wantInteraction = "explicit_confirmation"
				}
			case "failure":
				check(observe("failure", json.RawMessage(`{"assistant_output":"Partial."}`)))
				wantTask, wantRun = "failed", "failed"
			case "stop":
				exec(`UPDATE runs SET desired_control='stop'`)
				wantTask, wantRun = "canceled", "stopped"
			case "rollback":
				exec(`CREATE TRIGGER reject_completion BEFORE INSERT ON events WHEN NEW.event_type='task.result_decided' BEGIN SELECT RAISE(ABORT,'test rollback'); END`)
				wantTask, wantRun = "active", "active"
			}
			w := observe("runtime_terminated", nil)
			if scenario == "rollback" {
				if w.Code == 200 {
					t.Fatal("expected rollback")
				}
			} else {
				check(w)
				check(observe("runtime_terminated", nil))
			}
			var task, run, interaction, finished string
			if err := s.store.DB().QueryRow(`SELECT t.status,r.status,t.interaction_state,coalesce(t.finished_at,'') FROM tasks t JOIN runs r ON r.task_id=t.id`).Scan(&task, &run, &interaction, &finished); err != nil {
				t.Fatal(err)
			}
			if task != wantTask || run != wantRun || interaction != wantInteraction {
				t.Fatalf("task=%s run=%s interaction=%s", task, run, interaction)
			}
			terminal := task == "complete" || task == "failed" || task == "canceled"
			if (finished != "") != terminal {
				t.Fatalf("finished_at=%q terminal=%v", finished, terminal)
			}
			var decisions int
			if err := s.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='task.result_decided'`).Scan(&decisions); err != nil {
				t.Fatal(err)
			}
			if (scenario == "normal" && decisions != 1) || (scenario != "normal" && decisions != 0) {
				t.Fatalf("decisions=%d", decisions)
			}
			var messages int
			if err := s.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='task.assistant_message'`).Scan(&messages); err != nil || messages != 1 {
				t.Fatalf("assistant events=%d err=%v", messages, err)
			}
		})
	}
}

func TestCompatibilityAcceptExistingResult(t *testing.T) {
	s, closeStore := budgetTestServer(t, contracts.RunBudgetSnapshot{})
	defer closeStore()
	ctx := context.Background()
	if _, err := s.store.DB().Exec(`UPDATE tasks SET status='awaiting_user',owner_subject_id='budget-owner',interaction_state='awaiting_message'; UPDATE runs SET status='verification_pending'; INSERT INTO evidence(id,workspace_id,task_id,run_id,kind,summary,source,provenance,created_at,verified,verifier_id) VALUES('legacy','default','budget-task','budget-run','agent_result','done','runner','{}','2026-01-01T00:00:00Z',0,'')`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	version := int64(1)
	payload, _ := json.Marshal(acceptResultPayload{TaskID: "budget-task", RunID: "budget-run", Decision: "accept"})
	result, events, err := s.commandTaskAcceptResult(ctx, tx, CommandContext{Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "budget-owner", DeviceID: "budget-device", Scopes: []string{"client:command"}}, AcceptedAt: time.Now().UTC()}, contracts.CommandRequest{ExpectedVersion: &version, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if result.ResourceVersion != 2 || len(events) != 1 || events[0].Type != "task.result_decided" {
		t.Fatalf("result=%+v events=%+v", result, events)
	}
	var status, run, interaction string
	if err = tx.QueryRow(`SELECT t.status,r.status,t.interaction_state FROM tasks t JOIN runs r ON r.task_id=t.id`).Scan(&status, &run, &interaction); err != nil {
		t.Fatal(err)
	}
	if status != "complete" || run != "verified" || interaction != "none" {
		t.Fatalf("task=%s run=%s interaction=%s", status, run, interaction)
	}
}
