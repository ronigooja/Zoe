package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
