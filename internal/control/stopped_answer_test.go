package control

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
	"github.com/ronigooja/Zoe/internal/runner"
)

type stoppedAnswerRuntime struct {
	prompt string
	cancel context.CancelFunc
}

func (r *stoppedAnswerRuntime) Create(context.Context, runner.RuntimeSpec) (runner.RuntimeBinding, error) {
	return runner.RuntimeBinding{SessionID: "new-session"}, nil
}
func (r *stoppedAnswerRuntime) Prompt(_ context.Context, _ runner.RuntimeBinding, text string) error {
	r.prompt = text
	return nil
}
func (r *stoppedAnswerRuntime) Status(context.Context, runner.RuntimeBinding) (map[string]opencode.Status, error) {
	return nil, nil
}
func (r *stoppedAnswerRuntime) Messages(context.Context, runner.RuntimeBinding) ([]opencode.Message, error) {
	return nil, nil
}
func (r *stoppedAnswerRuntime) Events(context.Context, runner.RuntimeBinding) (<-chan opencode.Event, <-chan error) {
	r.cancel()
	return make(chan opencode.Event), make(chan error)
}
func (r *stoppedAnswerRuntime) Abort(context.Context, runner.RuntimeBinding) error { return nil }
func (r *stoppedAnswerRuntime) Terminate(context.Context, runner.RuntimeBinding) (string, error) {
	return "stopped", nil
}

func TestStoppedAnswerNewClaimPromptReceipt(t *testing.T) {
	store, _, _ := instanceAuthFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	server, err := NewServer(store, Config{Profiles: map[string]RuntimeProfile{"worker": {ID: "worker", Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: runner.OpenCodeWorkspaceWritePolicy, PermissionTier: "read_only_research"}}, DefaultProfileID: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE actors SET subject_id='owner' WHERE id='instance'`,
		`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id,interaction_state,interaction_run_id,interaction_action_digest,interaction_question) VALUES('task','default','title','original goal','user','worker','waiting',1,'` + stamp + `','` + stamp + `','instance','cmd','owner','awaiting_message','old-run','digest','Which year?')`,
		`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('old-run','default','task','old-attempt','stopped','run',1,1,'runner','instance','cmd','original goal','worker','opencode','runtime','model','worker','` + stamp + `')`,
		`INSERT INTO pending_runtime_actions(id,workspace_id,task_id,run_id,fence,request_id,kind,question,action_digest,expires_at,created_at) VALUES('question','default','task','old-run',1,'old-form','worker_question','Which year?','digest','` + now.Add(time.Hour).Format(time.RFC3339Nano) + `','` + stamp + `')`,
		`UPDATE tasks SET harness='opencode' WHERE id='task'`,
	} {
		if _, err := store.DB().Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := "question"
	if _, _, err := server.AppendTaskInputTx(ctx, tx, CommandContext{Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "owner", DeviceID: "instance"}, AcceptedAt: now}, "task", TaskInput{Content: "Use 2026 and preserve the original scope.", QuestionID: &q}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	token := "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA"
	if err := server.RegisterRunnerCredential(ctx, token, "runner", []string{"default"}, []string{"runner:claim", "runner:observe", "runner:control:read"}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	client, err := runner.NewControlClient(ts.URL, token, ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := client.Claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	if len(claim.Entries) != 1 || claim.Entries[0].Question.RunID != "old-run" {
		t.Fatalf("original binding lost: %+v", claim.Entries)
	}
	journal, err := runner.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runtime := &stoppedAnswerRuntime{cancel: cancel}
	engine := runner.Engine{Control: client, Runtime: runtime, Journal: journal, Routes: map[string]runner.Route{"worker": {Harness: "opencode", IsolationPolicy: runner.OpenCodeWorkspaceWritePolicy, RuntimeRouteID: "runtime", ModelRouteID: "model", Model: "fixture"}}}
	if err := engine.Process(processCtx, *claim); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !strings.Contains(runtime.prompt, "Which year?") || !strings.Contains(runtime.prompt, "Use 2026") || !strings.Contains(runtime.prompt, "old-run") {
		t.Fatalf("missing structured provenance: %s", runtime.prompt)
	}
	var state string
	var used int
	if err := store.DB().QueryRow(`SELECT delivery_state FROM task_question_answers`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT authorization_used FROM pending_runtime_actions WHERE id='question'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if state != "acknowledged" || used != 1 {
		t.Fatalf("receipt=%s used=%d", state, used)
	}
}
