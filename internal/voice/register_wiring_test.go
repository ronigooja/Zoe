package voice

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/steward"
	"github.com/ronigooja/Zoe/internal/storage"
)

// registerDisabledHTTPServer applies to fixtures requiring no implicit register.
func registerDisabledHTTPServer(t *testing.T, cfg Config) (*httptest.Server, *storage.Store, string) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	server, err := control.NewServer(store, control.Config{Profiles: map[string]control.RuntimeProfile{"zoe-steward": {ID: "zoe-steward", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "steward", IsolationPolicy: "opencode-workspace-write-v1", PermissionTier: "read_only_research"}}, DefaultProfileID: "zoe-steward"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(server, store, cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := control.InitializeInstanceAuth(ctx, store, key); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(func() { ts.Close(); server.Shutdown(); _ = store.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('handoff-session','default','owner','instance','request',X'01',1,'active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	return ts, store, key
}

func TestHandoffSnapshotRefreshRetainsRegister(t *testing.T) {
	for _, action := range []steward.Action{steward.ActionNew, steward.ActionClarify} {
		t.Run(string(action), func(t *testing.T) {
			var routes, choices atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				routes.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]string{"output_text": `{"routes":["task"],"request":"A complete accepted request."}`})
			}))
			defer model.Close()
			modelCfg := RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}
			cfg := Config{RegisterConfig: &RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: modelCfg, CompactModel: modelCfg, CompactCharacters: 1000}}
			var mutate func()
			cfg.Steward = classificationStewardFunc(func(_ context.Context, input steward.Input) (steward.Choice, error) {
				choices.Add(1)
				if input.Request != "A complete accepted request." {
					t.Errorf("request changed: %q", input.Request)
				}
				if mutate != nil {
					change := mutate
					mutate = nil
					change()
				}
				if action == steward.ActionClarify {
					reply := "Which task?"
					return steward.Choice{Action: action, Reply: &reply}, nil
				}
				return steward.Choice{Action: action}, nil
			})
			ts, store, key := registerDisabledHTTPServer(t, cfg)
			// Commit alone, including a transaction that rolls back, must call no model.
			hash := sha256.Sum256([]byte("original"))
			status, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "snapshot-commit", "voice.turn.commit", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "snapshot", "revision": 1, "content": "original", "user_confirmed": true})
			if status != 200 {
				t.Fatalf("commit: %d %s", status, raw)
			}
			if routes.Load() != 0 || choices.Load() != 0 {
				t.Fatal("commit launched classification")
			}
			var id string
			if err := store.DB().QueryRow(`SELECT id FROM voice_turns WHERE turn_id='snapshot'`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			svc := newService(store.DB(), cfg)
			register, err := NewRegister(store.DB(), *cfg.RegisterConfig)
			if err != nil {
				t.Fatal(err)
			}
			svc.register = register
			tx, err := store.DB().BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = svc.commandCommitTurn(context.Background(), tx, control.CommandContext{Principal: contracts.Principal{WorkspaceID: "default", SubjectID: "owner", DeviceID: "instance", Scopes: []string{"client:command"}}, CommandID: "rolled-back", AcceptedAt: time.Now().UTC()}, contracts.CommandRequest{Payload: resolvedRequestJSON(map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "rollback", "revision": 1, "content": "rolled back original", "user_confirmed": true})})
			_ = tx.Rollback()
			if err != nil {
				t.Fatal(err)
			}
			if routes.Load() != 0 || choices.Load() != 0 {
				t.Fatal("rolled back commit launched model")
			}
			mutate = func() {
				// Complete an actual concurrent DB write while Steward is outside authority.
				done := make(chan error, 1)
				go func() {
					_, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,status,version,created_by_device_id,command_id,profile_id,created_at,updated_at,owner_subject_id) VALUES('concurrent','default','duplicate','Other task','voice','queued',1,'instance','cmd','zoe-steward',?,?, 'owner')`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
					done <- err
				}()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}
			svc.classifyCommittedTurn(id)
			status, _, raw = handoffCommand(t, ts.Client(), ts.URL, key, "snapshot-handoff", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "snapshot", "revision": 1, "content_hash": fmt.Sprintf("%x", hash)})
			if status != 200 {
				t.Fatalf("refresh: %d %s", status, raw)
			}
			if routes.Load() != 1 || choices.Load() != 2 {
				t.Fatalf("calls register=%d steward=%d", routes.Load(), choices.Load())
			}
		})
	}
}

func TestResolvedRequestHandoffFoundation(t *testing.T) {
	const full = "Delete the lunar mission draft from the research workspace. Preserve all published reports."
	for _, action := range []string{"new", "append", "append_pending", "answer", "answer_stale", "continue", "clarify", "report", "cancel", "redisplay", "foreign", "chat", "none", "missing", "stale"} {
		t.Run(action, func(t *testing.T) {
			var selected string
			var changeFacts func()
			var received steward.Input
			questionID := "pending-question"
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				output := `{"routes":["task"],"request":` + string(resolvedRequestJSON(full)) + `}`
				if action == "chat" {
					output = `{"routes":["chat"],"request":null}`
				}
				if action == "none" {
					output = `{"routes":["task"],"request":"none"}`
				}
				if action == "missing" {
					output = `{"routes":["task"]}`
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"output_text": output})
			}))
			defer model.Close()
			cfg := Config{RegisterConfig: &RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}, CompactModel: RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}, CompactCharacters: 1000}}
			cfg.Steward = classificationStewardFunc(func(_ context.Context, input steward.Input) (steward.Choice, error) {
				received = input
				if selected == "" {
					return steward.Choice{Action: steward.ActionNew}, nil
				}
				switch action {
				case "foreign":
					id := "foreign-task"
					return steward.Choice{Action: steward.ActionAppend, TaskID: &id}, nil
				case "clarify":
					reply := "Which draft?"
					return steward.Choice{Action: steward.ActionClarify, Reply: &reply}, nil
				case "report":
					reply := "The draft task is queued."
					return steward.Choice{Action: steward.ActionReport, TaskID: &selected, Reply: &reply}, nil
				case "stale":
					if changeFacts != nil {
						changeFacts()
						changeFacts = nil
					}
					return steward.Choice{Action: steward.ActionAppend, TaskID: &selected}, nil
				case "append_pending":
					return steward.Choice{Action: steward.ActionAppend, TaskID: &selected}, nil
				case "answer":
					return steward.Choice{Action: steward.ActionAnswer, TaskID: &selected, QuestionID: &questionID}, nil
				case "answer_stale":
					return steward.Choice{}, steward.ErrNoQuestion
				default:
					return steward.Choice{Action: steward.Action(action), TaskID: &selected}, nil
				}
			})
			ts, store, key := registerDisabledHTTPServer(t, cfg)
			if action == "stale" {
				changeFacts = func() {
					if _, err := store.DB().Exec(`UPDATE tasks SET version=version+1 WHERE id=?`, selected); err != nil {
						t.Error(err)
					}
				}
			}
			if action != "new" && action != "chat" && action != "none" && action != "missing" {
				selected = commitAndHandoff(t, ts, key, "seed", "original seed").ResourceID
				if action == "continue" || action == "redisplay" {
					if _, err := store.DB().Exec(`UPDATE tasks SET status='complete',result='Authoritative result' WHERE id=?`, selected); err != nil {
						t.Fatal(err)
					}
				}
				if action == "append_pending" || action == "answer" {
					seedHandoffQuestion(t, store, selected, questionID)
				}
			}
			before := 0
			if selected != "" {
				before = 1
			}
			result := commitAndHandoff(t, ts, key, "current", "delete it")
			var count int
			if err := store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "new", "continue":
				if count != before+1 {
					t.Fatalf("task count=%d", count)
				}
				var goal string
				if err := store.DB().QueryRow(`SELECT goal FROM tasks WHERE id=?`, result.ResourceID).Scan(&goal); err != nil || goal != full {
					t.Fatalf("goal=%q err=%v", goal, err)
				}
				if action == "continue" {
					var priorID, priorGoal, status, resultText string
					if err := store.DB().QueryRow(`SELECT prior_task_id,goal,status,result FROM task_prior_contexts WHERE task_id=?`, result.ResourceID).Scan(&priorID, &priorGoal, &status, &resultText); err != nil || priorID != selected || priorGoal != full || resultText != "Authoritative result" {
						t.Fatalf("prior=%s %q %s %q err=%v", priorID, priorGoal, status, resultText, err)
					}
				}
			case "append", "stale", "append_pending", "answer":
				if count != before || result.ResourceID != selected {
					t.Fatalf("append created replacement: %+v count=%d", result, count)
				}
				var entries int
				if err := store.DB().QueryRow(`SELECT count(*) FROM task_entries WHERE task_id=? AND content=?`, selected, full).Scan(&entries); err != nil || entries != 2 {
					t.Fatalf("full request entries=%d err=%v", entries, err)
				}
				var decision string
				if action == "append_pending" || action == "answer" {
					if err := store.DB().QueryRow(`SELECT decision FROM pending_runtime_actions WHERE id=?`, questionID).Scan(&decision); err != nil {
						t.Fatal(err)
					}
					if (action == "answer" && decision != "answer") || (action == "append_pending" && decision != "") {
						t.Fatalf("pending decision=%q", decision)
					}
				}
			default:
				if count != before {
					t.Fatalf("rejected/nonexecuting action admitted work: count=%d", count)
				}
			}
			if action != "chat" && action != "none" && action != "missing" && received.Request != full {
				t.Fatalf("Steward request=%q", received.Request)
			}
			for _, candidate := range received.Tasks {
				if candidate.TaskID == "" || candidate.Goal != full {
					t.Fatalf("candidate=%+v", candidate)
				}
			}
			if action == "cancel" || action == "report" || action == "clarify" || action == "redisplay" {
				hash := sha256.Sum256([]byte("delete it"))
				status, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "repeat-nonexecution", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "current", "revision": 1, "content_hash": fmt.Sprintf("%x", hash)})
				if status != 200 {
					t.Fatalf("action replay: %d %s", status, raw)
				}
				var n int
				if err := store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&n); err != nil || n != count {
					t.Fatalf("action replay admitted work count=%d err=%v", n, err)
				}
			}
			// Same revision and a later corrected revision share stable entry linkage.
			if action == "append" || action == "answer" || action == "append_pending" {
				hash := sha256.Sum256([]byte("delete it"))
				status, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "repeat-current", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "current", "revision": 1, "content_hash": fmt.Sprintf("%x", hash)})
				if status != 200 {
					t.Fatalf("repeat: %d %s", status, raw)
				}
				status, _, raw = handoffCommand(t, ts.Client(), ts.URL, key, "revision-current", "voice.turn.commit", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "current", "revision": 2, "content": "corrected provenance", "user_confirmed": true})
				if status != 200 {
					t.Fatalf("revision: %d %s", status, raw)
				}
				hash = sha256.Sum256([]byte("corrected provenance"))
				status, _, raw = handoffCommand(t, ts.Client(), ts.URL, key, "handoff-revision-current", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "current", "revision": 2, "content_hash": fmt.Sprintf("%x", hash)})
				if status != 200 {
					t.Fatalf("revision handoff: %d %s", status, raw)
				}
				var n int
				if err := store.DB().QueryRow(`SELECT count(*) FROM voice_task_inputs WHERE turn_id='current'`).Scan(&n); err != nil || n != 1 {
					t.Fatalf("stable input receipts=%d err=%v", n, err)
				}
				if err := store.DB().QueryRow(`SELECT count(*) FROM task_entries WHERE task_id=?`, selected).Scan(&n); err != nil || n != 2 {
					t.Fatalf("replay entries=%d err=%v", n, err)
				}
			}
		})
	}
}

func seedHandoffQuestion(t *testing.T, store *storage.Store, taskID, questionID string) {
	t.Helper()
	now := time.Now().UTC()
	at := now.Format(time.RFC3339Nano)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO runs(id,workspace_id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('question-run','default',?,'question-attempt','active','run',1,1,'runner','instance','cmd','goal','profile','opencode','runtime','model','worker',?)`, []any{taskID, at}},
		{`INSERT INTO pending_runtime_actions(id,workspace_id,task_id,run_id,fence,request_id,kind,question,action_digest,expires_at,created_at) VALUES(?,'default',?,'question-run',1,'request','worker_question','Which year?','digest',?,?)`, []any{questionID, taskID, now.Add(time.Hour).Format(time.RFC3339Nano), at}},
		{`UPDATE tasks SET status='waiting',interaction_state='awaiting_message',interaction_run_id='question-run',interaction_action_digest='digest',interaction_question='Which year?',version=version+1 WHERE id=?`, []any{taskID}},
	}
	for _, statement := range statements {
		if _, err := store.DB().Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func resolvedRequestJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func TestRegisterApprovedRequestSchema(t *testing.T) {
	for _, output := range []string{`{"routes":["chat"],"request":null}`, `{"routes":["task","ui"],"request":"First action. Then retain [all] details.\nDo not truncate."}`, `{"routes":["memory_write"],"request":"Remember the airport is LHR."}`} {
		if _, err := parseRegisterRoute(output); err != nil {
			t.Fatalf("valid %s: %v", output, err)
		}
	}
	for _, output := range []string{`{"routes":["chat"],"request":""}`, `{"routes":["task"],"request":null}`, `{"routes":["task"],"request":"none"}`, `{"routes":["ui"],"request":" NONE "}`, `{"routes":["task"],"request":"  "}`, `{"routes":["task"],"restatement":"old"}`, `{"routes":["task"],"request":"a","request":"b"}`, `{"routes":["chat","ui"],"request":null}`} {
		if _, err := parseRegisterRoute(output); err == nil {
			t.Fatalf("accepted %s", output)
		}
	}
	if strings.Contains(testPrompts(t).Register(), "one plain sentence") {
		t.Fatal("truncating old instruction")
	}
}
