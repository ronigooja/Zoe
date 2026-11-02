package control

import (
	"context"
	"testing"
	"time"
)

func TestExpireRuntimeActionsWorksWithMainInteractionStateCheck(t *testing.T) {
	store, server, _ := instanceAuthFixture(t)
	now := time.Date(2026, 10, 24, 14, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id,interaction_state,interaction_run_id,interaction_question) VALUES('schema-task','default','title','goal','user','profile','active',1,?,?,?,'cmd','owner','awaiting_message','schema-run','Question?')`, at, at, "instance"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('schema-run','default','schema-task','schema-attempt','active','run',1,1,'runner','instance','cmd','goal','profile','opencode','runtime','model','worker',?)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO pending_runtime_actions(id,workspace_id,task_id,run_id,fence,request_id,kind,question,action_digest,expires_at,created_at) VALUES('schema-action','default','schema-task','schema-run',1,'request','worker_question','Question?','digest',?,?)`, now.Add(-time.Minute).Format(time.RFC3339Nano), at); err != nil {
		t.Fatal(err)
	}
	if err := server.ExpireRuntimeActions(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var status, interaction, expiredKind string
	var version int
	if err := store.DB().QueryRow(`SELECT status,interaction_state,interaction_expired_kind,version FROM tasks WHERE id='schema-task'`).Scan(&status, &interaction, &expiredKind, &version); err != nil {
		t.Fatal(err)
	}
	if status != "awaiting_user" || interaction != "awaiting_message" || expiredKind != "question" || version != 2 {
		t.Fatalf("task=%s/%s/%s version=%d", status, interaction, expiredKind, version)
	}
}
