package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/steward"
)

func TestUIControllerStoredWithoutConsumer(t *testing.T) {
	for _, client := range []*modelclient.Client{nil, {}} {
		svc := newService(nil, Config{UIController: client})
		defer svc.recallCancel()
		if svc.uiController != client {
			t.Fatal("UI controller was not retained")
		}
	}
}

func TestUIExitsAndTaskDisplay(t *testing.T) {
	for _, name := range []string{"ui", "task_ui", "task", "attach_sets", "attach_queued", "attach_waiting", "attach_awaiting_user", "attach_keeps", "complete_sets", "complete_clears"} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			routes := []string{"task"}
			choice := steward.Choice{Action: steward.ActionNew}
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				out, _ := json.Marshal(map[string]any{"route": strings.Join(routes, "_"), "request": "Research and show the lunar mission information."})
				_ = json.NewEncoder(w).Encode(map[string]string{"output_text": string(out)})
			}))
			defer model.Close()
			cfg := RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}
			ts, store, key := handoffHTTPServerWithConfig(t, Config{RegisterConfig: &RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: cfg, CompactModel: cfg, CompactCharacters: 1000}, Steward: classificationStewardFunc(func(context.Context, steward.Input) (steward.Choice, error) {
				mu.Lock()
				defer mu.Unlock()
				return choice, nil
			})})
			uiTargetSchema(t, store.DB())
			target := ""
			if strings.HasPrefix(name, "attach_") || strings.HasPrefix(name, "complete_") {
				target = commitAndHandoff(t, ts, key, "seed", "research the lunar mission").ResourceID
				flag, status := 0, "active"
				if name == "attach_keeps" || name == "complete_clears" {
					flag = 1
				}
				if name == "attach_queued" || name == "attach_waiting" || name == "attach_awaiting_user" {
					status = strings.TrimPrefix(name, "attach_")
				}
				if strings.HasPrefix(name, "complete_") {
					status = "complete"
				}
				if _, err := store.DB().Exec(`UPDATE tasks SET ui_display=?,status=?,result='previous result' WHERE id=?`, flag, status, target); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				choice = steward.Choice{Action: steward.ActionAppend, TaskID: &target}
				if strings.HasPrefix(name, "complete_") {
					choice.Action = steward.ActionContinue
				}
				mu.Unlock()
			}
			withUI := name == "task_ui" || name == "attach_sets" || name == "complete_sets" || name == "attach_queued" || name == "attach_waiting" || name == "attach_awaiting_user"
			wantFlag := withUI || name == "attach_keeps"
			mu.Lock()
			if name == "ui" {
				routes = []string{"ui"}
			} else if withUI {
				routes = []string{"task", "ui"}
			}
			mu.Unlock()
			r := commitAndHandoff(t, ts, key, "display", "show the lunar mission information")
			var exits string
			var uiHandled int
			if err := store.DB().QueryRow(`SELECT exits,ui_handled FROM voice_turns WHERE turn_id='display'`).Scan(&exits, &uiHandled); err != nil {
				t.Fatal(err)
			}
			wantExits := "task"
			if name == "ui" {
				wantExits = "ui"
			} else if withUI {
				wantExits = "task_ui"
			}
			wantHandled := 0
			if withUI {
				wantHandled = 1
			}
			if exits != wantExits || uiHandled != wantHandled {
				t.Fatalf("exits=%q ui_handled=%d", exits, uiHandled)
			}
			if name == "ui" {
				var tasks, replies, handled int
				_ = store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks)
				_ = store.DB().QueryRow(`SELECT count(*) FROM voice_fast_replies`).Scan(&replies)
				_ = store.DB().QueryRow(`SELECT handoff_handled FROM voice_turns WHERE turn_id='display'`).Scan(&handled)
				if tasks != 0 || replies != 0 || handled != 0 || r.ResourceType != "voice_turn" {
					t.Fatalf("UI-only tasks=%d replies=%d handled=%d receipt=%+v", tasks, replies, handled, r)
				}
				return
			}
			if withUI {
				var requestTask, request string
				if err := store.DB().QueryRow(`SELECT task_id,request FROM ui_requests WHERE voice_turn_id=(SELECT id FROM voice_turns WHERE turn_id='display')`).Scan(&requestTask, &request); err != nil {
					t.Fatal(err)
				}
				if requestTask != r.ResourceID || request != "Research and show the lunar mission information." {
					t.Fatalf("request task=%s request=%q", requestTask, request)
				}
				var jobs int
				_ = store.DB().QueryRow(`SELECT count(*) FROM ui_jobs`).Scan(&jobs)
				if jobs != 0 {
					t.Fatalf("task+ui called controller at register time: jobs=%d", jobs)
				}
			}
			var flag int
			var goal string
			if err := store.DB().QueryRow(`SELECT ui_display,goal FROM tasks WHERE id=?`, r.ResourceID).Scan(&flag, &goal); err != nil {
				t.Fatal(err)
			}
			if (flag == 1) != wantFlag {
				t.Fatalf("display=%d want=%t", flag, wantFlag)
			}
			if strings.HasPrefix(name, "attach_") {
				if r.ResourceID != target {
					t.Fatal("attach created another task")
				}
				var entries int
				if err := store.DB().QueryRow(`SELECT count(*) FROM task_entries WHERE task_id=? AND instr(content,?)>0`, target, testPrompts(t).TaskUIDisplay()).Scan(&entries); err != nil || entries != 0 {
					t.Fatalf("append display definition entries=%d err=%v", entries, err)
				}
			} else {
				if strings.Contains(goal, testPrompts(t).TaskUIDisplay()) {
					t.Fatalf("display definition missing or leaked: %q", goal)
				}
				var initial string
				if err := store.DB().QueryRow(`SELECT content FROM task_entries WHERE task_id=? AND kind='user_message'`, r.ResourceID).Scan(&initial); err != nil || strings.Contains(initial, testPrompts(t).TaskUIDisplay()) {
					t.Fatalf("initial display definition missing or leaked err=%v", err)
				}
				if strings.HasPrefix(name, "complete_") && (r.ResourceID == target || strings.Contains(goal, "previous result")) {
					t.Fatal("continue failed distinct task or polluted full goal")
				}
			}
		})
	}
}

func TestUIDisplaySchema(t *testing.T) {
	_, store, _ := handoffHTTPServer(t)
	db := store.DB()
	for _, tc := range []struct{ table, column string }{{"voice_turns", "exits"}, {"voice_turns", "ui_handled"}, {"tasks", "ui_display"}} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, tc.table, tc.column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s.%s count=%d err=%v", tc.table, tc.column, count, err)
		}
	}
	var legacy int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('voice_turns') WHERE name='triage_category'`).Scan(&legacy); err != nil || legacy != 0 {
		t.Fatalf("legacy column=%d err=%v", legacy, err)
	}
	for _, query := range []string{
		`INSERT INTO ui_surfaces(workspace_id,owner_subject_id,surface_id,container,state_json,version,created_at,updated_at) VALUES('default','owner','main','window','[]',1,'now','now')`,
		`INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,created_at,updated_at) VALUES('request','default','owner','register','now','now')`,
		`INSERT INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,created_at,updated_at) VALUES('request','default','owner','input','now','now')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`UPDATE ui_surfaces SET container='unknown'`, `UPDATE ui_surfaces SET state_json='{}'`,
		`UPDATE ui_surfaces SET width=400`, `UPDATE ui_surfaces SET height=300`,
		`UPDATE ui_requests SET source='unknown'`, `UPDATE ui_requests SET status='pending'`, `UPDATE ui_requests SET task_question=2`,
		`UPDATE ui_feedback_deliveries SET status='generated'`, `UPDATE ui_feedback_deliveries SET attempts=-1`,
		`INSERT INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,created_at,updated_at) VALUES('missing','default','owner','input','now','now')`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatalf("accepted invalid schema value: %s", query)
		}
	}
	var status string
	var attempts int
	if err := db.QueryRow(`SELECT status,attempts FROM ui_feedback_deliveries WHERE request_id='request'`).Scan(&status, &attempts); err != nil || status != "pending" || attempts != 0 {
		t.Fatalf("feedback defaults=%s/%d err=%v", status, attempts, err)
	}
	if _, err := db.Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('ui-schema-turn','default','handoff-session',1,'ui-schema-turn',1,'screen',X'01','now','schema');
INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,voice_turn_id,created_at,updated_at) VALUES('first','default','owner','register','ui-schema-turn','now','now');
INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,created_at,updated_at) VALUES('null-turn','default','owner','task','now','now');`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,voice_turn_id,created_at,updated_at) VALUES('duplicate','default','owner','register','ui-schema-turn','now','now')`); err == nil {
		t.Fatal("duplicate workspace/voice turn request accepted")
	}
}

func TestStewardRedisplayCompletedTask(t *testing.T) {
	for _, mode := range []string{"show", "no_entry", "no_ui", "report", "running_none"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			routes := []string{"task"}
			choice := steward.Choice{Action: steward.ActionNew}
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				out, _ := json.Marshal(map[string]any{"route": strings.Join(routes, "_"), "request": "Show the earlier lunar mission result."})
				_ = json.NewEncoder(w).Encode(map[string]string{"output_text": string(out)})
			}))
			defer model.Close()
			cfg := RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}
			ts, store, key := handoffHTTPServerWithConfig(t, Config{RegisterConfig: &RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: cfg, CompactModel: cfg, CompactCharacters: 1000}, Steward: classificationStewardFunc(func(_ context.Context, input steward.Input) (steward.Choice, error) {
				mu.Lock()
				defer mu.Unlock()
				if input.DisplayRequested != (len(routes) == 2) {
					t.Error("screen_display does not reflect register exits")
				}
				return choice, nil
			})})
			uiTargetSchema(t, store.DB())
			target := commitAndHandoff(t, ts, key, "seed", "research the lunar mission").ResourceID
			status := "complete"
			if mode == "running_none" {
				status = "active"
			}
			if _, err := store.DB().Exec(`UPDATE tasks SET status=?,result='previous result' WHERE id=?`, status, target); err != nil {
				t.Fatal(err)
			}
			if mode != "no_entry" {
				for _, entry := range []struct{ id, at, content string }{{"older", "2026-01-01T00:00:00Z", "old result"}, {"latest", "2026-01-02T00:00:00Z", "latest result"}} {
					if _, err := store.DB().Exec(`INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) SELECT ?,id,workspace_id,'assistant_message',?,(SELECT author_device_id FROM task_entries WHERE task_id=tasks.id LIMIT 1),? FROM tasks WHERE id=?`, entry.id, entry.content, entry.at, target); err != nil {
						t.Fatal(err)
					}
				}
				uiFlowRun(t, store.DB(), target, "redisplay-run")
				payload, _ := json.Marshal(map[string]any{"payload": map[string]string{"task_id": target, "run_id": "redisplay-run", "entry_id": "older"}})
				if _, err := store.DB().Exec(`INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,aggregate_type,aggregate_id,aggregate_version,payload) SELECT 'redisplay-result',workspace_id,current_cursor+1,1,'task.assistant_message','2026-01-01T00:00:00Z','task',?,1,? FROM event_counters WHERE workspace_id='default'`, target, payload); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec(`UPDATE event_counters SET current_cursor=current_cursor+1 WHERE workspace_id='default'`); err != nil {
					t.Fatal(err)
				}
			}
			count := func(query string) int {
				t.Helper()
				var n int
				if err := store.DB().QueryRow(query).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			before := count(`SELECT count(*) FROM task_entries`)
			beforeRuns := count(`SELECT count(*) FROM runs`)
			mu.Lock()
			choice = steward.Choice{Action: steward.ActionRedisplay, TaskID: &target}
			routes = []string{"task", "ui"}
			if mode == "no_ui" {
				routes = []string{"task"}
			}
			if mode == "report" {
				reply := "The work is complete."
				choice = steward.Choice{Action: steward.ActionReport, TaskID: &target, Reply: &reply}
			}
			mu.Unlock()
			commitAndHandoff(t, ts, key, "display", "show the earlier result")
			var storedAction string
			if err := store.DB().QueryRow(`SELECT steward_action FROM voice_turns WHERE turn_id='display'`).Scan(&storedAction); err != nil {
				t.Fatal(err)
			}
			wantAction := string(steward.ActionRedisplay)
			if mode == "report" {
				wantAction = string(steward.ActionReport)
			}
			if storedAction != wantAction {
				t.Fatalf("stored action=%q want=%q", storedAction, wantAction)
			}
			if count(`SELECT count(*) FROM tasks`) != 1 || count(`SELECT handoff_handled FROM voice_turns WHERE turn_id='display'`) != 1 {
				t.Fatal("redisplay admitted work or left handoff unhandled")
			}
			wantEntries := before
			if count(`SELECT count(*) FROM task_entries`) != wantEntries {
				t.Fatal("incorrect task append behavior")
			}
			if mode == "show" || mode == "running_none" || mode == "no_ui" {
				if count(`SELECT ui_handled FROM voice_turns WHERE turn_id='display'`) != 1 || count(`SELECT count(*) FROM ui_requests WHERE source='task' AND request='Show the earlier lunar mission result.'`) != 1 {
					t.Fatal("missing UI receipt or handled marker")
				}
				commitAndHandoff(t, ts, key, "display-again", "show the earlier result")
				// The dispatcher may also create task_entry:older. Only the two
				// turn-keyed jobs belong to these explicit redisplays.
				rows, err := store.DB().Query(`SELECT dedupe_key FROM ui_jobs WHERE kind='task_entry' ORDER BY dedupe_key`)
				if err != nil {
					t.Fatal(err)
				}
				var keys []string
				for rows.Next() {
					var key string
					if err := rows.Scan(&key); err != nil {
						rows.Close()
						t.Fatal(err)
					}
					keys = append(keys, key)
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				rows.Close()
				for _, turn := range []string{"display", "display-again"} {
					var turnID string
					if err := store.DB().QueryRow(`SELECT id FROM voice_turns WHERE turn_id=?`, turn).Scan(&turnID); err != nil {
						t.Fatal(err)
					}
					wantKey := "task_entry:" + target + "#" + turnID
					n := 0
					for _, key := range keys {
						if key == wantKey {
							n++
						}
					}
					if n != 1 {
						t.Fatalf("redisplay job %q count=%d want=1; keys=%v", wantKey, n, keys)
					}
				}
				if display := count(`SELECT ui_display FROM tasks`); display != 1 {
					t.Fatalf("ui_display=%d want=1; keys=%v", display, keys)
				}
				if sources := count(`SELECT count(*) FROM ui_result_sources s JOIN ui_requests r ON r.id=s.request_id JOIN voice_turns v ON v.id=r.voice_turn_id WHERE v.turn_id IN ('display','display-again') AND s.entry_id='older' AND s.run_id='redisplay-run'`); sources != 2 {
					t.Fatalf("redisplays pinned authoritative sources=%d want=2; keys=%v", sources, keys)
				}
				if tasks, entries, runs := count(`SELECT count(*) FROM tasks`), count(`SELECT count(*) FROM task_entries`), count(`SELECT count(*) FROM runs`); tasks != 1 || entries != before || runs != beforeRuns {
					t.Fatalf("redisplays admitted work: tasks=%d want=1 entries=%d want=%d runs=%d want=%d; keys=%v", tasks, entries, before, runs, beforeRuns, keys)
				}
			} else {
				want := stewardErrorSpeech
				if mode == "report" {
					want = "The work is complete."
				}
				var text, intent string
				if err := store.DB().QueryRow(`SELECT text,intent_kind FROM voice_fast_replies WHERE turn_id='display'`).Scan(&text, &intent); err != nil || text != want || intent != "task_report" {
					t.Fatalf("fallback text=%q intent=%q err=%v", text, intent, err)
				}
				if count(`SELECT count(*) FROM ui_jobs`) != 0 || count(`SELECT count(*) FROM ui_requests`) != 0 {
					t.Fatal("report fallback scheduled UI work")
				}
			}
		})
	}
}
