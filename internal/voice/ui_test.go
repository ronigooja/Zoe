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
				out, _ := json.Marshal(map[string]any{"routes": routes, "restatement": "none"})
				_ = json.NewEncoder(w).Encode(map[string]string{"output_text": string(out)})
			}))
			defer model.Close()
			cfg := RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}
			ts, store, key := handoffHTTPServerWithConfig(t, Config{RegisterConfig: &RegisterConfig{Model: cfg, CompactModel: cfg, CompactCharacters: 1000}, Steward: classificationStewardFunc(func(context.Context, string, string, string, []steward.Candidate) (steward.Choice, error) {
				mu.Lock()
				defer mu.Unlock()
				return choice, nil
			})})
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
				choice = steward.Choice{Action: steward.ActionAttach, Request: "research the lunar mission"}
				mu.Unlock()
			}
			withUI := name == "task_ui" || name == "attach_sets" || name == "complete_sets" || name == "attach_queued" || name == "attach_waiting" || name == "attach_awaiting_user"
			wantFlag := withUI || name == "attach_keeps"
			mu.Lock()
			if name == "ui" {
				routes = []string{"ui"}
			} else if withUI {
				routes = []string{"ui", "task"}
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
				wantExits = "task,ui"
			}
			if exits != wantExits || uiHandled != 0 {
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
				if err := store.DB().QueryRow(`SELECT count(*) FROM task_entries WHERE task_id=? AND instr(content,?)>0`, target, TaskUIDisplayInstruction).Scan(&entries); err != nil || (entries > 0) != withUI {
					t.Fatalf("append display definition entries=%d err=%v", entries, err)
				}
			} else {
				if strings.Contains(goal, TaskUIDisplayInstruction) != wantFlag {
					t.Fatalf("display definition missing or leaked: %q", goal)
				}
				var initial string
				if err := store.DB().QueryRow(`SELECT content FROM task_entries WHERE task_id=? AND kind='user_message'`, r.ResourceID).Scan(&initial); err != nil || strings.Contains(initial, TaskUIDisplayInstruction) != wantFlag {
					t.Fatalf("initial display definition missing or leaked err=%v", err)
				}
				if strings.HasPrefix(name, "complete_") && (r.ResourceID == target || !strings.Contains(goal, "previous result")) {
					t.Fatal("complete attach did not create follow-up with previous result")
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
		`INSERT INTO ui_surfaces VALUES('default','owner','main','window','[]',1,'now','now')`,
		`INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,created_at,updated_at) VALUES('request','default','owner','register','now','now')`,
		`INSERT INTO ui_feedback_deliveries(request_id,workspace_id,owner_subject_id,text,created_at,updated_at) VALUES('request','default','owner','input','now','now')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`UPDATE ui_surfaces SET container='unknown'`, `UPDATE ui_surfaces SET state_json='{}'`,
		`UPDATE ui_requests SET source='unknown'`, `UPDATE ui_requests SET status='pending'`, `UPDATE ui_requests SET awaiting_input=2`,
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

func TestUIMigrationsUpgradeAndReplay(t *testing.T) {
	_, store, _ := handoffHTTPServer(t)
	db := store.DB()
	if _, err := db.Exec(`DROP TABLE ui_feedback_deliveries; DROP TABLE ui_requests; DROP TABLE ui_surfaces;
ALTER TABLE tasks DROP COLUMN ui_display;
ALTER TABLE voice_turns DROP COLUMN exits;
ALTER TABLE voice_turns DROP COLUMN ui_handled;
ALTER TABLE voice_turns ADD COLUMN triage_category TEXT;
INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,triage_category) VALUES('ui-upgrade','default','handoff-session',1,'ui-upgrade',1,'retained sentence',X'01','now','upgrade','task');
DELETE FROM schema_migrations WHERE namespace='voice' AND version>=223;`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		var text string
		var exits any
		var handled int
		if err := db.QueryRow(`SELECT content,exits,ui_handled FROM voice_turns WHERE id='ui-upgrade'`).Scan(&text, &exits, &handled); err != nil || text != "retained sentence" || exits != nil || handled != 0 {
			t.Fatalf("upgrade content=%q exits=%v handled=%d err=%v", text, exits, handled, err)
		}
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE namespace='voice' AND version>=223`); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
