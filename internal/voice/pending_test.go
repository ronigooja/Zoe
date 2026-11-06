package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/steward"
)

type pendingClassifier struct {
	category string
	fail     error
}

func (f *pendingClassifier) Classify(context.Context, string) (string, error) {
	return f.category, f.fail
}

func TestHeldSpokenConfirmScopedOnce(t *testing.T) {
	f := &pendingClassifier{category: "task"}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: f})
	first := commitAndHandoff(t, ts, key, "held", "delete all files in my Downloads folder")
	f.category = "confirm"
	answer := commitAndHandoff(t, ts, key, "answer", "yes")
	if answer.ResourceID != first.ResourceID {
		t.Fatalf("confirmation created another task: %s != %s", answer.ResourceID, first.ResourceID)
	}
	var status, interaction, scope, digest string
	if err := store.DB().QueryRow(`SELECT t.status,t.interaction_state,c.scope,c.action_digest FROM tasks t JOIN task_confirmations c ON c.task_id=t.id WHERE t.id=?`, first.ResourceID).Scan(&status, &interaction, &scope, &digest); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || interaction != "none" || scope != "task:admit" || len(digest) != 64 {
		t.Fatalf("invalid authorization: %s %s %s %s", status, interaction, scope, digest)
	}
	hash := sha256.Sum256([]byte("yes"))
	code, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "retry-confirm", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "answer", "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])})
	if code != 200 {
		t.Fatalf("retry: %d %s", code, raw)
	}
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/runner/v1/claims", nil)
		req.Header.Set("Authorization", "Bearer BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var claim contracts.JobClaim
		err = json.NewDecoder(resp.Body).Decode(&claim)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || claim.TaskID != first.ResourceID {
			t.Fatalf("claim: %+v status=%d err=%v", claim, resp.StatusCode, err)
		}
	}
	var runs, confirmations int
	_ = store.DB().QueryRow(`SELECT count(*) FROM runs WHERE task_id=?`, first.ResourceID).Scan(&runs)
	_ = store.DB().QueryRow(`SELECT count(*) FROM task_confirmations WHERE task_id=?`, first.ResourceID).Scan(&confirmations)
	if runs != 1 || confirmations != 1 {
		t.Fatalf("runs=%d confirmations=%d", runs, confirmations)
	}
}

func TestHeldDeclineAndSessionCancel(t *testing.T) {
	for _, category := range []string{"decline", "cancel"} {
		t.Run(category, func(t *testing.T) {
			f := &pendingClassifier{category: "task"}
			ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: f})
			first := commitAndHandoff(t, ts, key, "held", "delete all files in my Downloads folder")
			f.category = category
			commitAndHandoff(t, ts, key, "answer", "forget it")
			var status, text string
			_ = store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, first.ResourceID).Scan(&status)
			_ = store.DB().QueryRow(`SELECT text FROM voice_fast_replies WHERE turn_id='answer'`).Scan(&text)
			want := "OK, cancelled"
			if category == "cancel" {
				want = "OK, stopped"
			}
			if status != "canceled" || text != recallPrefix+want {
				t.Fatalf("status=%s reply=%q", status, text)
			}
		})
	}
}

func TestSpokenConfirmStewardAsk(t *testing.T) {
	f := &pendingClassifier{category: "task"}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: f, Steward: &fakeSteward{decision: steward.Ask}})
	first := commitAndHandoff(t, ts, key, "ask", "please handle this")
	f.category = "confirm"
	answer := commitAndHandoff(t, ts, key, "answer", "yes")
	var status string
	_ = store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, first.ResourceID).Scan(&status)
	if answer.ResourceID != first.ResourceID || status != "queued" {
		t.Fatalf("answer=%+v status=%s", answer, status)
	}
}

func TestPendingSafety(t *testing.T) {
	for _, scenario := range []string{"none", "stale", "other-session", "unknown", "timeout", "cancel-none", "cancel-running", "cancel-other-session"} {
		t.Run(scenario, func(t *testing.T) {
			f := &pendingClassifier{category: "task"}
			ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: f})
			id := ""
			if scenario != "none" && scenario != "cancel-none" {
				goal := "delete all files in my Downloads folder"
				if scenario == "cancel-running" {
					goal = "look this up"
				}
				id = commitAndHandoff(t, ts, key, "held", goal).ResourceID
			}
			switch scenario {
			case "stale":
				_, _ = store.DB().Exec(`UPDATE tasks SET created_at=? WHERE id=?`, time.Now().Add(-11*time.Minute).UTC().Format(time.RFC3339Nano), id)
			case "other-session", "cancel-other-session":
				if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) SELECT 'elsewhere',workspace_id,owner_id,device_id,'elsewhere',X'01',1,'ended',created_at,updated_at FROM voice_sessions WHERE id='handoff-session'`); err != nil {
					t.Fatal(err)
				}
				_, err := store.DB().Exec(`UPDATE voice_task_links SET session_id='elsewhere' WHERE task_id=?`, id)
				if err != nil {
					t.Fatal(err)
				}
			case "cancel-running":
				_, _ = store.DB().Exec(`UPDATE tasks SET status='active' WHERE id=?`, id)
			}
			f.category = "confirm"
			if scenario == "unknown" {
				f.category = "unknown"
			}
			if scenario == "timeout" {
				f.fail = context.DeadlineExceeded
			}
			if scenario == "cancel-none" || scenario == "cancel-running" || scenario == "cancel-other-session" {
				f.category = "cancel"
			}
			commitAndHandoff(t, ts, key, "answer", "yes")
			var status string
			if id != "" {
				_ = store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, id).Scan(&status)
			}
			if scenario == "cancel-running" {
				if status != "canceled" {
					t.Fatalf("not canceled: %s", status)
				}
				return
			}
			if id != "" && status != "waiting" {
				t.Fatalf("held task changed: %s", status)
			}
			var confirmations int
			_ = store.DB().QueryRow(`SELECT count(*) FROM task_confirmations`).Scan(&confirmations)
			if confirmations != 0 {
				t.Fatalf("unauthorized confirmations=%d", confirmations)
			}
			if scenario == "none" || scenario == "stale" || scenario == "other-session" {
				var category string
				_ = store.DB().QueryRow(`SELECT triage_category FROM voice_turns WHERE turn_id='answer'`).Scan(&category)
				if category != "task" {
					t.Fatalf("unbound category accepted: %s", category)
				}
			}
			if scenario == "unknown" || scenario == "timeout" {
				commitAndHandoff(t, ts, key, "answer-again", "uncertain")
				var tasks, replies int
				_ = store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks)
				_ = store.DB().QueryRow(`SELECT count(*) FROM voice_fast_replies WHERE intent_kind='task_control'`).Scan(&replies)
				if tasks != 1 || replies != 1 {
					t.Fatalf("tasks=%d reasks=%d", tasks, replies)
				}
			}
			if scenario == "cancel-none" || scenario == "cancel-other-session" {
				var text string
				_ = store.DB().QueryRow(`SELECT text FROM voice_fast_replies WHERE turn_id='answer'`).Scan(&text)
				if text != recallPrefix+"Nothing is running." {
					t.Fatalf("reply=%q", text)
				}
			}
		})
	}
}

func TestCancelLiveRunUsesNormalStop(t *testing.T) {
	f := &pendingClassifier{category: "task"}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: f})
	first := commitAndHandoff(t, ts, key, "work", "look this up")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/runner/v1/claims", nil)
	req.Header.Set("Authorization", "Bearer BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var claim contracts.JobClaim
	err = json.NewDecoder(resp.Body).Decode(&claim)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("claim: status=%d err=%v", resp.StatusCode, err)
	}
	f.category = "cancel"
	commitAndHandoff(t, ts, key, "stop", "forget it, don't look that up")
	var desired, status string
	if err := store.DB().QueryRow(`SELECT r.desired_control,t.status FROM runs r JOIN tasks t ON t.id=r.task_id WHERE r.task_id=?`, first.ResourceID).Scan(&desired, &status); err != nil {
		t.Fatal(err)
	}
	if desired != "stop" || status != "active" {
		t.Fatalf("desired=%s status=%s; live termination must not be fabricated", desired, status)
	}
}

func TestPendingClassificationLogsExcludeText(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	f := &pendingClassifier{category: "task"}
	ts, _, key := handoffHTTPServerWithConfig(t, Config{Classifier: f})
	commitAndHandoff(t, ts, key, "private-hold", "delete private-download-sentinel")
	f.category = "confirm"
	commitAndHandoff(t, ts, key, "private-answer", "private-utterance-sentinel")
	if strings.Contains(logs.String(), "private-download-sentinel") || strings.Contains(logs.String(), "private-utterance-sentinel") {
		t.Fatal("private text in logs")
	}
	if !strings.Contains(logs.String(), "voice_classification id=") {
		t.Fatal("no classification diagnostic was observed")
	}
}
