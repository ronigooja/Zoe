package voice

import (
	"database/sql"
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
