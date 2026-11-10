package voice

import (
	"testing"
	"time"
)

func TestProgressSaysStoppingWhenStopRequestedButNotConfirmed(t *testing.T) {
	now := time.Date(2026, 11, 6, 1, 0, 10, 0, time.UTC)
	classifier := &fakeClassifier{category: "progress"}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Classifier: classifier, Now: func() time.Time { return now }})
	at := now.Add(-10 * time.Second).Format(time.RFC3339Nano)
	db := store.DB()
	_, _ = db.Exec(`INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES('task-device','default','owner','task','approved',?)`, at)
	_, _ = db.Exec(`INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES('task-cmd','default','owner','task-key',X'01','{}',?)`, at)
	_, _ = db.Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,started_at,created_by_device_id,command_id,owner_subject_id) VALUES('stopping','default','stopping','stopping','voice','zoe-steward','active',1,?,?,?,?,?,?)`, at, at, at, "task-device", "task-cmd", "owner")
	_, _ = db.Exec(`INSERT INTO voice_task_links(workspace_id,session_id,generation,turn_id,turn_revision,task_id,linked_by_device_id,command_id,linked_at) VALUES('default','handoff-session',1,'old',1,'stopping','instance','task-cmd',?)`, at)
	if _, err := db.Exec(`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,fence,authorized_device_id,command_id,goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at) VALUES('stopping-run','default','stopping','stopping-attempt','active','stop',9001,'task-device','task-cmd','goal','zoe-steward','runtime','model','worker','opencode',?)`, at); err != nil {
		t.Fatal(err)
	}
	commitAndHandoff(t, ts, key, "progress-stopping", "is it done")
	var text string
	if err := db.QueryRow(`SELECT text FROM voice_fast_replies`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "已经请求停止，正在停下。" {
		t.Fatalf("text=%q", text)
	}
}
