package acceptance

import (
	"context"
	"testing"

	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func TestFreshInstanceSchemaHasNoProductTenancy(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, register := range []func(*storage.Store) error{storage.RegisterCoreMigrations, control.RegisterInstanceMaintenanceMigration, capabilities.RegisterMigrations, memoryapi.RegisterMigrations, voice.RegisterMigrations} {
		if err := register(store); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='workspaces'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("product tenancy table remains")
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type='table' AND p.name IN ('workspace_id','workspace_ids')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("%d product tenancy columns remain", count)
	}
}
