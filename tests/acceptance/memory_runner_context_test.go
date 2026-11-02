package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memory"
	"github.com/ronigooja/Zoe/internal/runner"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestRunnerClaimJSONRoundTripsMemoryContextThroughRealControlClient(t *testing.T) {
	bootstrap, device, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
	profile := control.RuntimeProfile{ID: "worker", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1", Scopes: []string{"memory:read:personal"}}
	store, handler := memoryRunnerServer(t, control.Config{RunnerToken: runnerToken, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim"}, DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": profile}})
	reviewInitializeInstance(t, handler, bootstrap, device)
	created := reviewRequest(handler, http.MethodPost, "/v1/commands", device, "task", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"memory task","goal":"SQLite","source":"user"}`)})
	if created.Code != http.StatusOK {
		t.Fatalf("task create=%d %s", created.Code, created.Body.String())
	}
	remembered := reviewRequest(handler, http.MethodPost, "/v1/commands", device, "memory", contracts.CommandRequest{Kind: "memory.remember", Payload: json.RawMessage(`{"scope":{"kind":"personal"},"subject":"database","key":"choice","content":"Use SQLite for local transactions","source_kind":"user_note"}`)})
	if remembered.Code != http.StatusOK {
		t.Fatalf("memory remember=%d %s", remembered.Code, remembered.Body.String())
	}
	live := httptest.NewServer(handler)
	defer live.Close()
	client, err := runner.NewControlClient(live.URL, runnerToken, live.Client())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := client.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || len(claim.MemoryContext.Items) != 1 || claim.MemoryContext.Items[0].Content != "Use SQLite for local transactions" {
		t.Fatalf("claim memory context=%+v", claim)
	}
	if len(claim.MemoryContext.Manifest.Entries) != 1 || len(claim.MemoryContext.Manifest.Entries[0].SourceIDs) != 1 {
		t.Fatalf("claim manifest=%+v", claim.MemoryContext.Manifest)
	}
	var sourceCount int
	if err := store.DB().QueryRow(`SELECT count(*) FROM memory_revision_sources WHERE claim_id=?`, claim.MemoryContext.Manifest.Entries[0].ClaimID).Scan(&sourceCount); err != nil || sourceCount != 1 {
		t.Fatalf("source links=%d err=%v", sourceCount, err)
	}
}

func TestRunnerClaimMemoryContextExcludesAnotherSubjectsPersonalMemory(t *testing.T) {
	bootstrap, device, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
	profile := control.RuntimeProfile{ID: "worker", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1", Scopes: []string{"memory:read:personal"}}
	store, handler := memoryRunnerServer(t, control.Config{RunnerToken: runnerToken, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim"}, DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": profile}})
	reviewInitializeInstance(t, handler, bootstrap, device)
	created := reviewRequest(handler, http.MethodPost, "/v1/commands", device, "task", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"subject scope","goal":"scope","source":"user"}`)})
	if created.Code != http.StatusOK {
		t.Fatalf("task create=%d %s", created.Code, created.Body.String())
	}
	repo := memory.NewRepository(store.DB())
	ctx := context.Background()
	other := memory.Scope{Kind: memory.ScopePersonal, PrincipalID: "subject-b"}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sourceContent := "only subject B can see this"
	source := memory.Source{ID: "subject-b-source", Version: 1, Scope: other, Kind: "user_note", Reference: "command:subject-b-source", ContentHash: sha256Hex(sourceContent), RecordedAt: now, Producer: "user"}
	_, err = repo.PutTx(ctx, tx, memory.WriteInput{WorkspaceID: "default", ActorID: "subject-b", Source: source, Claim: memory.Claim{WorkspaceID: "default", Scope: other, Subject: "private", Key: "fact", Content: sourceContent, Authority: memory.AuthorityUserConfirmed, Status: memory.ClaimActive, Sources: []memory.Source{source}}})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	live := httptest.NewServer(handler)
	defer live.Close()
	client, err := runner.NewControlClient(live.URL, runnerToken, live.Client())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := client.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil {
		t.Fatal("claim was nil")
	}
	for _, item := range claim.MemoryContext.Items {
		if item.Content == "only subject B can see this" {
			t.Fatalf("subject B personal memory leaked into subject A claim: %+v", claim.MemoryContext.Items)
		}
	}
}

func sha256Hex(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func TestRunnerClaimMemoryContextExcludesUnverifiedAndUnsourcedClaims(t *testing.T) {
	bootstrap, device, runnerToken := reviewToken(t), reviewToken(t), reviewToken(t)
	profile := control.RuntimeProfile{ID: "worker", RuntimeRouteID: "runtime", ModelRouteID: "model", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1", Scopes: []string{"memory:read:personal"}}
	store, handler := memoryRunnerServer(t, control.Config{RunnerToken: runnerToken, RunnerID: "runner", RunnerWorkspaceIDs: []string{"default"}, RunnerScopes: []string{"runner:claim"}, DefaultProfileID: "worker", Profiles: map[string]control.RuntimeProfile{"worker": profile}})
	reviewInitializeInstance(t, handler, bootstrap, device)
	created := reviewRequest(handler, http.MethodPost, "/v1/commands", device, "task", contracts.CommandRequest{Kind: "task.create", Payload: json.RawMessage(`{"title":"verified scope","goal":"verification","source":"user"}`)})
	if created.Code != http.StatusOK {
		t.Fatalf("task create=%d %s", created.Code, created.Body.String())
	}
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// These rows model persisted legacy/corrupt candidates; the claim path must
	// still fail closed when authority is inferred or its source is unavailable.
	for _, row := range []struct {
		id, authority, content string
	}{
		{id: "inferred-claim", authority: "model_inferred", content: "inferred must not be disclosed"},
		{id: "unsourced-claim", authority: "user_confirmed", content: "missing source must not be disclosed"},
	} {
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO memory_claims(id,workspace_id,scope_kind,scope_id,principal_id,subject,claim_key,authority_kind,status,revision,valid_from,valid_to,supersedes_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, row.id, "default", "personal", "", "instance", "verification", row.id, row.authority, "active", 1, nil, nil, nil, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO memory_revisions(claim_id,revision,content,authority_kind,status,recorded_at,actor_id,extractor_version) VALUES(?,?,?,?,?,?,?,?)`, row.id, 1, row.content, row.authority, "active", now, "test", "test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO memory_sources(workspace_id,id,version,scope_kind,scope_id,principal_id,kind,reference,content_hash,recorded_at,producer) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "default", "inferred-source", 1, "personal", "", "instance", "user_note", "command:inferred-source", "", now, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO memory_revision_sources(claim_id,revision,workspace_id,source_id,source_version) VALUES(?,?,?,?,?)`, "inferred-claim", 1, "default", "inferred-source", 1); err != nil {
		t.Fatal(err)
	}

	live := httptest.NewServer(handler)
	defer live.Close()
	client, err := runner.NewControlClient(live.URL, runnerToken, live.Client())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := client.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil {
		t.Fatal("claim was nil")
	}
	if len(claim.MemoryContext.Items) != 0 {
		t.Fatalf("unverified or unsourced claims entered MemoryContext: %+v", claim.MemoryContext.Items)
	}
}

func memoryRunnerServer(t *testing.T, cfg control.Config) (*storage.Store, http.Handler) {
	t.Helper()
	store := openStore(t, filepath.Join(t.TempDir(), "memory-runner.db"))
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	srv, err := control.NewServer(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Register(srv, store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterRunnerCredential(context.Background(), cfg.RunnerToken, cfg.RunnerID, cfg.RunnerWorkspaceIDs, cfg.RunnerScopes); err != nil {
		t.Fatal(err)
	}
	return store, bindReviewHandler(srv.Handler(), store)
}
