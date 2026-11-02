package control

import (
	"context"
	"testing"
	"time"
)

func TestExpireRuntimeActionsUpdatesOnlyLiveAwaitingTaskAndIsIdempotent(t *testing.T) {
	store, server, _ := instanceAuthFixture(t)
	now := time.Date(2026, 10, 24, 14, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour).Format(time.RFC3339Nano)
	for i, task := range []struct{ id, status, interaction string }{
		{"live-question", "active", "awaiting_message"},
		{"done-question", "complete", "awaiting_message"},
		{"canceled-question", "canceled", "awaiting_message"},
	} {
		if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id,interaction_state,interaction_run_id,interaction_question) VALUES(?,?,?,?,'user','profile',?,1,?,?,?,'cmd','owner',?,?,?)`, task.id, "default", task.id, "goal", task.status, at, at, "instance", task.interaction, "run-"+task.id, "What?"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "run-"+task.id, "default", task.id, "attempt-"+task.id, map[bool]string{true: "active", false: "stopped"}[task.status == "active"], "run", 1, i+1, "runner", "instance", "cmd", "goal", "profile", "opencode", "runtime", "model", "worker", at); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO pending_runtime_actions(id,workspace_id,task_id,run_id,fence,request_id,kind,question,action_digest,expires_at,created_at) VALUES(?,?,?,?,1,?,?,?,?,?,?)`, "action-"+task.id, "default", task.id, "run-"+task.id, "request", map[bool]string{true: "worker_question", false: "worker_question"}[true], "What?", "digest-"+task.id, now.Add(-time.Minute).Format(time.RFC3339Nano), at); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.ExpireRuntimeActions(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var status, state, expiredKind string
	var version int
	if err := store.DB().QueryRow(`SELECT status,interaction_state,interaction_expired_kind,version FROM tasks WHERE id='live-question'`).Scan(&status, &state, &expiredKind, &version); err != nil {
		t.Fatal(err)
	}
	if status != "awaiting_user" || state != "awaiting_message" || expiredKind != "question" || version != 2 {
		t.Fatalf("live task=%s/%s/%s version=%d", status, state, expiredKind, version)
	}
	if err := server.ExpireRuntimeActions(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT version FROM tasks WHERE id='live-question'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("expiry was not idempotent: version=%d", version)
	}
	if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id='done-question'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" {
		t.Fatalf("terminal task changed to %s", status)
	}
}

func TestExpireRuntimeActionsStopsRunAndBumpsControlVersion(t *testing.T) {
	store, server, _ := instanceAuthFixture(t)
	now := time.Date(2026, 10, 24, 14, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour).Format(time.RFC3339Nano)
	_, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id,interaction_state,interaction_run_id,interaction_question) VALUES('permission-task','default','title','goal','user','profile','active',4,?,?, 'instance','cmd','owner','explicit_confirmation','permission-run','Allow?')`, at, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().Exec(`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('permission-run','default','permission-task','attempt','active','run',7,1,'runner','instance','cmd','goal','profile','opencode','runtime','model','worker',?)`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().Exec(`INSERT INTO pending_runtime_actions(id,workspace_id,task_id,run_id,fence,request_id,kind,question,action_digest,expires_at,created_at) VALUES('permission-action','default','permission-task','permission-run',1,'request','permission_requested','Allow?','digest',?,?)`, now.Add(-time.Minute).Format(time.RFC3339Nano), at)
	if err != nil {
		t.Fatal(err)
	}
	if err = server.ExpireRuntimeActions(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var desired string
	var controlVersion int
	if err = store.DB().QueryRow(`SELECT desired_control,control_version FROM runs WHERE id='permission-run'`).Scan(&desired, &controlVersion); err != nil {
		t.Fatal(err)
	}
	if desired != "stop" || controlVersion != 8 {
		t.Fatalf("control=%s/%d", desired, controlVersion)
	}
	var decision string
	if err = store.DB().QueryRow(`SELECT decision FROM pending_runtime_actions WHERE id='permission-action'`).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if decision != "reject" {
		t.Fatalf("permission decision=%s", decision)
	}
}
