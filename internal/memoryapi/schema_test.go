package memoryapi

import (
	"context"
	"testing"

	"github.com/ronigooja/Zoe/internal/storage"
)

func TestMigration222FreshAndUpgrade(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade"}[upgrade], func(t *testing.T) {
			ctx := context.Background()
			store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := storage.RegisterCoreMigrations(store); err != nil {
				t.Fatal(err)
			}
			if upgrade {
				if err := store.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec(`CREATE TABLE memory_batch_boundaries(workspace_id TEXT,principal_id TEXT,boundary_ns INTEGER); INSERT INTO memory_batch_boundaries VALUES('workspace','owner',99)`); err != nil {
					t.Fatal(err)
				}
			}
			if err := RegisterMigrations(store); err != nil {
				t.Fatal(err)
			}
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			var version, count int
			if err := store.DB().QueryRow(`SELECT count(*),max(version) FROM schema_migrations WHERE namespace='memory_ingest'`).Scan(&count, &version); err != nil || count != 1 || version != 222 {
				t.Fatalf("migration=%d/%d err=%v", count, version, err)
			}
			if _, err := store.DB().Exec(`INSERT INTO memory_ingest_state(workspace_id,principal_id,boundary_ns) VALUES('workspace','owner',0)`); err != nil {
				t.Fatal(err)
			}
			var next int64
			if err := store.DB().QueryRow(`SELECT next_attempt_ms FROM memory_ingest_state`).Scan(&next); err != nil || next != 0 {
				t.Fatalf("default=%d err=%v", next, err)
			}
			if _, err := store.DB().Exec(`INSERT INTO memory_ingest_state(workspace_id,principal_id,boundary_ns) VALUES('workspace','owner',1)`); err == nil {
				t.Fatal("missing primary key")
			}
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if upgrade {
				var old int64
				if err := store.DB().QueryRow(`SELECT boundary_ns FROM memory_batch_boundaries`).Scan(&old); err != nil || old != 99 {
					t.Fatalf("old table changed: %d %v", old, err)
				}
			}
		})
	}
}
