package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	if _, err := store.DB().Exec(`INSERT INTO voice_sessions(id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('handoff-session','owner','instance','request',X'01',1,'active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	return ts, store, key
}

func TestResolvedRequestHandoffFoundation(t *testing.T) {
	const full = "Delete the lunar mission draft from the research directory. Preserve all published reports."
	for _, action := range []string{"new", "append", "append_pending", "answer", "answer_stale", "continue", "clarify", "report", "cancel", "redisplay", "foreign", "chat", "none", "missing", "stale"} {
		t.Run(action, func(t *testing.T) {
			var selected string
			var changeFacts func()
			var received steward.Input
			questionID := "pending-question"
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				output := `{"route":"task","request":` + string(resolvedRequestJSON(full)) + `}`
				if action == "chat" {
					output = `{"route":"chat","request":null}`
				}
				if action == "none" {
					output = `{"route":"task","request":"none"}`
				}
				if action == "missing" {
					output = `{"route":"task"}`
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
			cfg.IngestEnabled = true
			ts, store, _ := registerDisabledHTTPServer(t, cfg)
			if action == "stale" {
				changeFacts = func() {
					if _, err := store.DB().Exec(`UPDATE tasks SET version=version+1 WHERE id=?`, selected); err != nil {
						t.Error(err)
					}
				}
			}
			if action != "new" && action != "chat" && action != "none" && action != "missing" {
				selected = delegateTurn(t, ts, store, "seed", "original seed").ResourceID
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
			result := delegateTurn(t, ts, store, "current", "delete it")
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
			switch action {
			case "cancel":
				var status string
				if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, selected).Scan(&status); err != nil || status != "canceled" {
					t.Fatalf("cancel status=%q err=%v", status, err)
				}
			case "report", "clarify", "none", "missing", "foreign", "answer_stale":
				want := stewardErrorSpeech
				if action == "report" {
					want = "The draft task is queued."
				} else if action == "clarify" {
					want = "Which draft?"
				}
				var reply string
				if err := store.DB().QueryRow(`SELECT text FROM voice_fast_replies WHERE turn_id='current'`).Scan(&reply); err != nil || reply != want {
					t.Fatalf("held reply=%q want=%q err=%v", reply, want, err)
				}
			case "chat":
				if n := writeJobCount(t, store, `SELECT count(*) FROM voice_fast_replies`); n != 0 || received.Request != "" {
					t.Fatalf("chat triggered a reply or Steward: replies=%d request=%q", n, received.Request)
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
			// A replay of the same transcript and delegation identity cannot add work.
			if replay := delegateTurn(t, ts, store, "current", "delete it"); replay != result {
				t.Fatalf("result changed on replay: first=%+v second=%+v", result, replay)
			}
			if action == "append" || action == "answer" || action == "append_pending" {
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
		{`INSERT INTO runs(id,task_id,attempt_id,status,desired_control,control_version,fence,runner_id,authorized_device_id,command_id,goal,profile_id,harness,runtime_route_id,model_route_id,role,started_at) VALUES('question-run',?,'question-attempt','active','run',1,1,'runner','instance','cmd','goal','profile','opencode','runtime','model','worker',?)`, []any{taskID, at}},
		{`INSERT INTO pending_runtime_actions(id,task_id,run_id,fence,request_id,kind,question,action_digest,expires_at,created_at) VALUES(?,?,'question-run',1,'request','worker_question','Which year?','digest',?,?)`, []any{questionID, taskID, now.Add(time.Hour).Format(time.RFC3339Nano), at}},
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
	for _, output := range []string{`{"route":"chat","request":null}`, `{"route":"task_ui","request":"First action. Then retain [all] details.\nDo not truncate."}`, `{"route":"memory_write","request":"Remember the airport is LHR."}`} {
		if _, err := parseRegisterRoute(output); err != nil {
			t.Fatalf("valid %s: %v", output, err)
		}
	}
	for _, output := range []string{`{"route":"chat","request":""}`, `{"route":"task","request":null}`, `{"route":"task","request":"none"}`, `{"route":"ui","request":" NONE "}`, `{"route":"task","request":"  "}`, `{"route":"task","restatement":"old"}`, `{"route":"task","request":"a","request":"b"}`, `{"routes":["chat","ui"],"request":null}`} {
		if _, err := parseRegisterRoute(output); err == nil {
			t.Fatalf("accepted %s", output)
		}
	}
	if strings.Contains(testPrompts(t).Register(), "one plain sentence") {
		t.Fatal("truncating old instruction")
	}
}
