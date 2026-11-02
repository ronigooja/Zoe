package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/runner"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestProductionControlClientReadsAnswerAndConsumesQuestionAction(t *testing.T) {
	ctx := context.Background()
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 32))
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES('device','default','owner','test','approved',?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) VALUES('answer-task','default','question','goal','user','profile','awaiting_user',2,?,?,?,?,?)`, stamp, stamp, "device", "command", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('answer-run','default','answer-task','answer-attempt','active','run',1,8,'configured-runner','device','command','goal','profile','opencode','runtime','model','worker',?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO pending_runtime_actions(id,workspace_id,task_id,run_id,fence,request_id,kind,question,message,action_digest,decision,expires_at,created_at) VALUES('answer-action','default','answer-task','answer-run',8,'frm-real','worker_question','red or blue','blue','answer-digest','answer',?,?)`, now.Add(time.Hour).Format(time.RFC3339Nano), stamp); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, Config{RunnerToken: token, RunnerID: "configured-runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:control:read"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterRunnerCredential(ctx, token, "configured-runner", []string{"default"}, []string{"runner:control:read"}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, err := runner.NewControlClient(httpServer.URL, token, httpServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	auth, err := client.RuntimeAuthorization(ctx, "answer-run", "frm-real", 8)
	if err != nil {
		t.Fatal(err)
	}
	if auth == nil || auth.Decision != "answer" || auth.Message != "blue" || auth.RequestID != "frm-real" {
		t.Fatalf("authorization=%+v, want answer message from pending action", auth)
	}
	var used int
	if err := store.DB().QueryRowContext(ctx, `SELECT authorization_used FROM pending_runtime_actions WHERE id='answer-action'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 1 {
		t.Fatalf("authorization_used=%d, want 1", used)
	}
	var remaining int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE task_id='answer-task' AND status IN ('dispatching','active','uncertain')`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("live runs=%d, want exactly one", remaining)
	}
}
