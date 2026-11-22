package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/ronigooja/Zoe/contracts"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/runner"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestProductionControlClientReadsAcceptedAnswerWithoutConsumingOnRead(t *testing.T) {
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
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO actors(id,subject_id,name,status,created_at) VALUES('device','owner','test','approved',?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) VALUES('answer-task','default','question','goal','user','profile','awaiting_user',2,?,?,?,?,?)`, stamp, stamp, "device", "command", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('answer-run','default','answer-task','answer-attempt','active','run',1,8,'configured-runner','device','command','goal','profile','opencode','runtime','model','worker',?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO pending_runtime_actions(id,workspace_id,task_id,run_id,fence,request_id,kind,question,message,action_digest,decision,expires_at,created_at) VALUES('answer-action','default','answer-task','answer-run',8,'frm-real','worker_question','red or blue','blue','answer-digest','answer',?,?)`, now.Add(-time.Hour).Format(time.RFC3339Nano), stamp); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE tasks SET interaction_run_id='answer-run',interaction_action_digest='answer-digest' WHERE id='answer-task'`,
		`INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES('answer-entry','answer-task','default','user_message','blue','device','2026-11-16T00:00:00Z')`,
		`INSERT INTO task_question_answers(entry_id,workspace_id,task_id,question_id) VALUES('answer-entry','default','answer-task','answer-action')`,
	} {
		if _, err := store.DB().ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	server, err := NewServer(store, Config{RunnerToken: token, RunnerID: "configured-runner", RunnerScopes: []string{"runner:control:read", "runner:observe"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterRunnerCredential(ctx, token, "configured-runner", []string{"runner:control:read", "runner:observe"}); err != nil {
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
	if used != 0 {
		t.Fatalf("authorization_used=%d, want repeatable pending answer read", used)
	}
	// A lost authorization response must leave the accepted answer readable.
	again, err := client.RuntimeAuthorization(ctx, "answer-run", "frm-real", 8)
	if err != nil || again == nil || again.EntryID != "answer-entry" || again.QuestionID != "answer-action" {
		t.Fatalf("repeat=%+v err=%v", again, err)
	}
	var remaining int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE task_id='answer-task' AND status IN ('dispatching','active','uncertain')`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("live runs=%d, want exactly one", remaining)
	}
	// Arbitrarily large accepted question text never enters the ACK receipt.
	question := strings.Repeat("\u2028", 100000)
	if _, err := store.DB().Exec(`UPDATE pending_runtime_actions SET question=? WHERE id='answer-action'`, question); err != nil {
		t.Fatal(err)
	}
	ack := contracts.EntryDelivery{AttemptID: "answer-attempt", Fence: 8, EntryID: "answer-entry", QuestionID: "answer-action", ActionDigest: "answer-digest"}
	encoded, _ := json.Marshal(ack)
	if len(encoded) > 512 || bytes.Contains(encoded, []byte(question)) {
		t.Fatalf("ack bytes=%d", len(encoded))
	}
	for _, wrong := range []contracts.EntryDelivery{
		{AttemptID: ack.AttemptID, Fence: ack.Fence, EntryID: ack.EntryID, QuestionID: "wrong-question", ActionDigest: ack.ActionDigest},
		{AttemptID: ack.AttemptID, Fence: ack.Fence, EntryID: "wrong-entry", QuestionID: ack.QuestionID, ActionDigest: ack.ActionDigest},
		{AttemptID: ack.AttemptID, Fence: ack.Fence, EntryID: ack.EntryID, QuestionID: ack.QuestionID, ActionDigest: "wrong-digest"},
	} {
		if err := client.AcknowledgeEntry(ctx, "answer-run", wrong); err == nil {
			t.Fatalf("wrong receipt accepted: %+v", wrong)
		}
	}
	// Replaying an ACK after losing its response must not leave a pending action.
	for i := 0; i < 2; i++ {
		if err := client.AcknowledgeEntry(ctx, "answer-run", ack); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM pending_runtime_actions WHERE run_id='answer-run' AND authorization_used=0`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("unused actions=%d err=%v", remaining, err)
	}
	if err := store.DB().QueryRow(`SELECT authorization_used FROM pending_runtime_actions WHERE id='answer-action'`).Scan(&used); err != nil || used != 1 {
		t.Fatalf("used=%d err=%v", used, err)
	}
	if err := client.Observe(ctx, "answer-run", contracts.RunnerObservation{ObservationID: "answer-uncertain", RunID: "answer-run", AttemptID: "answer-attempt", Fence: 8, Kind: "effect_unknown", Result: json.RawMessage(`{"reason":"answer_delivery_uncertain","entry_id":"answer-entry"}`)}); err != nil {
		t.Fatal(err)
	}
	var runStatus, taskStatus, message string
	if err := store.DB().QueryRow(`SELECT r.status,t.status,t.error FROM runs r JOIN tasks t ON t.id=r.task_id WHERE r.id='answer-run'`).Scan(&runStatus, &taskStatus, &message); err != nil {
		t.Fatal(err)
	}
	if runStatus != "uncertain" || taskStatus != "paused" || !strings.Contains(message, "uncertain") {
		t.Fatalf("run=%s task=%s message=%s", runStatus, taskStatus, message)
	}
	// A separate trusted terminal observation still finalizes after the exact ACK.
	if _, err := store.DB().Exec(`UPDATE runs SET runtime_session_id='answer-session' WHERE id='answer-run'`); err != nil {
		t.Fatal(err)
	}
	if err := client.Observe(ctx, "answer-run", contracts.RunnerObservation{ObservationID: "answer-result", RunID: "answer-run", AttemptID: "answer-attempt", Fence: 8, Kind: "result", RuntimeSessionID: "answer-session", Result: json.RawMessage(`{"assistant_output":"completed after confirmed answer"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := client.Observe(ctx, "answer-run", contracts.RunnerObservation{ObservationID: "answer-terminal", RunID: "answer-run", AttemptID: "answer-attempt", Fence: 8, Kind: "runtime_terminated", RuntimeSessionID: "answer-session", TerminationProof: &contracts.TerminationProof{Source: "runner_host_session", Method: "session_interrupted", RuntimeSessionID: "answer-session", CleanupComplete: true, VerifiedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id='answer-task'`).Scan(&taskStatus); err != nil || taskStatus != "complete" {
		t.Fatalf("final status=%s err=%v", taskStatus, err)
	}
}
