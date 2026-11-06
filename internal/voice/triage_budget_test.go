package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type budgetClassifier struct {
	delay    time.Duration
	failures int
	failure  error
	category string
	calls    int
	started  chan struct{}
}

func (f *budgetClassifier) Classify(ctx context.Context, _ string) (string, error) {
	f.calls++
	if f.started != nil {
		close(f.started)
	}
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if f.calls <= f.failures {
		return "", f.failure
	}
	return f.category, nil
}

func TestClassificationRemainingBudgetRouting(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		classifier            budgetClassifier
		category, reason      string
		attempts, jobs, tasks int
	}{
		{"late-recall", budgetClassifier{delay: 2200 * time.Millisecond, category: "recall"}, "recall", "none", 1, 1, 0},
		{"retry-error", budgetClassifier{failures: 1, failure: errors.New("private model error"), category: "recall"}, "recall", "none", 2, 1, 0},
		{"retry-timeout", budgetClassifier{failures: 1, failure: context.DeadlineExceeded, category: "recall"}, "recall", "none", 2, 1, 0},
		{"both-error", budgetClassifier{failures: 2, failure: errors.New("private model error")}, "unclassified", "triage_error", 2, 0, 1},
		{"both-timeout", budgetClassifier{failures: 2, failure: context.DeadlineExceeded}, "unclassified", "triage_timeout", 2, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, store, key := handoffHTTPServerWithConfig(t, Config{})
			const text = "private recall question"
			hash := sha256.Sum256([]byte(text))
			_, err := store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('budget-id','default','handoff-session',1,'budget-turn',1,?,?,?,'budget-commit')`, text, hash[:], time.Now().UTC().Format(time.RFC3339Nano))
			if err != nil {
				t.Fatal(err)
			}
			decision := &fakeSteward{}
			svc := newService(store.DB(), Config{Classifier: &tc.classifier, Steward: decision})
			defer svc.Stop()
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			svc.classifyCommittedTurn("budget-id")
			log.SetOutput(previous)
			var category string
			if err := store.DB().QueryRow(`SELECT triage_category FROM voice_turns WHERE id='budget-id'`).Scan(&category); err != nil {
				t.Fatal(err)
			}
			code, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "budget-handoff", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "budget-turn", "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])})
			if code != http.StatusOK {
				t.Fatalf("handoff: %d %s", code, raw)
			}
			var jobs, tasks int
			if err := store.DB().QueryRow(`SELECT (SELECT count(*) FROM voice_recall_jobs),(SELECT count(*) FROM tasks)`).Scan(&jobs, &tasks); err != nil {
				t.Fatal(err)
			}
			if category != tc.category || jobs != tc.jobs || tasks != tc.tasks {
				t.Errorf("category=%s jobs=%d tasks=%d; want %s,%d,%d", category, jobs, tasks, tc.category, tc.jobs, tc.tasks)
			}
			if tc.classifier.calls != tc.attempts {
				t.Errorf("attempts=%d want=%d", tc.classifier.calls, tc.attempts)
			}
			if decision.calls.Load() != 0 {
				t.Error("non-task classification called Steward")
			}
			if !strings.Contains(logs.String(), "reason="+tc.reason) || !strings.Contains(logs.String(), fmt.Sprintf("attempts=%d", tc.attempts)) || strings.Contains(logs.String(), text) || strings.Contains(logs.String(), "private model error") {
				t.Errorf("unsafe or missing classification diagnostics: %s", logs.String())
			}
		})
	}
}

func TestLateProgressUsesFreshSendTime(t *testing.T) {
	now := time.Now().UTC()
	var pushes []string
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		pushes = append(pushes, body["text"])
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()
	ts, store, key := handoffHTTPServerWithConfig(t, Config{})
	receipt := commitAndHandoff(t, ts, key, "seed-task", "please handle this")
	started := now.Add(-5 * time.Second).Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`UPDATE tasks SET status='active',started_at=? WHERE id=?`, started, receipt.ResourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gateway' WHERE id='handoff-session'`); err != nil {
		t.Fatal(err)
	}
	const text = "private progress question"
	hash := sha256.Sum256([]byte(text))
	if _, err := store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('progress-id','default','handoff-session',1,'progress-turn',1,?,?,?,'progress-commit')`, text, hash[:], now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	classifying := make(chan struct{})
	svc := newService(store.DB(), Config{Classifier: &budgetClassifier{delay: 2500 * time.Millisecond, category: "progress", started: classifying}, Now: func() time.Time { return now }, GatewayURL: gateway.URL, GatewayAPIKey: "fake-key", HTTPClient: gateway.Client()})
	defer svc.Stop()
	done := make(chan struct{})
	go func() { svc.classifyCommittedTurn("progress-id"); close(done) }()
	<-classifying
	unrelatedStarted := time.Now()
	unrelated := commitAndHandoff(t, ts, key, "unrelated-task", "please handle unrelated work")
	if time.Since(unrelatedStarted) >= time.Second {
		t.Error("slow progress delayed unrelated turn")
	}
	if _, err := store.DB().Exec(`UPDATE tasks SET status='complete' WHERE id=?`, unrelated.ResourceID); err != nil {
		t.Fatal(err)
	}
	<-done
	payload := map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "progress-turn", "revision": 1, "content_hash": fmt.Sprintf("%x", hash[:])}
	for _, id := range []string{"progress-handoff", "progress-retry"} {
		if code, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, id, "voice.turn.handoff", payload); code != http.StatusOK {
			t.Fatalf("handoff: %d %s", code, raw)
		}
	}
	now = now.Add(12 * time.Second)
	svc.dispatchFastReplies(context.Background())
	svc.dispatchFastReplies(context.Background())
	var tasks, replies int
	if err := store.DB().QueryRow(`SELECT (SELECT count(*) FROM tasks),(SELECT count(*) FROM voice_fast_replies WHERE turn_id='progress-turn')`).Scan(&tasks, &replies); err != nil {
		t.Fatal(err)
	}
	if tasks != 2 || replies != 1 || len(pushes) != 1 || !strings.Contains(pushes[0], "已经17秒") {
		t.Fatalf("late progress tasks=%d replies=%d sends=%q; want seed and unrelated tasks only, one fresh reply", tasks, replies, pushes)
	}
}
