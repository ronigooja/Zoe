package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/storage"
)

func instanceAuthFixture(t *testing.T) (*storage.Store, *Server, string) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, err := GenerateInstanceKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = InitializeInstanceAuth(ctx, store, key); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	return store, server, key
}
func instanceRequest(handler http.Handler, method, path, key string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	digest := sha256.Sum256([]byte(path + body))
	r.Header.Set("Idempotency-Key", hex.EncodeToString(digest[:]))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestInstanceKeyProtectsEveryBusinessRouteAndRemovesOwnerEndpoints(t *testing.T) {
	store, server, key := instanceAuthFixture(t)
	handler := server.Handler()
	wrong, _ := GenerateInstanceKey()
	if got := instanceRequest(handler, "GET", "/healthz", "", "").Code; got != 200 {
		t.Fatal(got)
	}
	for _, path := range []string{"/v1/snapshot", "/v1/commands", "/v1/events", "/v1/export", "/v1/memory", "/v1/voice/config", "/v1/admin/instance", "/v1/unknown"} {
		for _, credential := range []string{"", wrong, "invalid"} {
			if got := instanceRequest(handler, "GET", path, credential, "").Code; got != 401 {
				t.Errorf("%s unauthenticated=%d", path, got)
			}
		}
	}
	for _, path := range []string{"/v1/auth/status", "/v1/auth/setup", "/v1/auth/login", "/v1/auth/session", "/v1/auth/logout", "/v1/bootstrap", "/v1/pairings", "/v1/devices"} {
		if got := instanceRequest(handler, "POST", path, key, `{}`).Code; got != 404 {
			t.Errorf("removed %s=%d", path, got)
		}
	}
	for _, kind := range []string{"device.approve", "device.revoke", "project.member.add", "project.member.remove"} {
		body, _ := json.Marshal(contracts.CommandRequest{Kind: kind, Payload: json.RawMessage(`{}`)})
		if got := instanceRequest(handler, "POST", "/v1/commands", key, string(body)).Code; got < 400 {
			t.Errorf("removed command %s=%d", kind, got)
		}
	}
	response := instanceRequest(handler, "GET", "/v1/snapshot", key, "")
	if response.Code != 200 {
		t.Fatalf("snapshot=%d %s", response.Code, response.Body.String())
	}
	var snapshot contracts.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil || snapshot.WorkspaceID != "default" || snapshot.SubjectID != "owner" {
		t.Fatal("unstable authority")
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM actors`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("synthetic actors=%d %v", count, err)
	}
}

func TestPendingInstanceAuthProtectsBusinessRoutesAndBecomesReadyAfterInit(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err = InitializeInstanceAuthPending(ctx, store, ""); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	pending := instanceRequest(handler, http.MethodGet, "/readyz", "", "")
	if pending.Code != http.StatusServiceUnavailable || pending.Body.String() != `{"reason":"instance_key_not_initialized","status":"pending"}
` {
		t.Fatalf("pending readiness=%d body=%q", pending.Code, pending.Body.String())
	}
	if got := instanceRequest(handler, http.MethodGet, "/v1/snapshot", "", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("pending snapshot=%d", got)
	}
	if got := instanceRequest(handler, http.MethodPost, "/v1/commands", "", `{}`).Code; got != http.StatusUnauthorized {
		t.Fatalf("pending command=%d", got)
	}
	key, err := GenerateInstanceKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = InitializeInstanceAuth(ctx, store, key); err != nil {
		t.Fatal(err)
	}
	ready := instanceRequest(handler, http.MethodGet, "/readyz", "", "")
	if ready.Code != http.StatusOK || ready.Body.String() != `{"status":"ready"}
` {
		t.Fatalf("ready readiness=%d body=%q", ready.Code, ready.Body.String())
	}
	if got := instanceRequest(handler, http.MethodGet, "/v1/snapshot", key, "").Code; got != http.StatusOK {
		t.Fatalf("initialized snapshot=%d", got)
	}
}
func TestRotationFencesInflightMutationsAndPersistsAcrossInitialization(t *testing.T) {
	store, server, key := instanceAuthFixture(t)
	next, _ := GenerateInstanceKey()
	var stale contracts.Principal
	if err := server.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/capture", func(w http.ResponseWriter, r *http.Request) {
			stale, _ = PrincipalFromContext(r.Context())
			w.WriteHeader(204)
		})
	}); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	if got := instanceRequest(handler, "GET", "/v1/capture", key, "").Code; got != 204 {
		t.Fatal(got)
	}
	if err := RotateInstanceKey(context.Background(), store, next); err != nil {
		t.Fatal(err)
	}
	if got := instanceRequest(handler, "GET", "/v1/snapshot", key, "").Code; got != 401 {
		t.Fatal("old key accepted")
	}
	if got := instanceRequest(handler, "GET", "/v1/snapshot", next, "").Code; got != 200 {
		t.Fatal(got)
	}
	tx, err := store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = requireOwnerSessionTx(context.Background(), tx, stale)
	_ = tx.Rollback()
	if api, ok := err.(*APIError); !ok || api.Status != 401 {
		t.Fatalf("stale generation=%v", err)
	}
	if _, _, err = InitializeInstanceAuth(context.Background(), store, key); err == nil {
		t.Fatal("restart restored stale deployment key")
	}
	if _, _, err = InitializeInstanceAuth(context.Background(), store, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(`DELETE FROM instance_auth`); err != nil {
		t.Fatal(err)
	}
	if err = RotateInstanceKey(context.Background(), store, next); err == nil {
		t.Fatal("zero-row rotation succeeded")
	}
}
func TestInternalCredentialsRemainScopedAndRevokedAfterRegistrationRetry(t *testing.T) {
	store, server, key := instanceAuthFixture(t)
	runner, _ := GenerateInstanceKey()
	ingest, _ := GenerateInstanceKey()
	credentials := []struct{ token, id, scope string }{{runner, "runner", "runner:claim"}, {ingest, "ingest", "voice:turn:ingest"}}
	for _, entry := range credentials {
		if err := server.RegisterRunnerCredential(context.Background(), entry.token, entry.id, []string{"default"}, []string{entry.scope}); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.RegisterRunnerRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /runner/v1/test", func(w http.ResponseWriter, r *http.Request) {
			p, ok := RunnerPrincipalFromContext(r.Context())
			if !ok || p.RunnerID != "runner" || !HasRunnerScope(p, "runner:claim", "default") {
				t.Error("generic runner route did not receive the execution runner principal")
			}
			w.WriteHeader(204)
		})
	}); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	for _, entry := range credentials {
		if got := instanceRequest(handler, "GET", "/v1/snapshot", entry.token, "").Code; got != 401 {
			t.Errorf("worker owner access=%d", got)
		}
		want := http.StatusNoContent
		if entry.id == "ingest" {
			want = http.StatusForbidden
		}
		if got := instanceRequest(handler, "GET", "/runner/v1/test", entry.token, "").Code; got != want {
			t.Fatalf("%s generic runner access=%d, want %d", entry.id, got, want)
		}
	}
	if got := instanceRequest(handler, "GET", "/runner/v1/test", key, "").Code; got != 401 {
		t.Fatal("owner key became worker")
	}
	if _, err := store.DB().Exec(`UPDATE runner_credentials SET revoked_at='revoked'`); err != nil {
		t.Fatal(err)
	}
	for _, entry := range credentials {
		if err := server.RegisterRunnerCredential(context.Background(), entry.token, entry.id, []string{"default"}, []string{entry.scope}); err != nil {
			t.Fatal(err)
		}
		if got := instanceRequest(handler, "GET", "/runner/v1/test", entry.token, "").Code; got != 401 {
			t.Fatalf("registration revived revoked %s credential: status=%d", entry.id, got)
		}
	}
}
func TestInitializationRollsBackIfAuditActorCannotBeCreated(t *testing.T) {
	store, _, _ := instanceAuthFixture(t)
	if _, err := store.DB().Exec(`DELETE FROM instance_auth;DELETE FROM actors;CREATE TRIGGER reject_actor BEFORE INSERT ON actors BEGIN SELECT RAISE(ABORT,'injected actor failure');END`); err != nil {
		t.Fatal(err)
	}
	key, _ := GenerateInstanceKey()
	if _, _, err := InitializeInstanceAuth(context.Background(), store, key); err == nil {
		t.Fatal("actor failure ignored")
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM instance_auth`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("half-provisioned credential=%d %v", count, err)
	}
}

func TestOneKeySupportsConcurrentClientsAndIndependentPushInstallations(t *testing.T) {
	store, server, key := instanceAuthFixture(t)
	handler := server.Handler()
	const first = "11111111-1111-4111-8111-111111111111"
	const second = "22222222-2222-4222-8222-222222222222"
	register := func(installation, token string) string {
		t.Helper()
		body, _ := json.Marshal(contracts.CommandRequest{Kind: "device.push_token.register", Payload: json.RawMessage(`{"platform":"ios","installation_id":"` + installation + `","token":"` + token + `"}`)})
		response := instanceRequest(handler, "POST", "/v1/commands", key, string(body))
		if response.Code != 200 {
			t.Fatalf("register=%d %s", response.Code, response.Body.String())
		}
		var receipt contracts.CommandReceipt
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		return receipt.ResourceID
	}
	firstID := register(first, strings.Repeat("a", 64))
	secondID := register(second, strings.Repeat("b", 64))
	register(first, strings.Repeat("c", 64))
	var active int
	if err := store.DB().QueryRow(`SELECT count(*) FROM device_push_tokens WHERE status='active'`).Scan(&active); err != nil || active != 2 {
		t.Fatalf("independent active registrations=%d %v", active, err)
	}
	body := `{"kind":"device.push_token.revoke","payload":{"installation_id":"` + first + `","token_id":"` + secondID + `"}}`
	if response := instanceRequest(handler, "POST", "/v1/commands", key, body); response.Code != 404 {
		t.Fatalf("cross-installation revoke=%d %s", response.Code, response.Body.String())
	}
	if firstID == secondID {
		t.Fatal("independent targets share identifier")
	}
	results := make(chan int, 8)
	for i := 0; i < 8; i++ {
		go func() { results <- instanceRequest(handler, "GET", "/v1/snapshot", key, "").Code }()
	}
	for i := 0; i < 8; i++ {
		if status := <-results; status != 200 {
			t.Fatalf("concurrent shared-key read=%d", status)
		}
	}
	var actors int
	if err := store.DB().QueryRow(`SELECT count(*) FROM actors`).Scan(&actors); err != nil || actors != 1 {
		t.Fatalf("client use created auth identities=%d %v", actors, err)
	}
}

func TestOwnerAndInternalWorkerCredentialsCannotShareASecret(t *testing.T) {
	store, server, key := instanceAuthFixture(t)
	if err := server.RegisterRunnerCredential(context.Background(), key, "worker", []string{"default"}, []string{"runner:claim"}); err == nil {
		t.Fatal("owner secret registered as internal worker")
	}
	worker, _ := GenerateInstanceKey()
	if err := server.RegisterRunnerCredential(context.Background(), worker, "worker", []string{"default"}, []string{"runner:claim"}); err != nil {
		t.Fatal(err)
	}
	if err := RotateInstanceKey(context.Background(), store, worker); err == nil {
		t.Fatal("rotation promoted internal worker secret to owner")
	}
}
