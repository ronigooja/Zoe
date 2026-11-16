package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/uictl"
)

func uiCapSeed(t *testing.T, r *deliveryRig, live, dead int) {
	t.Helper()
	for i := 0; i < live+dead; i++ {
		id := fmt.Sprintf("stored-%d", i)
		deleted := i >= live
		r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,surface_id,surface_version,created_at,updated_at) VALUES(?,'default','owner','register',?,7,'now','now')`, id, id)
		r.exec(t, `INSERT INTO ui_surfaces(workspace_id,owner_subject_id,surface_id,container,state_json,version,request_id,deleted,created_at,updated_at) VALUES('default','owner',?,'window','[]',7,?,?,'now','now')`, id, id, deleted)
	}
}

func uiCapCommit(t *testing.T, r *deliveryRig, target string) error {
	t.Helper()
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,request,created_at,updated_at) VALUES('new','default','owner','register','Show the requested screen.','now','now')`)
	r.exec(t, `INSERT INTO ui_jobs(id,workspace_id,owner_subject_id,kind,dedupe_key,ui_request_id,attempts,next_attempt_at,created_at,updated_at) VALUES('new-job','default','owner','register','register:new','new',1,'now','now','now')`)
	b, err := r.svc.uiInput(context.Background(), uiJob{w: "default", o: "owner", request: "new"})
	if err != nil {
		return err
	}
	var input uictl.Input
	if err = json.Unmarshal(b, &input); err != nil {
		return err
	}
	output, err := uictl.Parse(uiFlowOutput(strings.ReplaceAll(uiFlowCreate+","+uiFlowRoot, "screen", target), false), nil)
	if err != nil {
		return err
	}
	return r.svc.commitUIJob(context.Background(), uiJob{id: "new-job", w: "default", o: "owner", request: "new", attempts: 1}, output, nil, input.Surfaces)
}

func TestUILiveSurfaceCapExcludesTombstones(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		live, dead   int
		allowed      bool
		version      int
	}{
		{"deleted-new", "fresh", 0, 64, true, 1},
		{"active-new", "fresh", 64, 0, false, 0},
		{"recreate-free", "stored-63", 63, 1, true, 8},
		{"recreate-full", "stored-64", 64, 1, false, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := uiTargetRig(t)
			uiFlowServer(t, r)
			uiCapSeed(t, r, tc.live, tc.dead)
			err := uiCapCommit(t, r, tc.target)
			if tc.allowed && err != nil {
				t.Fatal(err)
			}
			if !tc.allowed && !errors.Is(err, errUIState) {
				t.Fatalf("cap error=%v", err)
			}
			var live int
			_ = r.store.DB().QueryRow(`SELECT count(*) FROM ui_surfaces WHERE deleted=0`).Scan(&live)
			expected := tc.live
			if tc.allowed {
				expected++
			}
			if live != expected {
				t.Fatalf("live=%d", live)
			}
			if tc.allowed {
				var version int
				_ = r.store.DB().QueryRow(`SELECT version FROM ui_surfaces WHERE surface_id=?`, tc.target).Scan(&version)
				if version != tc.version {
					t.Fatalf("version=%d", version)
				}
			}
		})
	}
}

func TestUISnapshotScopedHTTP(t *testing.T) {
	r := uiTargetRig(t)
	uiFlowServer(t, r)
	uiTargetQuestion(t, r)
	uiTargetPresentation(t, r, true)
	r.exec(t, `UPDATE ui_surfaces SET width=400,height=300 WHERE surface_id='screen'`)
	// A foreign owner's document must never enter the snapshot, even via parameters.
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,surface_id,surface_version,created_at,updated_at) VALUES('secret','default','other','register','secret',1,'now','now')`)
	r.exec(t, `INSERT INTO ui_surfaces(workspace_id,owner_subject_id,surface_id,container,state_json,version,request_id,created_at,updated_at) VALUES('default','other','secret','window','[]',1,'secret','now','now')`)
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := control.InitializeInstanceAuth(context.Background(), r.store, key); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE actors SET subject_id='owner' WHERE id='instance'`)
	if err := r.svc.uiEventServer.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) { mux.HandleFunc("GET /v1/ui/snapshot", r.svc.handleUISnapshot) }); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(r.svc.uiEventServer.Handler())
	defer server.Close()
	fetch := func(auth bool) (int, uiSnapshot) {
		req, _ := http.NewRequest("GET", server.URL+"/v1/ui/snapshot?owner_subject_id=other", nil)
		if auth {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var got uiSnapshot
		_ = json.NewDecoder(response.Body).Decode(&got)
		return response.StatusCode, got
	}
	status, _ := fetch(false)
	if status != 401 {
		t.Fatal(status)
	}
	status, got := fetch(true)
	if status != 200 || len(got.Surfaces) != 1 {
		t.Fatalf("%d %+v", status, got)
	}
	sf := got.Surfaces[0]
	if sf.RequestID != "request" || sf.SurfaceID != "screen" || sf.SurfaceVersion != 1 || !sf.AwaitingInput || sf.TaskID != "task" || sf.QuestionID != "question" || sf.RenderStatus != "generated" || sf.Size == nil {
		t.Fatalf("%+v", sf)
	}
	var cursor int64
	_ = r.store.DB().QueryRow(`SELECT current_cursor FROM event_counters WHERE workspace_id='default'`).Scan(&cursor)
	if got.Cursor != cursor {
		t.Fatal("cursor mismatch")
	}
	empty, err := r.svc.readUISnapshot(context.Background(), contracts.Principal{WorkspaceID: "another", SubjectID: "owner"})
	if err != nil || len(empty.Surfaces) != 0 {
		t.Fatalf("%+v %v", empty, err)
	}
	uiTargetDelete(t, r)
	status, got = fetch(true)
	if status != 200 || len(got.Surfaces) != 1 {
		t.Fatalf("%d %+v", status, got)
	}
	sf = got.Surfaces[0]
	raw, _ := json.Marshal(sf)
	if !sf.Deleted || sf.SurfaceVersion != 2 || sf.RequestID != "delete" || sf.AwaitingInput || sf.Size != nil || strings.Contains(string(raw), "Title") || !strings.Contains(string(raw), "deleteSurface") {
		t.Fatal(string(raw))
	}
	r.exec(t, `UPDATE ui_surfaces SET request_id='secret' WHERE owner_subject_id='owner'`)
	status, _ = fetch(true)
	if status != 500 {
		t.Fatalf("invalid map status=%d", status)
	}
}

func TestUISnapshotCursorAndDocumentsShareReadTransaction(t *testing.T) {
	// WAL permits a writer to commit between the two reads, proving no mixed time.
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "snapshot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	_, err = db.Exec(`PRAGMA journal_mode=WAL;
 CREATE TABLE event_counters(workspace_id TEXT PRIMARY KEY,current_cursor INTEGER);
 INSERT INTO event_counters VALUES('default',1);
 CREATE TABLE ui_requests(id TEXT,workspace_id TEXT,owner_subject_id TEXT,surface_id TEXT,surface_version INTEGER,status TEXT,awaiting_input INTEGER,task_id TEXT,question_id TEXT);
 INSERT INTO ui_requests VALUES('old','default','owner','screen',1,'generated',0,NULL,NULL);
 CREATE TABLE ui_surfaces(surface_id TEXT,version INTEGER,deleted INTEGER,state_json TEXT,container TEXT,width INTEGER,height INTEGER,request_id TEXT,workspace_id TEXT,owner_subject_id TEXT);
 INSERT INTO ui_surfaces VALUES('screen',1,0,'[]','window',NULL,NULL,'old','default','owner');`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var cursor int64
	if err = tx.QueryRow(`SELECT current_cursor FROM event_counters WHERE workspace_id='default'`).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`BEGIN; UPDATE event_counters SET current_cursor=2; UPDATE ui_requests SET id='new',surface_version=2; UPDATE ui_surfaces SET version=2,request_id='new',deleted=1; COMMIT;`)
	if err != nil {
		t.Fatal(err)
	}
	surfaces, err := readUISnapshotSurfaces(context.Background(), tx, contracts.Principal{WorkspaceID: "default", SubjectID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 1 || len(surfaces) != 1 || surfaces[0].RequestID != "old" || surfaces[0].SurfaceVersion != 1 || surfaces[0].Deleted {
		t.Fatalf("%d %+v", cursor, surfaces)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	svc := newService(db, Config{})
	defer svc.recallCancel()
	latest, err := svc.readUISnapshot(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Cursor != 2 || latest.Surfaces[0].RequestID != "new" || !latest.Surfaces[0].Deleted {
		t.Fatalf("%+v", latest)
	}
}
