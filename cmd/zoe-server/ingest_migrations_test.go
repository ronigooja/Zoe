package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestKeyCommandMigrationsIncludeIngestState(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := registerKeyCommandMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM schema_migrations WHERE namespace='memory_ingest' AND version=222 AND name='memory_ingest_state'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ingestion migration count=%d err=%v", count, err)
	}
}

func TestKeyCommandMigrationsRecognizeIngestState(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade"}[upgrade], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "zoe.db")
			store, err := storage.Open(ctx, storage.Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := storage.RegisterCoreMigrations(store); err != nil {
				t.Fatal(err)
			}
			if upgrade {
				if err := store.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
			}
			// Simulate a server database with 222 already applied before the
			// offline key command registers its own migration list.
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
			reopened, err := storage.Open(ctx, storage.Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			store = reopened
			if err := storage.RegisterCoreMigrations(store); err != nil {
				t.Fatal(err)
			}
			if err := registerKeyCommandMigrations(store); err != nil {
				t.Fatal(err)
			}
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			var boundary, next int64
			if err := store.DB().QueryRow(`SELECT boundary_ns,next_attempt_ms FROM memory_ingest_state`).Scan(&boundary, &next); err != nil || boundary != 123 || next != 456 {
				t.Fatalf("ingestion progress changed: %d %d %v", boundary, next, err)
			}
		})
	}
}
