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

type rosterSeed struct {
	id, status, workspace, owner, session, interaction, question, result string
	generation                                                           int64
	created, updated                                                     time.Time
	started, finished                                                    *time.Time
}

func rosterFixture(t *testing.T) (*sql.DB, *Service, time.Time) {
	t.Helper()
	store, svc := newVoiceTestService(t, Config{})
	db := store.DB()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	at := now.Format(time.RFC3339Nano)
	rosterExec(t, db, `INSERT INTO workspaces(id,name,created_at) VALUES('other-workspace','Other',?)`, at)
	for _, workspace := range []string{"default", "other-workspace"} {
		for _, owner := range []string{"owner", "other-owner"} {
			rosterExec(t, db, `INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES(?,?,?,?,'approved',?)`, workspace+"-"+owner, workspace, owner, owner, at)
		}
	}
	for _, session := range []struct{ id, workspace string }{{"call", "default"}, {"other-call", "default"}, {"workspace-call", "other-workspace"}} {
		rosterExec(t, db, `INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES(?,?,'owner','device',?,X'01',1,'ended',?,?)`, session.id, session.workspace, session.id, at, at)
	}
	return db, svc, now
}

func rosterExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func seedRosterTask(t *testing.T, db *sql.DB, now time.Time, task rosterSeed) {
	t.Helper()
	if task.workspace == "" {
		task.workspace = "default"
	}
	if task.owner == "" {
		task.owner = "owner"
	}
	if task.session == "" {
		task.session = "call"
	}
	if task.generation == 0 {
		task.generation = 1
	}
	if task.interaction == "" {
		task.interaction = "none"
	}
	if task.created.IsZero() {
		task.created = now.Add(-3 * time.Hour)
	}
	if task.updated.IsZero() {
		task.updated = task.created
	}
	stamp := func(at *time.Time) any {
		if at == nil {
			return nil
		}
		return at.UTC().Format(time.RFC3339Nano)
	}
	at := now.Format(time.RFC3339Nano)
	rosterExec(t, db, `INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES(?,?,?,?,X'01','{}',?)`, "cmd-"+task.id, task.workspace, task.owner, "key-"+task.id, at)
	rosterExec(t, db, `INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,started_at,finished_at,created_by_device_id,command_id,owner_subject_id,interaction_state,interaction_question,result) VALUES(?,?,?,'goal','voice','default',?,1,?,?,?,?,?,?,?,?,?,?)`, task.id, task.workspace, "Title "+task.id, task.status, task.created.UTC().Format(time.RFC3339Nano), task.updated.UTC().Format(time.RFC3339Nano), stamp(task.started), stamp(task.finished), task.workspace+"-"+task.owner, "cmd-"+task.id, task.owner, task.interaction, task.question, task.result)
	rosterExec(t, db, `INSERT INTO voice_task_links(workspace_id,session_id,generation,turn_id,turn_revision,task_id,linked_by_device_id,command_id,linked_at) VALUES(?,?,?,?,1,?,'device',?,?)`, task.workspace, task.session, task.generation, "turn-"+task.id, task.id, "cmd-"+task.id, at)
}

func TestRosterScopeStatusesAndFields(t *testing.T) {
	db, svc, now := rosterFixture(t)
	recent, old, future := now.Add(-30*time.Minute), now.Add(-2*time.Hour), now.Add(time.Minute)
	boundary := now.Add(-time.Hour)
	long := strings.Repeat("完整结果", 300)
	seeds := []rosterSeed{
		{id: "queued", status: "queued", question: "hidden", result: "hidden"},
		{id: "active", status: "active", interaction: "awaiting_message", question: "hidden", result: "hidden"},
		{id: "waiting", status: "waiting", interaction: "awaiting_message", question: long},
		{id: "awaiting", status: "awaiting_user", interaction: "explicit_confirmation", question: "Confirm?"},
		{id: "waiting-none", status: "waiting", question: "hidden"},
		{id: "waiting-approval", status: "waiting", interaction: "approval_required", question: "hidden"},
		{id: "complete-recent", status: "complete", finished: &recent, question: "hidden", result: long},
		{id: "complete-fallback", status: "complete", updated: recent},
		{id: "complete-boundary", status: "complete", finished: &boundary},
		{id: "complete-old", status: "complete", finished: &old, updated: recent},
		{id: "complete-old-fallback", status: "complete", updated: old},
		{id: "complete-future", status: "complete", finished: &future},
		{id: "failed", status: "failed"},
		{id: "canceled", status: "canceled"},
		{id: "paused", status: "paused"},
		{id: "other-owner", status: "active", owner: "other-owner"},
		{id: "other-session", status: "active", session: "other-call"},
		{id: "other-generation", status: "active", generation: 2},
		{id: "other-workspace", status: "active", workspace: "other-workspace"},
		{id: "mismatched-link-workspace", status: "active"},
	}
	var want []callRosterTask
	for i, seed := range seeds {
		seed.created = now.Add(time.Duration(-180+i) * time.Minute)
		if i == 0 {
			// A very old active call task is still included; started_at wins over created_at.
			seed.created = now.Add(-48 * time.Hour)
			started := now.Add(-time.Minute)
			seed.started = &started
		}
		seedRosterTask(t, db, now, seed)
		if seed.id == "mismatched-link-workspace" {
			rosterExec(t, db, `UPDATE voice_task_links SET workspace_id='other-workspace' WHERE task_id=?`, seed.id)
		}
		if i < 9 {
			item := callRosterTask{ID: seed.id, Title: "Title " + seed.id, Status: seed.status}
			switch seed.id {
			case "waiting", "awaiting":
				item.Question = seed.question
			case "complete-recent":
				item.Result = long
			}
			if i == 0 {
				want = append(want, item)
			} else {
				want = append(want[:1], append([]callRosterTask{item}, want[1:]...)...)
			}
		}
	}
	// SQLite query_only rejects writes, even if the supplied transaction permits them.
	rosterExec(t, db, `PRAGMA query_only=ON`)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := svc.callRoster(context.Background(), tx, "default", "owner", "call", 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roster=%+v; want=%+v", got, want)
	}
}

func TestRosterLimitAndOrder(t *testing.T) {
	db, svc, now := rosterFixture(t)
	var want []callRosterTask
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("task-%02d", i)
		seedRosterTask(t, db, now, rosterSeed{id: id, status: "active", created: now.Add(time.Duration(-i) * time.Minute)})
		if i < 10 {
			want = append(want, callRosterTask{ID: id, Title: "Title " + id, Status: "active"})
		}
	}
	got, err := svc.callRoster(context.Background(), db, "default", "owner", "call", 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roster=%+v; want=%+v", got, want)
	}
}

func TestRosterEmptyAndQueryError(t *testing.T) {
	db, svc, now := rosterFixture(t)
	got, err := svc.callRoster(context.Background(), db, "default", "owner", "call", 1, now)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty roster=%+v, err=%v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.callRoster(ctx, db, "default", "owner", "call", 1, now); err == nil {
		t.Fatal("expected canceled query error")
	}
}
