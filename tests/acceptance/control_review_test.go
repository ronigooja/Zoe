package acceptance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

func reviewToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		t.Fatal(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func reviewServer(t *testing.T, cfg control.Config, register func(*control.Server)) (*storage.Store, http.Handler) {
	t.Helper()
	s := openStore(t, filepath.Join(t.TempDir(), "control.db"))
	if e := storage.RegisterCoreMigrations(s); e != nil {
		t.Fatal(e)
	}
	if e := control.RegisterInstanceMaintenanceMigration(s); e != nil {
		t.Fatal(e)
	}
	if e := s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	srv, e := control.NewServer(s, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if register != nil {
		register(srv)
	}
	if cfg.RunnerToken != "" {
		scopes := cfg.RunnerScopes
		if len(scopes) == 0 {
			scopes = []string{"runner:claim", "runner:observe", "runner:control:read", "runner:propose", "runner:budget"}
		}
		if err := srv.RegisterRunnerCredential(context.Background(), cfg.RunnerToken, cfg.RunnerID, scopes); err != nil {
			t.Fatal(err)
		}
	}
	return s, bindReviewHandler(srv.Handler(), s)
}
func reviewRequest(h http.Handler, method, path, token, key string, body any) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("X-Zoe-Installation-ID", "11111111-1111-4111-8111-111111111111")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type reviewAuditActor struct {
	ID          string
	WorkspaceID string
}
type reviewInstanceIdentity struct{ Device reviewAuditActor }
type reviewHandler struct {
	http.Handler
	store *storage.Store
}

func reviewInitializeInstance(t *testing.T, h http.Handler, code, token string) reviewInstanceIdentity {
	t.Helper()
	fixture, ok := h.(*reviewHandler)
	if !ok {
		t.Fatal("test handler needs explicit store binding")
	}
	if _, _, err := control.InitializeInstanceAuth(context.Background(), fixture.store, token); err != nil {
		t.Fatal(err)
	}
	return reviewInstanceIdentity{Device: reviewAuditActor{ID: "instance", WorkspaceID: "default"}}
}
func bindReviewHandler(h http.Handler, store *storage.Store) http.Handler {
	return &reviewHandler{Handler: h, store: store}
}
func TestReviewHTTPIdentityAndReceipts(t *testing.T) {
	code := reviewToken(t)
	token := reviewToken(t)
	s, h := reviewServer(t, control.Config{}, nil)
	reviewInitializeInstance(t, h, code, token)
	req := contracts.CommandRequest{Kind: "project.create", Payload: json.RawMessage(`{"name":"review"}`)}
	first := reviewRequest(h, "POST", "/v1/commands", token, "project", req)
	if first.Code != 200 {
		t.Fatalf("command=%d %s", first.Code, first.Body.String())
	}
	var receipt contracts.CommandReceipt
	json.Unmarshal(first.Body.Bytes(), &receipt)
	second := reviewRequest(h, "POST", "/v1/commands", token, "project", req)
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Error("duplicate changed receipt")
	}
	req.Payload = json.RawMessage(`{"name":"different"}`)
	if w := reviewRequest(h, "POST", "/v1/commands", token, "project", req); w.Code != 409 {
		t.Errorf("mismatch=%d", w.Code)
	}
	got := reviewRequest(h, "GET", "/v1/commands?key=project", token, "", nil)
	if !bytes.Equal(got.Body.Bytes(), first.Body.Bytes()) {
		t.Error("lost ACK lookup changed receipt")
	}
	for _, table := range []string{"projects", "command_receipts", "command_work", "events"} {
		var n int
		if e := s.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil {
			t.Fatal(e)
		}
		want := 1
		if table == "events" {
			want = 1
		}
		if n != want {
			t.Errorf("%s rows=%d", table, n)
		}
	}
	var hashes int
	if e := s.DB().QueryRow("SELECT count(*) FROM instance_auth WHERE length(key_hash)=32").Scan(&hashes); e != nil {
		t.Fatal(e)
	}
	if hashes != 1 {
		t.Error("hashed credential storage missing")
	}
}

func TestReviewExtensionRollback(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	s, h := reviewServer(t, control.Config{}, func(srv *control.Server) {
		if e := srv.RegisterCommand("review.fail", func(ctx context.Context, tx *sql.Tx, c control.CommandContext, r contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
			_, e := tx.ExecContext(ctx, "INSERT INTO projects VALUES('partial',?,'partial','','active',1,'now','now')", c.Principal.WorkspaceID)
			if e != nil {
				return contracts.CommandResult{}, nil, e
			}
			return contracts.CommandResult{}, nil, errors.New("injected extension failure")
		}); e != nil {
			t.Fatal(e)
		}
	})
	reviewInitializeInstance(t, h, code, token)
	w := reviewRequest(h, "POST", "/v1/commands", token, "rollback", contracts.CommandRequest{Kind: "review.fail", Payload: json.RawMessage(`{}`)})
	if w.Code != 500 {
		t.Fatalf("failure status=%d", w.Code)
	}
	for _, table := range []string{"projects", "command_receipts", "command_work", "events"} {
		var n int
		if e := s.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil {
			t.Fatal(e)
		}
		want := 0

		if n != want {
			t.Errorf("partial %s persisted", table)
		}
	}
}
func TestReviewCanonicalHashPreservesLargeIntegers(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	_, h := reviewServer(t, control.Config{}, func(s *control.Server) {
		s.RegisterCommand("review.integer", func(ctx context.Context, tx *sql.Tx, c control.CommandContext, r contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
			return contracts.CommandResult{}, nil, nil
		})
	})
	reviewInitializeInstance(t, h, code, token)
	for i, payload := range []string{`{"revision":9007199254740992}`, `{"revision":9007199254740993}`} {
		w := reviewRequest(h, "POST", "/v1/commands", token, "large", contracts.CommandRequest{Kind: "review.integer", Payload: json.RawMessage(payload)})
		want := 200
		if i == 1 {
			want = 409
		}
		if w.Code != want {
			t.Errorf("request %d status=%d want=%d: distinct integers collide", i, w.Code, want)
		}
	}
}
func TestReviewConfiguredRunnerRevocation(t *testing.T) {
	token := reviewToken(t)
	s, h := reviewServer(t, control.Config{RunnerToken: token, RunnerID: "review"}, func(srv *control.Server) {
		if e := srv.RegisterRunnerCredential(context.Background(), token, "review", []string{"runner:claim"}); e != nil {
			t.Fatal(e)
		}
		srv.RegisterRunnerRoutes(func(mux *http.ServeMux) {
			mux.HandleFunc("GET /runner/v1/review", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		})
	})
	if _, e := s.DB().Exec("UPDATE runner_credentials SET revoked_at='now'"); e != nil {
		t.Fatal(e)
	}
	if w := reviewRequest(h, "GET", "/runner/v1/review", token, "", nil); w.Code < 400 {
		t.Errorf("configured bearer remains authorized after persistent revocation: status=%d", w.Code)
	}
}

func TestReviewExplicitInstanceScopesAreNotExpanded(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	_, h := reviewServer(t, control.Config{InstanceScopes: []string{"client:command"}}, nil)
	reviewInitializeInstance(t, h, code, token)
	if w := reviewRequest(h, "GET", "/v1/snapshot", token, "", nil); w.Code != http.StatusForbidden {
		t.Errorf("restricted device gained read scope: %d", w.Code)
	}
	if w := reviewRequest(h, "POST", "/v1/commands", token, "project", contracts.CommandRequest{Kind: "project.create", Payload: json.RawMessage(`{"name":"allowed"}`)}); w.Code != http.StatusOK {
		t.Errorf("configured command grant was lost: %d", w.Code)
	}
	if w := reviewRequest(h, "POST", "/v1/commands", token, "approve-device", contracts.CommandRequest{Kind: "device.approve", Payload: json.RawMessage(`{"device_id":"anything"}`)}); w.Code < 400 {
		t.Errorf("device management was implicitly granted: %d", w.Code)
	}
}

func TestReviewLegacyDeviceApprovalCannotCreateSession(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	_, h := reviewServer(t, control.Config{}, nil)
	reviewInitializeInstance(t, h, code, token)
	w := reviewRequest(h, "POST", "/v1/commands", token, "approve", contracts.CommandRequest{Kind: "device.approve", Payload: json.RawMessage(`{"device_id":"legacy"}`)})
	if w.Code < 400 {
		t.Fatal("legacy device approval remains available")
	}
}
func TestReviewSSERevocationBlocksNewEvents(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	s, h := reviewServer(t, control.Config{}, nil)
	reviewInitializeInstance(t, h, code, token)
	live := httptest.NewServer(h)
	defer live.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	r, e := http.NewRequestWithContext(ctx, "GET", live.URL+"/v1/events", nil)
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	response, e := live.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("SSE=%d", response.StatusCode)
	}
	// Simulate the authoritative revoke transaction before a later workspace event.
	if _, e = s.DB().Exec("UPDATE instance_auth SET generation=generation+1"); e != nil {
		t.Fatal(e)
	}
	payload := `{"schema_version":1,"event_id":"after-revoke","cursor":"3","type":"review.after_revoke","occurred_at":"2026-10-21T00:00:00Z","workspace_id":"default","aggregate_type":"review","aggregate_id":"review","aggregate_version":1,"payload":{}}`
	if _, e = s.DB().Exec("INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,aggregate_type,aggregate_id,aggregate_version,payload) VALUES('after-revoke','default',3,1,'review.after_revoke','now','review','review',1,?)", payload); e != nil {
		t.Fatal(e)
	}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "review.after_revoke") {
			t.Error("revoked SSE credential received an event committed after revocation")
			break
		}
	}
}

func TestReviewLateCommitFailureRollsBackReceiptWorkAndOutbox(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	s, h := reviewServer(t, control.Config{}, nil)
	reviewInitializeInstance(t, h, code, token)
	if _, e := s.DB().Exec(`CREATE TRIGGER review_abort_work BEFORE INSERT ON command_work BEGIN SELECT RAISE(ABORT,'injected final work failure'); END`); e != nil {
		t.Fatal(e)
	}
	w := reviewRequest(h, "POST", "/v1/commands", token, "late-failure", contracts.CommandRequest{Kind: "project.create", Payload: json.RawMessage(`{"name":"late"}`)})
	if w.Code != 500 {
		t.Fatalf("status=%d", w.Code)
	}
	for _, table := range []string{"projects", "command_receipts", "command_work", "events"} {
		var n int
		if e := s.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil {
			t.Fatal(e)
		}
		want := 0

		if n != want {
			t.Errorf("%s survived failed work insertion", table)
		}
	}
	var cursor int
	if e := s.DB().QueryRow("SELECT current_cursor FROM event_counters WHERE singleton=1").Scan(&cursor); e != nil {
		t.Fatal(e)
	}
	if cursor != 0 {
		t.Error("event cursor increment survived rollback")
	}
}

func TestReviewProtectedRoutesAndReceiptIsolation(t *testing.T) {
	code, token, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
	_, h := reviewServer(t, control.Config{RunnerToken: runnerToken}, func(srv *control.Server) {
		srv.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) {
			mux.HandleFunc("GET /v1/review", func(w http.ResponseWriter, r *http.Request) {
				p, ok := control.PrincipalFromContext(r.Context())
				if !ok || p.DeviceID == "" {
					t.Error("missing trusted principal")
				}
				w.WriteHeader(204)
			})
		})
		srv.RegisterRunnerRoutes(func(mux *http.ServeMux) {
			mux.HandleFunc("GET /runner/v1/review", func(w http.ResponseWriter, r *http.Request) {
				if _, ok := control.RunnerPrincipalFromContext(r.Context()); !ok {
					t.Error("missing runner principal")
				}
				w.WriteHeader(204)
			})
		})
	})
	reviewInitializeInstance(t, h, code, token)
	for _, test := range []struct {
		path, token string
		status      int
	}{{"/v1/review", "", 401}, {"/v1/review", runnerToken, 401}, {"/v1/review", token, 204}, {"/runner/v1/review", token, 401}, {"/runner/v1/review", runnerToken, 204}, {"/review", token, 404}} {
		if w := reviewRequest(h, "GET", test.path, test.token, "", nil); w.Code != test.status {
			t.Errorf("%s got=%d want=%d", test.path, w.Code, test.status)
		}
	}
	req := contracts.CommandRequest{Kind: "project.create", Payload: json.RawMessage(`{"name":"private workspace"}`)}
	w := reviewRequest(h, "POST", "/v1/commands", token, "shared-key", req)
	if w.Code != 200 {
		t.Fatal("command failed")
	}
	var receipt contracts.CommandReceipt
	if e := json.Unmarshal(w.Body.Bytes(), &receipt); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/v1/commands/" + receipt.CommandID, "/v1/commands?key=shared-key", "/v1/snapshot"} {
		if w := reviewRequest(h, "GET", path, reviewToken(t), "", nil); w.Code != 401 {
			t.Errorf("wrong key on %s=%d", path, w.Code)
		}
	}

}

func TestReviewApprovalCannotResurrectStoppedTask(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	cfg := control.Config{DefaultProfileID: "review", Profiles: map[string]control.RuntimeProfile{"review": {ID: "review", RuntimeRouteID: "review", ModelRouteID: "review", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1"}}}
	s, h := reviewServer(t, cfg, nil)
	reviewInitializeInstance(t, h, code, token)
	w := reviewRequest(h, "POST", "/v1/commands", token, "create-task", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"review","goal":"review","source":"user"}`)})
	if w.Code != 200 {
		t.Fatalf("create=%d", w.Code)
	}
	var receipt contracts.CommandReceipt
	if e := json.Unmarshal(w.Body.Bytes(), &receipt); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB().Exec("INSERT INTO approvals(id,workspace_id,task_id,status,prompt,created_at,task_version) VALUES('pending','default',?,'pending','Review','2026-10-21T00:00:00Z',1)", receipt.ResourceID); e != nil {
		t.Fatal(e)
	}
	stop, _ := json.Marshal(map[string]string{"task_id": receipt.ResourceID})
	if w := reviewRequest(h, "POST", "/v1/commands", token, "stop-task", contracts.CommandRequest{Kind: "task.stop", Payload: stop}); w.Code != 200 {
		t.Fatalf("stop=%d", w.Code)
	}
	approve, _ := json.Marshal(map[string]string{"task_id": receipt.ResourceID, "approval_id": "pending", "decision": "approve"})
	w = reviewRequest(h, "POST", "/v1/commands", token, "approve-stopped", contracts.CommandRequest{Kind: "task.approve", Payload: approve})
	if w.Code < 400 {
		t.Errorf("pending approval revived canceled task: status=%d", w.Code)
	}
	var status string
	if e := s.DB().QueryRow("SELECT status FROM tasks WHERE id=?", receipt.ResourceID).Scan(&status); e != nil {
		t.Fatal(e)
	}
	if status != "canceled" {
		t.Errorf("stopped task became %s", status)
	}
}

func TestReviewRevocationRecheckedInsideMutationTransactions(t *testing.T) {
	code, token := reviewToken(t), reviewToken(t)
	var store *storage.Store
	store, h := reviewServer(t, control.Config{}, func(srv *control.Server) {
		srv.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) {
			mux.HandleFunc("POST /v1/review-stale-session", func(w http.ResponseWriter, r *http.Request) {
				p, _ := control.PrincipalFromContext(r.Context())
				// Deterministically put a revoke commit between authentication middleware
				// and both mutation entry points, reproducing an in-flight logout race.
				if _, err := store.DB().ExecContext(r.Context(), `UPDATE instance_auth SET generation=generation+1`); err != nil {
					t.Fatal(err)
				}
				called := false
				err := srv.DeviceTransaction(r.Context(), func(*sql.Tx, contracts.Principal) error { called = true; return nil })
				if api, ok := err.(*control.APIError); !ok || api.Status != 401 || called {
					t.Fatalf("stale DeviceTransaction=%v called=%v", err, called)
				}
				_, err = srv.ExecuteCommand(r.Context(), p, "stale-command", contracts.CommandRequest{Kind: "project.create", Payload: json.RawMessage(`{"name":"stale"}`)}, []byte("stale"))
				if api, ok := err.(*control.APIError); !ok || api.Status != 401 {
					t.Fatalf("stale ExecuteCommand=%v", err)
				}
				w.WriteHeader(200)
			})
		})
	})
	reviewInitializeInstance(t, h, code, token)
	if w := reviewRequest(h, "POST", "/v1/review-stale-session", token, "", nil); w.Code != 200 {
		t.Fatalf("stale request fixture=%d", w.Code)
	}
	var projects int
	if err := store.DB().QueryRow(`SELECT count(*) FROM projects`).Scan(&projects); err != nil || projects != 0 {
		t.Fatalf("stale mutation committed: %d %v", projects, err)
	}
}
