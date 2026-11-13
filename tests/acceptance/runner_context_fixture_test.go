package acceptance

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
)

func runnerContextServer(t *testing.T, cfg control.Config) (*storage.Store, http.Handler) {
	t.Helper()
	store := openStore(t, filepath.Join(t.TempDir(), "runner-context.db"))
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
	if err := memoryapi.RegisterMigrations(store); err != nil {
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
