package voice

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/steward"
)

// rollbackVoiceAssignment applies when a fixture reconstructs pre-217 schema;
// remove the later register tables and receipt before rolling back assignment.
const rollbackVoiceAssignment = `DROP TABLE voice_context_registers;
DROP TABLE IF EXISTS voice_register_memory_boundaries;
DELETE FROM schema_migrations WHERE namespace='voice' AND version>=218;
ALTER TABLE voice_turns DROP COLUMN steward_action;
ALTER TABLE voice_turns DROP COLUMN steward_task_id;
DELETE FROM schema_migrations WHERE namespace='voice' AND version=217;`

func TestAssignmentMigrationKeepsOldRows(t *testing.T) {
	_, store, _ := handoffHTTPServer(t)
	rosterExec(t, store.DB(), `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,steward_decision) VALUES('assignment-upgrade','default','handoff-session',1,'assignment-upgrade',1,'original',X'01','2026-11-06T00:00:00Z','assignment-upgrade','dispatch');`+rollbackVoiceAssignment)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var content, decision, action, task string
	if err := store.DB().QueryRow(`SELECT content,steward_decision,steward_action,steward_task_id FROM voice_turns WHERE id='assignment-upgrade'`).Scan(&content, &decision, &action, &task); err != nil {
		t.Fatal(err)
	}
	if content != "original" || decision != "dispatch" || action != "" || task != "" {
		t.Fatalf("upgrade lost row/defaults: %q %q %q %q", content, decision, action, task)
	}
}

func repeatAssignmentHandoff(t *testing.T, ts *httptest.Server, key, turn, sentence string) contracts.CommandReceipt {
	t.Helper()
	hash := sha256.Sum256([]byte(sentence))
	p := map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": turn, "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])}
	var receipt contracts.CommandReceipt
	// Retry both the same command and a different command for the same turn.
	for _, command := range []string{"handoff-" + turn, "repeat-" + turn} {
		code, r, raw := handoffCommand(t, ts.Client(), ts.URL, key, command, "voice.turn.handoff", p)
		if code != http.StatusOK {
			t.Fatalf("repeat handoff: %d %s", code, raw)
		}
		receipt = r
	}
	return receipt
}

func TestAssignNewAndNone(t *testing.T) {
	for _, action := range []steward.Action{steward.ActionNew, steward.ActionNone} {
		t.Run(string(action), func(t *testing.T) {
			f := &fakeSteward{assignment: steward.Assignment{Action: action}}
			ts, store, key := handoffHTTPServerWithConfig(t, Config{Steward: f})
			r := commitAndHandoff(t, ts, key, "assignment", "check the weather")
			repeatAssignmentHandoff(t, ts, key, "assignment", "check the weather")
			var tasks, replies, handled int
			var stored, decision string
			if err := store.DB().QueryRow(`SELECT (SELECT count(*) FROM tasks),(SELECT count(*) FROM voice_fast_replies),handoff_handled,steward_action,steward_decision FROM voice_turns WHERE turn_id='assignment'`).Scan(&tasks, &replies, &handled, &stored, &decision); err != nil {
				t.Fatal(err)
			}
			if stored != string(action) || decision != "dispatch" || f.calls.Load() != 1 || replies != 0 {
				t.Fatalf("assignment=%s decision=%s calls=%d replies=%d", stored, decision, f.calls.Load(), replies)
			}
			if action == steward.ActionNone {
				if tasks != 0 || handled != 1 || r.ResourceType != "voice_turn" {
					t.Fatalf("none: tasks=%d handled=%d receipt=%+v", tasks, handled, r)
				}
			} else {
				var status string
				if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, r.ResourceID).Scan(&status); err != nil || status != "queued" || tasks != 1 {
					t.Fatalf("new: status=%s tasks=%d err=%v", status, tasks, err)
				}
			}
		})
	}
}

func TestAssignAnswerOriginalSentenceAndRoster(t *testing.T) {
	f := &fakeSteward{}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Steward: f})
	first := commitAndHandoff(t, ts, key, "work", "research the project")
	rosterExec(t, store.DB(), `UPDATE tasks SET status='waiting',interaction_state='awaiting_message',interaction_question='Which city?' WHERE id=?`, first.ResourceID)
	f.assignment = steward.Assignment{Action: steward.ActionAnswer, TaskID: first.ResourceID}
	const sentence = "上海，保留原始措辞。"
	r := commitAndHandoff(t, ts, key, "answer", sentence)
	repeatAssignmentHandoff(t, ts, key, "answer", sentence)
	var status, resumed string
	var handled, entries, tasks int
	if err := store.DB().QueryRow(`SELECT t.status,v.resumed_task_id,v.handoff_handled,(SELECT count(*) FROM task_entries WHERE task_id=t.id AND kind='user_message' AND content=?),(SELECT count(*) FROM tasks) FROM tasks t JOIN voice_turns v ON v.resumed_task_id=t.id WHERE v.turn_id='answer'`, sentence).Scan(&status, &resumed, &handled, &entries, &tasks); err != nil {
		t.Fatal(err)
	}
	if r.ResourceID != first.ResourceID || resumed != first.ResourceID || status != "queued" || handled != 1 || entries != 1 || tasks != 1 {
		t.Fatalf("answer=%+v status=%s resumed=%s handled=%d entries=%d tasks=%d", r, status, resumed, handled, entries, tasks)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.roster) != 1 || f.roster[0].ID != first.ResourceID || f.roster[0].Status != "waiting" || f.roster[0].Question != "Which city?" {
		t.Fatalf("roster=%+v", f.roster)
	}
}

func TestAssignAttachActiveStopsOnceAndConcatenates(t *testing.T) {
	f := &fakeSteward{}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Steward: f})
	const original = "research the original project"
	first := commitAndHandoff(t, ts, key, "work", original)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/runner/v1/claims", nil)
	req.Header.Set("Authorization", "Bearer BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var claim contracts.JobClaim
	err = json.NewDecoder(resp.Body).Decode(&claim)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || claim.TaskID != first.ResourceID {
		t.Fatalf("claim=%+v status=%d err=%v", claim, resp.StatusCode, err)
	}
	f.assignment = steward.Assignment{Action: steward.ActionAttach, TaskID: first.ResourceID}
	const sentence = "only consider the latest version"
	r := commitAndHandoff(t, ts, key, "refinement", sentence)
	repeatAssignmentHandoff(t, ts, key, "refinement", sentence)
	var stops, tasks int
	var goal, title, status, oldStatus string
	if err := store.DB().QueryRow(`SELECT (SELECT count(*) FROM runs WHERE task_id=? AND desired_control='stop'),(SELECT count(*) FROM tasks),goal,title,status,(SELECT status FROM tasks WHERE id=?) FROM tasks WHERE id=?`, first.ResourceID, first.ResourceID, r.ResourceID).Scan(&stops, &tasks, &goal, &title, &status, &oldStatus); err != nil {
		t.Fatal(err)
	}
	if stops != 1 || tasks != 2 || goal != original+"\n"+sentence || title != sentence || status != "queued" || oldStatus != "active" {
		t.Fatalf("stops=%d tasks=%d goal=%q title=%q status=%s old=%s", stops, tasks, goal, title, status, oldStatus)
	}
	var stopEvents int
	if err := store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='task.stop_requested' AND aggregate_id=?`, first.ResourceID).Scan(&stopEvents); err != nil || stopEvents != 1 {
		t.Fatalf("stop events=%d err=%v", stopEvents, err)
	}
	var entries int
	if err := store.DB().QueryRow(`SELECT count(*) FROM task_entries WHERE task_id=? AND kind='user_message' AND content=?`, r.ResourceID, goal).Scan(&entries); err != nil || entries != 1 {
		t.Fatalf("initial message entries=%d err=%v", entries, err)
	}
}

func TestAssignAttachCompletePreservesOldAndTruncatesRunes(t *testing.T) {
	f := &fakeSteward{}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Steward: f})
	first := commitAndHandoff(t, ts, key, "work", "research the project")
	result := strings.Repeat("结", 605)
	rosterExec(t, store.DB(), `UPDATE tasks SET status='complete',result=?,finished_at=updated_at WHERE id=?`, result, first.ResourceID)
	f.assignment = steward.Assignment{Action: steward.ActionAttach, TaskID: first.ResourceID}
	const sentence = "expand the last point"
	r := commitAndHandoff(t, ts, key, "followup", sentence)
	repeatAssignmentHandoff(t, ts, key, "followup", sentence)
	var goal, status, oldResult string
	var tasks int
	if err := store.DB().QueryRow(`SELECT n.goal,o.status,o.result,(SELECT count(*) FROM tasks) FROM tasks n,tasks o WHERE n.id=? AND o.id=?`, r.ResourceID, first.ResourceID).Scan(&goal, &status, &oldResult, &tasks); err != nil {
		t.Fatal(err)
	}
	if goal != sentence+"\n上一个任务的结果:\n"+strings.Repeat("结", 600) || status != "complete" || oldResult != result || tasks != 2 {
		t.Fatalf("goal=%q status=%s result preserved=%t tasks=%d", goal, status, oldResult == result, tasks)
	}
}

func TestAssignInvalidTargetFallsBackToNew(t *testing.T) {
	for _, action := range []steward.Action{steward.ActionAnswer, steward.ActionAttach} {
		for _, scenario := range []string{"other-session", "other-generation", "other-owner", "missing", "failed", "canceled", "explicit-confirmation"} {
			if action == steward.ActionAttach && scenario == "explicit-confirmation" {
				continue // Attach uses the normal stop path, never confirmation assent.
			}
			t.Run(string(action)+"/"+scenario, func(t *testing.T) {
				f := &fakeSteward{}
				ts, store, key := handoffHTTPServerWithConfig(t, Config{Steward: f})
				first := commitAndHandoff(t, ts, key, "work", "research the project")
				rosterExec(t, store.DB(), `UPDATE tasks SET status='waiting',interaction_state='awaiting_message',interaction_question='Which city?' WHERE id=?`, first.ResourceID)
				target := first.ResourceID
				switch scenario {
				case "other-session":
					rosterExec(t, store.DB(), `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) SELECT 'elsewhere',workspace_id,owner_id,device_id,'elsewhere',X'01',1,'ended',created_at,updated_at FROM voice_sessions WHERE id='handoff-session'`)
					rosterExec(t, store.DB(), `UPDATE voice_task_links SET session_id='elsewhere' WHERE task_id=?`, target)
				case "other-generation":
					rosterExec(t, store.DB(), `UPDATE voice_task_links SET generation=2 WHERE task_id=?`, target)
				case "other-owner":
					rosterExec(t, store.DB(), `UPDATE tasks SET owner_subject_id='another-owner' WHERE id=?`, target)
				case "missing":
					target = "nonexistent"
				case "failed", "canceled":
					rosterExec(t, store.DB(), `UPDATE tasks SET status=? WHERE id=?`, scenario, target)
				case "explicit-confirmation":
					rosterExec(t, store.DB(), `UPDATE tasks SET interaction_state='explicit_confirmation' WHERE id=?`, target)
				}
				f.assignment = steward.Assignment{Action: action, TaskID: target}
				r := commitAndHandoff(t, ts, key, "fallback", "original new sentence")
				var goal, status string
				if err := store.DB().QueryRow(`SELECT goal,status FROM tasks WHERE id=?`, r.ResourceID).Scan(&goal, &status); err != nil || goal != "original new sentence" || status != "queued" || r.ResourceID == first.ResourceID {
					t.Fatalf("fallback=%+v goal=%q status=%s err=%v", r, goal, status, err)
				}
			})
		}
	}
}

func TestAssignLegacyAskAdmitsQueued(t *testing.T) {
	ts, store, key := handoffHTTPServer(t)
	const sentence = "ordinary legacy request"
	hash := sha256.Sum256([]byte(sentence))
	rosterExec(t, store.DB(), `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,triage_category,steward_decision) VALUES('legacy','default','handoff-session',1,'legacy',1,?,?,'2026-11-06T00:00:00Z','legacy','task','ask')`, sentence, hash[:])
	r := repeatAssignmentHandoff(t, ts, key, "legacy", sentence)
	var status string
	if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, r.ResourceID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("legacy status=%s err=%v", status, err)
	}
}
