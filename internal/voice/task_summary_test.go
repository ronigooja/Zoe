package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/steward"
)

func TestTaskSummaryDurableSweep(t *testing.T) {
	for _, output := range []string{"Asked for budget; established costs.", "", strings.Repeat("中", 200)} {
		t.Run(output, func(t *testing.T) {
			db, _, now := rosterFixture(t)
			candidateSeed(t, db, now, rosterSeed{id: "summary", status: "active", result: "full authoritative budget result"}, "budget goal")
			rosterExec(t, db, `INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES
('addition-2','summary','default','user_message','second addition','default-owner',?),
('addition-1','summary','default','user_message','first addition','default-owner',?)`, now.Add(-time.Hour).Format(time.RFC3339Nano), now.Add(-2*time.Hour).Format(time.RFC3339Nano))
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				encoded, _ := json.Marshal(body)
				if !strings.Contains(string(encoded), "full authoritative budget result") || !strings.Contains(string(encoded), "budget goal") {
					t.Error("missing source facts")
				}
				if strings.Index(string(encoded), "first addition") < 0 || strings.Index(string(encoded), "first addition") > strings.Index(string(encoded), "second addition") {
					t.Error("missing or unordered additions")
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"output_text": output})
			}))
			defer server.Close()
			cfg := RegisterModelConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"}
			r, err := NewRegister(db, RegisterConfig{Model: cfg, CompactModel: cfg, CompactCharacters: 1000, TaskSummaryInstruction: testPrompts(t).TaskSummary()})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := r.summarizeTasks(ctx, now); err != nil || calls != 0 {
				t.Fatalf("active calls=%d err=%v", calls, err)
			}
			rosterExec(t, db, `UPDATE tasks SET status='complete' WHERE id='summary'`)
			// Reopen the database to exercise restart recovery of unsummarized tasks.
			path := filepath.Join(t.TempDir(), "restart.db")
			rosterExec(t, db, `VACUUM INTO ?`, path)
			reopened, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			r.db = reopened
			for i := 0; i < 2; i++ {
				if err := r.summarizeTasks(ctx, now); err != nil {
					t.Fatal(err)
				}
			}
			var summary sql.NullString
			var result string
			if err := reopened.QueryRow(`SELECT summary_result,result FROM tasks WHERE id='summary'`).Scan(&summary, &result); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !summary.Valid || summary.String != output || result != "full authoritative budget result" {
				t.Fatalf("calls=%d summary=%+v result=%q", calls, summary, result)
			}
		})
	}
}

func TestTaskSummaryBackoffAndAuthentication(t *testing.T) {
	db, _, now := rosterFixture(t)
	candidateSeed(t, db, now, rosterSeed{id: "summary", status: "complete", result: "full result"}, "budget")
	status, calls := 503, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(status) }))
	defer server.Close()
	cfg := RegisterModelConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"}
	r, err := NewRegister(db, RegisterConfig{Model: cfg, CompactModel: cfg, CompactCharacters: 1000, TaskSummaryInstruction: testPrompts(t).TaskSummary()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now = time.Now()
	if err := r.summarizeTasks(ctx, now); err == nil {
		t.Fatal("expected model failure")
	}
	var summary sql.NullString
	if err := db.QueryRow(`SELECT summary_result FROM tasks WHERE id='summary'`).Scan(&summary); err != nil || summary.Valid {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	states, err := (backoff.Store{DB: db}).List(ctx)
	if err != nil || len(states) != 1 || states[0].Failures != 1 || !states[0].NextAttemptAt.After(now) {
		t.Fatalf("states=%+v err=%v", states, err)
	}
	restarted, err := NewRegister(db, RegisterConfig{Model: cfg, CompactModel: cfg, CompactCharacters: 1000, TaskSummaryInstruction: testPrompts(t).TaskSummary()})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.summarizeTasks(ctx, now); err != nil || calls != 1 {
		t.Fatalf("early retry calls=%d err=%v", calls, err)
	}
	status = 401
	if err := restarted.summarizeTasks(ctx, states[0].NextAttemptAt); err == nil {
		t.Fatal("expected auth failure")
	}
	if err := restarted.summarizeTasks(ctx, now.Add(24*time.Hour)); err != nil || calls != 2 {
		t.Fatalf("auth retry calls=%d err=%v", calls, err)
	}
}

func TestCandidateSummaryFallback(t *testing.T) {
	for _, tc := range []struct {
		name, status, result, want string
		summary                    any
	}{
		{"NULL summary", "complete", "full original budget result\n", `"full original budget result\n"`, nil},
		{"stored summary", "complete", "full original budget result", `"budget summary"`, "budget summary"},
		{"empty summary", "complete", "full original budget result", `""`, ""},
		{"no result", "active", "", `null`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, svc, now := rosterFixture(t)
			candidateSeed(t, db, now, rosterSeed{id: "summary", status: tc.status, result: tc.result}, "budget")
			rosterExec(t, db, `UPDATE tasks SET summary_result=? WHERE id='summary'`, tc.summary)
			candidates, err := svc.taskCandidates(context.Background(), db, "default", "owner", "budget", now, 8)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates=%+v err=%v", candidates, err)
			}
			c := candidates[0]
			data, err := json.Marshal(steward.Candidate{TaskID: c.TaskID, Goal: c.Goal, Status: c.Status, SummaryResult: c.SummaryResult})
			if err != nil || strings.Contains(string(data), `"result":`) || !strings.Contains(string(data), `"summary_result":`+tc.want+`,`) {
				t.Fatalf("input=%s err=%v", data, err)
			}
			if c.Result != tc.result {
				t.Fatal("authoritative result lost")
			}
		})
	}
}
