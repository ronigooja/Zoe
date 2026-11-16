package voice

import (
	"context"
	"strings"
	"testing"
)

func TestUIResultSourcePinnedAcrossLaterResult(t *testing.T) {
	r := uiTargetRig(t)
	r.seedTask(t, "task", "owner")
	r.exec(t, `UPDATE tasks SET ui_display=1,result='mutable A' WHERE id='task'`)
	r.exec(t, `INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,fence,runner_id,authorized_device_id,command_id,goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at) VALUES('run','default','task','attempt','stopped',1,'runner','device-1','cmd','goal','profile','runtime','model','worker','opencode','now')`)
	r.seedEntry(t, "task", "A", "authoritative A")
	r.seedEvent(t, 1, "task", "run", "A")
	r.svc.consumeUITaskEvents(context.Background())
	var request string
	if err := r.store.DB().QueryRow(`SELECT request_id FROM ui_result_sources WHERE entry_id='A'`).Scan(&request); err != nil {
		t.Fatal(err)
	}
	r.seedEntry(t, "task", "B", "authoritative B")
	r.seedEvent(t, 2, "task", "run", "B")
	r.exec(t, `UPDATE tasks SET result='mutable B' WHERE id='task'`)
	input, err := r.svc.uiInput(context.Background(), uiJob{w: "default", o: "owner", request: request})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(input), "authoritative A") || strings.Contains(string(input), "authoritative B") || strings.Contains(string(input), "mutable B") {
		t.Fatal(string(input))
	}
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,task_id,created_at,updated_at) VALUES('redisplay','default','owner','task','task','now','now')`)
	tx, err := r.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pinUIResultTx(context.Background(), tx, "default", "owner", "task", "redisplay", "", 0); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	result, err := r.svc.uiPinnedResult(context.Background(), "default", "owner", "task", "redisplay")
	if err != nil || *result != "authoritative B" {
		t.Fatalf("latest=%v err=%v", result, err)
	}
	if _, err := r.svc.uiPinnedResult(context.Background(), "default", "other", "task", "redisplay"); err == nil {
		t.Fatal("crossowner result")
	}
	r.exec(t, `UPDATE ui_result_sources SET event_cursor=999 WHERE request_id='redisplay'`)
	if _, err := r.svc.uiPinnedResult(context.Background(), "default", "owner", "task", "redisplay"); err == nil {
		t.Fatal("forged binding")
	}
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,task_id,created_at,updated_at) VALUES('unbound','default','owner','task','task','now','now')`)
	if _, err := r.svc.uiInput(context.Background(), uiJob{w: "default", o: "owner", request: "unbound"}); err == nil || err.Error() != "task_result_pending" {
		t.Fatalf("unbound=%v", err)
	}
}
