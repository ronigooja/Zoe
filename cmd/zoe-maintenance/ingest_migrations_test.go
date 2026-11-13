package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestOpenStoreRecognizesAppliedIngestMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "zoe.db")
	store, err := storage.Open(ctx, storage.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := memoryapi.RegisterMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO memory_ingest_state VALUES('workspace','owner',123,456)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	store = reopened
	var boundary, next int64
	if err := store.DB().QueryRow(`SELECT boundary_ns,next_attempt_ms FROM memory_ingest_state`).Scan(&boundary, &next); err != nil || boundary != 123 || next != 456 {
		t.Fatalf("ingestion progress changed: %d %d %v", boundary, next, err)
	}
}
