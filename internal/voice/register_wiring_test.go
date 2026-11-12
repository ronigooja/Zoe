package voice

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/memory"
	"github.com/ronigooja/Zoe/internal/steward"
)

func TestRegisterHandoffRoutesAndActions(t *testing.T) {
	for _, name := range []string{"chat", "memory_write", "recall", "new", "none", "answer", "answer_stale", "cancel", "cancel_live", "cancel_complete", "cancel_failed", "cancel_canceled", "report", "report_empty", "attach_active", "attach_complete", "foreign", "attach_failed", "attach_canceled", "route_error", "choose_error", "no_match", "confirm", "decline"} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			registerCalls, stewardCalls := 0, 0
			route, action, selected := "task", "new", ""
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				instruction, _ := body["instructions"].(string)
				if messages, ok := body["messages"].([]any); ok && len(messages) > 0 {
					first, _ := messages[0].(map[string]any)
					instruction, _ = first["content"].(string)
				}
				register := strings.Contains(instruction, "Route memory_write")
				if register {
					registerCalls++
				} else {
					stewardCalls++
				}
				if register && name == "route_error" || !register && name == "choose_error" {
					w.WriteHeader(503)
					return
				}
				output := fmt.Sprintf(`{"action":%q,"task":%q,"reply":""}`, action, selected)
				if action == "report" {
					output = fmt.Sprintf(`{"action":"report","task":%q,"reply":"The task is queued."}`, selected)
				}
				if register {
					restatement := "none"
					if route != "task" {
						restatement = ""
					}
					output = fmt.Sprintf(`{"route":%q,"restatement":%q}`, route, restatement)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"output_text": output, "choices": []any{map[string]any{"message": map[string]string{"content": output}}}})
			}))
			defer model.Close()
			client, err := steward.FromEnv(func(k string) string {
				switch k {
				case "ZOE_STEWARD_BASE_URL":
					return model.URL
				case "ZOE_STEWARD_MODEL":
					return "fixture"
				case "ZOE_STEWARD_API_KEY_FILE":
					return "fixture"
				}
				return ""
			}, func(string) ([]byte, error) { return []byte("fixture"), nil })
			if err != nil {
				t.Fatal(err)
			}
			cfg := Config{Steward: client, RegisterConfig: &RegisterConfig{Model: RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}, CompactModel: RegisterModelConfig{BaseURL: model.URL, Model: "compact-fixture", APIKey: "fixture"}, CompactCharacters: 1000}}
			ts, store, key := handoffHTTPServerWithConfig(t, cfg)
			if err := memory.RegisterMigrations(store); err != nil {
				t.Fatal(err)
			}
			if err := store.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			count := func(query string, args ...any) int {
				t.Helper()
				var n int
				if err := store.DB().QueryRow(query, args...).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			var target string
			if strings.HasPrefix(name, "answer") || strings.HasPrefix(name, "cancel") || name == "report" || strings.HasPrefix(name, "attach_") || name == "foreign" {
				target = commitAndHandoff(t, ts, key, "seed", "research the lunar mission").ResourceID
				mu.Lock()
				selected = "research the lunar mission"
				mu.Unlock()
				switch name {
				case "answer":
					_, err = store.DB().Exec(`UPDATE tasks SET status='waiting',interaction_state='awaiting_message',interaction_question='Which year?' WHERE id=?`, target)
				case "attach_active":
					_, err = store.DB().Exec(`UPDATE tasks SET status='active' WHERE id=?`, target)
				case "cancel_live":
					req, _ := http.NewRequest(http.MethodPost, ts.URL+"/runner/v1/claims", nil)
					req.Header.Set("Authorization", "Bearer BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA")
					resp, claimErr := ts.Client().Do(req)
					if claimErr != nil {
						t.Fatal(claimErr)
					}
					resp.Body.Close()
					if resp.StatusCode != 200 {
						t.Fatalf("claim status=%d", resp.StatusCode)
					}
				case "attach_complete":
					_, err = store.DB().Exec(`UPDATE tasks SET status='complete',result=? WHERE id=?`, strings.Repeat("月", 700), target)
				case "cancel_complete", "cancel_failed", "cancel_canceled":
					_, err = store.DB().Exec(`UPDATE tasks SET status=? WHERE id=?`, strings.TrimPrefix(name, "cancel_"), target)
				case "attach_failed", "attach_canceled":
					_, err = store.DB().Exec(`UPDATE tasks SET status=? WHERE id=?`, strings.TrimPrefix(name, "attach_"), target)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			mu.Lock()
			switch name {
			case "chat", "memory_write", "recall":
				route = name
			case "answer", "cancel", "report", "none":
				action = name
			case "answer_stale":
				action = "answer"
			case "cancel_live", "cancel_complete", "cancel_failed", "cancel_canceled":
				action = "cancel"
			case "report_empty":
				action = "report"
			case "no_match":
				action = "attach"
				selected = "not a candidate"
			case "confirm", "decline":
				route = name
			case "attach_active", "attach_complete", "foreign", "attach_failed", "attach_canceled":
				action = "attach"
			}
			mu.Unlock()
			// A foreign selection cannot come from real candidates: a stale decision
			// is injected only after classification to exercise transactional ownership.
			if name == "foreign" || name == "answer_stale" || name == "attach_failed" || name == "attach_canceled" {
				mu.Lock()
				action = "new"
				selected = ""
				mu.Unlock()
			}
			text := "delete the lunar mission draft"
			if name == "chat" {
				text = "thanks for your help"
				// Candidate retrieval always selects finished_at. Renaming it makes
				// an accidental query fail instead of silently returning no candidates.
				if _, err := store.DB().Exec(`ALTER TABLE tasks RENAME COLUMN finished_at TO chat_candidate_query_tripwire`); err != nil {
					t.Fatal(err)
				}
			}
			if name == "memory_write" {
				text = "remember my preferred airport is LHR"
			}
			var r contracts.CommandReceipt
			if name == "foreign" || name == "answer_stale" || name == "attach_failed" || name == "attach_canceled" {
				code, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "commit-current", "voice.turn.commit", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "current", "revision": 1, "content": text, "user_confirmed": true})
				if code != 200 {
					t.Fatal(raw)
				}
				deadline := time.Now().Add(time.Second)
				for count(`SELECT count(*) FROM voice_turns WHERE turn_id='current' AND triage_category IS NOT NULL`) == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				storedAction := "attach"
				if name == "answer_stale" {
					storedAction = "answer"
				}
				if _, err := store.DB().Exec(`UPDATE voice_turns SET steward_action=?,steward_task_id=? WHERE turn_id='current'`, storedAction, target); err != nil {
					t.Fatal(err)
				}
				if name == "foreign" {
					if _, err := store.DB().Exec(`UPDATE tasks SET owner_subject_id='someone-else' WHERE id=?`, target); err != nil {
						t.Fatal(err)
					}
				}
				hash := sha256.Sum256([]byte(text))
				code, r, raw = handoffCommand(t, ts.Client(), ts.URL, key, "handoff-current", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "current", "revision": 1, "content_hash": fmt.Sprintf("%x", hash)})
				if code != 200 {
					t.Fatal(raw)
				}
			} else {
				r = commitAndHandoff(t, ts, key, "current", text)
			}
			switch name {
			case "chat":
				if r.ResourceType != "voice_turn" || r.ResourceID != "current" || r.ResourceVersion != 1 {
					t.Fatalf("chat result=%+v", r)
				}
				if count(`SELECT count(*) FROM voice_turns WHERE turn_id='current' AND triage_category='chat' AND steward_decision='' AND steward_action='' AND handoff_handled=1`) != 1 {
					t.Fatal("chat was not stored and silently handled; candidate retrieval may have run")
				}
				if count(`SELECT count(*) FROM tasks`) != 0 || count(`SELECT count(*) FROM voice_fast_replies`) != 0 {
					t.Fatal("chat created a task or fast reply")
				}
			case "memory_write":
				if count(`SELECT count(*) FROM memory_staged_writes WHERE original_text=?`, text) != 1 || count(`SELECT count(*) FROM tasks`) != 0 || count(`SELECT count(*) FROM voice_fast_replies`) != 0 {
					t.Fatal("memory staging created task/reply or missed original")
				}
			case "recall":
				if count(`SELECT count(*) FROM voice_recall_jobs WHERE question=?`, text) != 1 || count(`SELECT count(*) FROM tasks`) != 0 {
					t.Fatal("recall not queued")
				}
			case "none":
				if count(`SELECT count(*) FROM tasks`) != 0 || count(`SELECT count(*) FROM voice_fast_replies`) != 0 {
					t.Fatal("none had side effects")
				}
			case "route_error", "choose_error", "no_match", "confirm", "decline":
				if count(`SELECT count(*) FROM tasks`) != 0 || count(`SELECT count(*) FROM voice_fast_replies WHERE text=?`, stewardErrorSpeech) != 1 {
					t.Fatal("model error did not fail closed")
				}
			case "answer", "attach_active":
				if r.ResourceID != target || count(`SELECT count(*) FROM tasks`) != 1 || count(`SELECT count(*) FROM task_entries WHERE task_id=? AND content=?`, target, text) != 1 {
					t.Fatal("addition did not append exactly once")
				}
				if name == "attach_active" && count(`SELECT count(*) FROM tasks WHERE id=? AND status='active'`, target) != 1 {
					t.Fatal("attach stopped active task")
				}
			case "cancel":
				if count(`SELECT count(*) FROM tasks WHERE id=? AND status='canceled'`, target) != 1 || count(`SELECT count(*) FROM voice_fast_replies WHERE text='任务已经停止。'`) != 1 {
					t.Fatal("cancel did not use truthful stop path")
				}
			case "cancel_complete", "cancel_failed", "cancel_canceled":
				if count(`SELECT count(*) FROM tasks WHERE id=? AND status=?`, target, strings.TrimPrefix(name, "cancel_")) != 1 || count(`SELECT count(*) FROM voice_fast_replies WHERE turn_id='current'`) != 1 {
					t.Fatal("terminal cancel changed true state")
				}
			case "cancel_live":
				if count(`SELECT count(*) FROM tasks WHERE id=? AND status='active'`, target) != 1 || count(`SELECT count(*) FROM runs WHERE task_id=? AND desired_control='stop'`, target) != 1 || count(`SELECT count(*) FROM voice_fast_replies WHERE text='已经请求停止，正在停下。'`) != 1 {
					t.Fatal("live cancel claimed completion before runner acknowledgement")
				}
			case "report", "report_empty":
				if count(`SELECT count(*) FROM voice_turns WHERE turn_id='current' AND report_text='The task is queued.' AND pending_question=''`) != 1 {
					t.Fatal("report reply was not isolated from the pending question")
				}
				want := 1
				if name == "report_empty" {
					want = 0
				}
				if count(`SELECT count(*) FROM tasks`) != want || count(`SELECT count(*) FROM voice_fast_replies WHERE intent_kind='task_report' AND text='The task is queued.'`) != 1 {
					t.Fatal("report did not hold model reply")
				}
			case "attach_complete":
				if count(`SELECT count(*) FROM tasks`) != 2 || count(`SELECT count(*) FROM tasks WHERE id=? AND goal=?`, r.ResourceID, text+"\n上一个任务的结果:\n"+strings.Repeat("月", 600)) != 1 {
					t.Fatal("completed attach lost bounded result")
				}
			default:
				if count(`SELECT count(*) FROM tasks WHERE id=? AND status='queued' AND interaction_state='none'`, r.ResourceID) != 1 {
					t.Fatal("new was held for confirmation")
				}
			}
			hash := sha256.Sum256([]byte(text))
			before := count(`SELECT count(*) FROM tasks`)
			status, _, raw := handoffCommand(t, ts.Client(), ts.URL, key, "repeat-current", "voice.turn.handoff", map[string]any{"session_id": "handoff-session", "generation": 1, "turn_id": "current", "revision": 1, "content_hash": fmt.Sprintf("%x", hash)})
			if status != 200 || count(`SELECT count(*) FROM tasks`) != before {
				t.Fatalf("repeat changed task count: %d %s", status, raw)
			}
			if name == "memory_write" && count(`SELECT count(*) FROM memory_staged_writes`) != 1 {
				t.Fatal("replay staged twice")
			}
			if name == "chat" {
				mu.Lock()
				calls, choices := registerCalls, stewardCalls
				mu.Unlock()
				if calls != 1 || choices != 0 || count(`SELECT count(*) FROM voice_fast_replies`) != 0 || count(`SELECT count(*) FROM voice_turns WHERE turn_id='current' AND handoff_handled=1`) != 1 {
					t.Fatalf("chat replay: register=%d steward=%d; expected one register call, no Steward or speech, handled", calls, choices)
				}
			}
			if (name == "cancel" || name == "cancel_live") && count(`SELECT count(*) FROM events WHERE event_type='task.stop_requested' AND aggregate_id=?`, target) != 1 {
				t.Fatal("replay stopped twice")
			}
			if name == "answer" || name == "attach_active" {
				if count(`SELECT count(*) FROM task_entries WHERE task_id=? AND content=?`, target, text) != 1 {
					t.Fatal("replay appended twice")
				}
			}
		})
	}
}
