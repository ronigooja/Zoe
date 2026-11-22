package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

func recoveryRig(t *testing.T) (*deliveryRig, contracts.Principal) {
	t.Helper()
	r := &deliveryRig{}
	r.clock.Store(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC).UnixNano())
	store, err := storage.Open(context.Background(), storage.Config{Path: filepath.Join(t.TempDir(), "recovery.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	r.store = store
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := control.RegisterInstanceMaintenanceMigration(r.store); err != nil {
		t.Fatal(err)
	}
	if err := r.store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.svc = newService(store.DB(), Config{Now: r.now})
	uiFlowServer(t, r)
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := control.InitializeInstanceAuth(context.Background(), r.store, key); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := r.store.DB().QueryRow(`SELECT generation FROM instance_auth WHERE singleton=1`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	return r, contracts.Principal{WorkspaceID: "default", SubjectID: "owner", DeviceID: "instance", Generation: generation}
}

func TestGlobalRecoveryContractOwnershipAndAuthentication(t *testing.T) {
	r, p := recoveryRig(t)
	uiTargetQuestion(t, r)
	uiTargetPresentation(t, r, true)
	r.seedCall(t, "owner-call", "owner", 7)
	r.seedCall(t, "foreign-call", "other", 9)
	r.exec(t, `INSERT INTO ui_requests(id,workspace_id,owner_subject_id,source,surface_id,surface_version,created_at,updated_at) VALUES('secret','default','other','register','secret',1,'now','now')`)
	r.exec(t, `INSERT INTO ui_surfaces(workspace_id,owner_subject_id,surface_id,container,state_json,version,request_id,created_at,updated_at) VALUES('default','other','secret','window','[]',1,'secret','now','now')`)
	if err := r.svc.uiEventServer.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) { mux.HandleFunc("GET /v1/recovery", r.svc.handleRecovery) }); err != nil {
		t.Fatal(err)
	}
	for _, auth := range []bool{false, true} {
		req := httptest.NewRequest("GET", "/v1/recovery?owner_subject_id=other&workspace_id=secret", nil)
		if auth {
			req.Header.Set("Authorization", "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		}
		w := httptest.NewRecorder()
		r.svc.uiEventServer.Handler().ServeHTTP(w, req)
		if !auth {
			if w.Code != 401 {
				t.Fatalf("unauthenticated: %d", w.Code)
			}
			continue
		}
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret") {
			t.Fatal("foreign owner leaked")
		}
		if strings.Contains(w.Body.String(), "foreign-call") {
			t.Fatal("foreign session leaked")
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) != 4 || raw["cursor"] == nil || raw["snapshot"] == nil || raw["ui"] == nil || raw["voice"] == nil {
			t.Fatalf("DTO keys: %v", raw)
		}
		var got recoverySnapshot
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Cursor != got.Snapshot.EventCursor || len(got.Snapshot.Tasks) != 1 || len(got.UI.Surfaces) != 1 || got.Snapshot.SubjectID != "owner" || got.Snapshot.Authority.Generation < 1 {
			t.Fatalf("%+v", got)
		}
		if got.Voice.CurrentSession == nil || got.Voice.CurrentSession.ID != "owner-call" || got.Voice.CurrentSession.Generation != 7 || got.Voice.CurrentSession.State != "active" {
			t.Fatalf("voice=%+v", got.Voice)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cacheable checkpoint")
		}
	}
	p.Generation++
	if _, err := r.svc.readRecovery(context.Background(), p); err == nil {
		t.Fatal("stale generation accepted")
	}
	p.Generation--
	p.SubjectID = "other"
	if _, err := r.svc.readRecovery(context.Background(), p); err == nil {
		t.Fatal("foreign principal accepted")
	}
}

func TestGlobalRecoveryAtomicSnapshot(t *testing.T) {
	r, p := recoveryRig(t)
	uiTargetQuestion(t, r)
	uiTargetPresentation(t, r, true)
	// WAL permits a second connection to commit while the reader retains its view.
	r.exec(t, `PRAGMA journal_mode=WAL`)
	r.store.DB().SetMaxOpenConns(2)
	tx, err := r.store.DB().BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	before, err := r.svc.readRecoveryTx(context.Background(), tx, p)
	if err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE event_counters SET current_cursor=current_cursor+1 WHERE singleton=1`)
	r.exec(t, `UPDATE tasks SET version=version+1,result='new result' WHERE id='task'`)
	r.exec(t, `UPDATE voice_pool_state SET blocked=1,reason_code='release_unknown' WHERE id=1`)
	after, err := r.svc.readRecoveryTx(context.Background(), tx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("mixed checkpoint: before=%+v after=%+v", before, after)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	latest, err := r.svc.readRecovery(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Cursor == before.Cursor || !latest.Voice.Pool.Blocked || latest.Snapshot.Tasks[0].Result != "new result" {
		t.Fatalf("latest=%+v", latest)
	}
}
