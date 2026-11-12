package voice

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func candidateSeed(t *testing.T, db *sql.DB, now time.Time, seed rosterSeed, title string) {
	t.Helper()
	seedRosterTask(t, db, now, seed)
	rosterExec(t, db, `UPDATE tasks SET title=? WHERE id=?`, title, seed.id)
}

func candidateRequests(got []taskCandidate) []string {
	var out []string
	for _, c := range got {
		out = append(out, c.Request)
	}
	return out
}

func TestCandidatesTermsAndWildcardEscaping(t *testing.T) {
	want := []string{"report", "2026", "研究", "究进", "进展", "中", "abc", "def"}
	if got := candidateTerms("Please the REPORT 2026 研究进展 中 abc%def_ a I"); !reflect.DeepEqual(got, want) {
		t.Fatalf("terms=%q want=%q", got, want)
	}
	if got := candidateTerms(strings.Repeat("report ", 30)); !reflect.DeepEqual(got, []string{"report"}) {
		t.Fatalf("duplicate terms=%q", got)
	}
	var words []string
	for i := 0; i < 30; i++ {
		words = append(words, fmt.Sprintf("word%d", i))
	}
	if got := candidateTerms(strings.Join(words, " ")); len(got) != 24 {
		t.Fatalf("term cap=%d", len(got))
	}
	db, svc, now := rosterFixture(t)
	var match bool
	if err := db.QueryRow(`SELECT ? LIKE ? ESCAPE '\'`, "literal%_\\", candidateLikePattern("%_\\")).Scan(&match); err != nil || !match {
		t.Fatalf("literal LIKE match=%v err=%v", match, err)
	}
	if err := db.QueryRow(`SELECT ? LIKE ? ESCAPE '\'`, "anything", candidateLikePattern("%_")).Scan(&match); err != nil || match {
		t.Fatalf("wildcard escaped=%v err=%v", match, err)
	}
	candidateSeed(t, db, now, rosterSeed{id: "match", status: "complete"}, "budget")
	got, err := svc.taskCandidates(context.Background(), db, "default", "owner", "%_", now, 8)
	if err != nil || len(got) != 0 {
		t.Fatalf("punctuation query=%+v err=%v", got, err)
	}
}

func TestCandidatesChineseEnglishAndCrossSession(t *testing.T) {
	db, svc, now := rosterFixture(t)
	candidateSeed(t, db, now, rosterSeed{id: "chinese", status: "complete", session: "other-call"}, "调查项目研究进展")
	candidateSeed(t, db, now, rosterSeed{id: "english", status: "complete", result: "BUDGET report available"}, "other")
	for _, tc := range []struct{ text, want string }{{"项目研究", "调查项目研究进展"}, {"Please report the budget", "other"}} {
		got, err := svc.taskCandidates(context.Background(), db, "default", "owner", tc.text, now, 8)
		if err != nil || len(got) != 1 || got[0].Request != tc.want {
			t.Fatalf("%q got=%+v err=%v", tc.text, got, err)
		}
	}
}

func TestCandidatesTitleWeightRankingAndNewestPresentation(t *testing.T) {
	db, svc, now := rosterFixture(t)
	candidateSeed(t, db, now, rosterSeed{id: "title", status: "complete", created: now.Add(-4 * time.Hour)}, "budget")
	candidateSeed(t, db, now, rosterSeed{id: "result", status: "complete", result: "budget", created: now.Add(-2 * time.Hour)}, "unrelated")
	got, err := svc.taskCandidates(context.Background(), db, "default", "owner", "budget", now, 1)
	if err != nil || len(got) != 1 || got[0].Request != "budget" {
		t.Fatalf("title weight got=%+v err=%v", got, err)
	}
	got, err = svc.taskCandidates(context.Background(), db, "default", "owner", "budget", now, 2)
	if err != nil || !reflect.DeepEqual(candidateRequests(got), []string{"unrelated", "budget"}) {
		t.Fatalf("newest presentation got=%+v err=%v", got, err)
	}
	candidateSeed(t, db, now, rosterSeed{id: "new-title", status: "complete", created: now.Add(-3 * time.Hour)}, "budget latest")
	got, err = svc.taskCandidates(context.Background(), db, "default", "owner", "budget", now, 1)
	if err != nil || len(got) != 1 || got[0].Request != "budget latest" {
		t.Fatalf("score tie recency got=%+v err=%v", got, err)
	}
}

func TestCandidatesOwnerStatusAndAgeFilters(t *testing.T) {
	db, svc, now := rosterFixture(t)
	seeds := []rosterSeed{
		{id: "queued", status: "queued"}, {id: "active", status: "active"},
		{id: "waiting", status: "waiting"}, {id: "awaiting", status: "awaiting_user"},
		{id: "complete", status: "complete", created: now.Add(-30 * 24 * time.Hour)},
		{id: "old", status: "complete", created: now.Add(-30*24*time.Hour - time.Second)},
		{id: "future", status: "active", created: now.Add(time.Minute)},
		{id: "failed", status: "failed"}, {id: "canceled", status: "canceled"}, {id: "paused", status: "paused"},
		{id: "other-owner", status: "active", owner: "other-owner"},
		{id: "other-workspace", status: "active", workspace: "other-workspace"},
	}
	for _, seed := range seeds {
		candidateSeed(t, db, now, seed, "budget "+seed.id)
	}
	got, err := svc.taskCandidates(context.Background(), db, "default", "owner", "budget", now, 20)
	if err != nil || len(got) != 5 {
		t.Fatalf("filters got=%+v err=%v", got, err)
	}
	for _, c := range got {
		if strings.Contains(c.Request, "other") || c.Request == "budget old" || c.Request == "budget future" {
			t.Fatalf("unexpected candidate=%+v", c)
		}
	}
}

func TestCandidatesAlwaysIncludeLatestSessionEmptyTermsAndDedup(t *testing.T) {
	db, svc, now := rosterFixture(t)
	rosterExec(t, db, `UPDATE voice_sessions SET created_at=? WHERE id='other-call'`, now.Add(-30*time.Minute).Format(time.RFC3339Nano))
	rosterExec(t, db, `UPDATE voice_sessions SET created_at=? WHERE id='call'`, now.Add(-2*time.Hour).Format(time.RFC3339Nano))
	for i := 0; i < 7; i++ {
		candidateSeed(t, db, now, rosterSeed{id: fmt.Sprintf("running-%d", i), status: "active", session: "other-call", created: now.Add(time.Duration(-i-1) * time.Minute)}, fmt.Sprintf("budget %d", i))
	}
	for _, seed := range []rosterSeed{
		{id: "old-session", status: "active", created: now.Add(-time.Minute)},
		{id: "too-old", status: "queued", session: "other-call", created: now.Add(-time.Hour - time.Second)},
		{id: "done", status: "complete", session: "other-call", created: now.Add(-time.Minute)},
		{id: "foreign", status: "active", owner: "other-owner", session: "other-call", created: now.Add(-time.Minute)},
	} {
		candidateSeed(t, db, now, seed, "unrelated "+seed.id)
	}
	for _, text := range []string{"", "the is %_", "budget"} {
		got, err := svc.taskCandidates(context.Background(), db, "default", "owner", text, now, 5)
		if err != nil || !reflect.DeepEqual(candidateRequests(got), []string{"budget 0", "budget 1", "budget 2", "budget 3", "budget 4"}) {
			t.Fatalf("text=%q always=%+v err=%v", text, got, err)
		}
	}
	got, err := svc.taskCandidates(context.Background(), db, "default", "owner", "budget", now, 8)
	if err != nil || len(got) != 7 {
		t.Fatalf("dedup got=%+v err=%v", got, err)
	}
	got, err = svc.taskCandidates(context.Background(), db, "default", "owner", "", now, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("small limit got=%+v err=%v", got, err)
	}
}

func TestCandidatesLimitsAndBoundedScan(t *testing.T) {
	db, svc, now := rosterFixture(t)
	for i := 0; i < 501; i++ {
		title := "budget"
		if i == 500 {
			title = "uniqueolder"
		}
		candidateSeed(t, db, now, rosterSeed{id: fmt.Sprintf("task-%03d", i), status: "complete", created: now.Add(-time.Duration(i+120) * time.Minute)}, title)
	}
	for _, tc := range []struct{ limit, want int }{{0, 8}, {-1, 8}, {100, 20}, {3, 3}} {
		got, err := svc.taskCandidates(context.Background(), db, "default", "owner", "budget", now, tc.limit)
		if err != nil || len(got) != tc.want {
			t.Fatalf("limit=%d count=%d err=%v", tc.limit, len(got), err)
		}
	}
	got, err := svc.taskCandidates(context.Background(), db, "default", "owner", "uniqueolder", now, 8)
	if err != nil || len(got) != 0 {
		t.Fatalf("documented 500-row recall boundary got=%+v err=%v", got, err)
	}
}

func TestCandidatesFieldsAdditionsAndNullHandling(t *testing.T) {
	db, svc, now := rosterFixture(t)
	finished := now.Add(-time.Hour)
	for _, seed := range []rosterSeed{
		{id: "waiting", status: "waiting", interaction: "awaiting_message", question: "Which budget?", result: strings.Repeat("中", 650)},
		{id: "confirmation", status: "awaiting_user", interaction: "explicit_confirmation", question: "Confirm?"},
		{id: "hidden", status: "active", interaction: "awaiting_message", question: "hidden"},
		{id: "approval", status: "waiting", interaction: "approval_required", question: "hidden"},
		{id: "complete", status: "complete", finished: &finished},
		{id: "null-title", status: "complete"},
	} {
		candidateSeed(t, db, now, seed, "budget "+seed.id)
	}
	for i := 0; i < 6; i++ {
		kind := "user_message"
		if i == 5 {
			kind = "system"
		}
		rosterExec(t, db, `INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES(?,'waiting','default',?,?,'default-owner',?)`, fmt.Sprintf("entry-%d", i), kind, fmt.Sprintf("%d", i)+strings.Repeat("文", 350), now.Add(time.Duration(-180+i)*time.Minute).Format(time.RFC3339Nano))
	}
	// Nullability is tested against a real SQLite temporary shadow of tasks;
	// production's title/result/question columns are currently NOT NULL.
	rosterExec(t, db, `CREATE TEMP TABLE tasks AS SELECT * FROM main.tasks`)
	rosterExec(t, db, `UPDATE tasks SET result=NULL,interaction_question=NULL,finished_at=NULL WHERE id='hidden'`)
	rosterExec(t, db, `UPDATE tasks SET title=NULL WHERE id='null-title'`)
	rosterExec(t, db, `PRAGMA query_only=ON`)
	got, err := svc.taskCandidates(context.Background(), db, "default", "owner", "budget", now, 20)
	if err != nil || len(got) != 5 {
		t.Fatalf("fields got=%+v err=%v", got, err)
	}
	for _, c := range got {
		if _, err := time.Parse(time.RFC3339, c.CreatedAt); err != nil {
			t.Fatal(err)
		}
		if c.Request == "budget waiting" {
			if c.Question != "Which budget?" || len([]rune(c.Result)) != 600 || len(c.Additions) != 3 || len([]rune(c.Additions[0])) != 300 || !strings.HasPrefix(c.Additions[0], "2") || !strings.HasPrefix(c.Additions[2], "4") {
				t.Fatalf("waiting facts=%+v", c)
			}
		} else if c.Request == "budget confirmation" {
			if c.Question != "Confirm?" {
				t.Fatalf("confirmation facts=%+v", c)
			}
		} else if c.Request == "budget complete" {
			if c.FinishedAt != finished.Format(time.RFC3339Nano) {
				t.Fatalf("finished facts=%+v", c)
			}
		} else if c.Question != "" || c.FinishedAt != "" {
			t.Fatalf("hidden/null facts=%+v", c)
		}
	}
}

func TestCandidatesKnownSynonymRecallLimit(t *testing.T) {
	db, svc, now := rosterFixture(t)
	candidateSeed(t, db, now, rosterSeed{id: "synonym", status: "complete"}, "医生")
	got, err := svc.taskCandidates(context.Background(), db, "default", "owner", "大夫", now, 8)
	if err != nil || len(got) != 0 {
		t.Fatalf("no shared bigram: synonym is intentionally not found, got=%+v err=%v", got, err)
	}
}

func TestCandidatesEmptyAndQueryError(t *testing.T) {
	db, svc, now := rosterFixture(t)
	got, err := svc.taskCandidates(context.Background(), db, "default", "owner", "budget", now, 8)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty=%+v err=%v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.taskCandidates(ctx, db, "default", "owner", "budget", now, 8); err == nil {
		t.Fatal("expected canceled query error")
	}
}
