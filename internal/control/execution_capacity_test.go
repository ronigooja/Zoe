package control

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func TestSnapshotExecutionCapacityRetainsUnknownSlotsAndExplainsQueue(t *testing.T) {
	store, server, key := instanceAuthFixture(t)
	ctx := context.Background()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	insertTask := func(id, owner string) {
		t.Helper()
		_, err := store.DB().ExecContext(ctx, `INSERT INTO tasks(id,workspace_id,owner_subject_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id) VALUES(?,'default',?,?,'goal','user','profile','queued',1,?,?,'instance','command')`, id, owner, id, at, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertTask("queued", "owner")
	insertTask("dependency", "owner")
	insertTask("hidden", "another-owner")
	for i, status := range []string{"active", "dispatching", "uncertain", "uncertain", "stopped", "verification_pending", "failed", "verified"} {
		_, err := store.DB().ExecContext(ctx, `INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,fence,authorized_device_id,command_id,goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at) VALUES(?,'default','hidden',?,?,?,'instance','command','goal','profile','runtime','model','worker','opencode',?)`, fmt.Sprint("run", i), fmt.Sprint("attempt", i), status, i+1, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() contracts.Snapshot {
		t.Helper()
		response := instanceRequest(server.Handler(), "GET", "/v1/snapshot", key, "")
		if response.Code != 200 {
			t.Fatalf("snapshot status=%d body=%s", response.Code, response.Body.String())
		}
		var out contracts.Snapshot
		if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Runs) != 0 {
			t.Fatal("snapshot exposed another owner's runs")
		}
		return out
	}
	out := snapshot()
	want := contracts.ExecutionCapacity{ActiveRuns: 2, UncertainRuns: 2, MaxRuns: 12, AvailableSlots: 8, DispatchBlocked: false}
	if out.ExecutionCapacity != want {
		t.Fatalf("capacity=%+v want %+v", out.ExecutionCapacity, want)
	}
	for _, task := range out.Tasks {
		if task.QueueReason != "pending_dispatch" {
			t.Fatalf("queued task=%+v", task)
		}
	}

	// Once unknown effects are authoritatively settled, slots become available.
	if _, err := store.DB().ExecContext(ctx, `UPDATE runs SET status='stopped' WHERE status='uncertain'`); err != nil {
		t.Fatal(err)
	}
	out = snapshot()
	if out.ExecutionCapacity.AvailableSlots != 10 || out.ExecutionCapacity.DispatchBlocked || out.Tasks[0].QueueReason != "pending_dispatch" {
		t.Fatalf("released snapshot=%+v", out)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE runs SET status='uncertain' WHERE id='run2'`); err != nil {
		t.Fatal(err)
	}
	out = snapshot()
	if out.ExecutionCapacity.UncertainRuns != 1 || out.ExecutionCapacity.AvailableSlots != 9 || out.ExecutionCapacity.DispatchBlocked {
		t.Fatalf("partial unknown capacity=%+v", out.ExecutionCapacity)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE runs SET status='active' WHERE id IN ('run2','run3')`); err != nil {
		t.Fatal(err)
	}
	if got := snapshot().ExecutionCapacity.ReasonCode; got != "" {
		t.Fatalf("ordinary saturation=%q", got)
	}

	if _, err := store.DB().ExecContext(ctx, `INSERT INTO task_dependencies(task_id,dependency_task_id,workspace_id,status,created_at) VALUES('queued','dependency','default','waiting',?)`, at); err != nil {
		t.Fatal(err)
	}
	for _, task := range snapshot().Tasks {
		if task.ID == "queued" && task.QueueReason != "dependencies_pending" {
			t.Fatalf("dependency queue=%+v", task)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE zoe_instance_state SET dispatch_enabled=0 WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	if got := snapshot().ExecutionCapacity.ReasonCode; got != "instance_dispatch_fenced" {
		t.Fatalf("fenced reason=%q", got)
	}
}

func TestConfiguredCapacityLimitChangesAvailableSlots(t *testing.T) {
	store, server, key := instanceAuthFixture(t)
	server.cfg.MaxRuns = 3
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,owner_subject_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id) VALUES('configured-task','default','owner','configured','goal','user','profile','active',1,?,?, 'instance','command')`, at, at); err != nil {
		t.Fatal(err)
	}
	for i, status := range []string{"active", "uncertain"} {
		if _, err := store.DB().Exec(`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,fence,authorized_device_id,command_id,goal,profile_id,runtime_route_id,model_route_id,role,harness,started_at) VALUES(?,'default','configured-task',?,?,?,'instance','command','goal','profile','runtime','model','worker','opencode',?)`, fmt.Sprintf("configured-%d", i), fmt.Sprintf("configured-attempt-%d", i), status, i+1, at); err != nil {
			t.Fatal(err)
		}
	}
	w := instanceRequest(server.Handler(), "GET", "/v1/snapshot", key, "")
	if w.Code != 200 {
		t.Fatalf("snapshot=%d %s", w.Code, w.Body.String())
	}
	var out contracts.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ExecutionCapacity.MaxRuns != 3 || out.ExecutionCapacity.AvailableSlots != 1 {
		t.Fatalf("capacity=%+v", out.ExecutionCapacity)
	}
}

func TestClaimCapacityUsesConfiguredLimit(t *testing.T) {
	if executionCapacityAvailable(2, 3) != true || executionCapacityAvailable(3, 3) != false || executionCapacityAvailable(4, 3) != false {
		t.Fatal("claim capacity did not follow configured limit")
	}
}
