package storage

import (
	"context"
	"strings"
	"testing"
)

func TestPreviousAuthorityBaselineRequiresFreshInstance(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.DB().Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,name TEXT NOT NULL,applied_at TEXT NOT NULL,namespace TEXT NOT NULL); INSERT INTO schema_migrations VALUES(1,'core_authority','now','core')`); err != nil {
		t.Fatal(err)
	}
	if err = RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "fresh Zoe instance") {
		t.Fatalf("previous baseline was not rejected: %v", err)
	}
	var name string
	if err = store.DB().QueryRow(`SELECT name FROM schema_migrations WHERE version=1`).Scan(&name); err != nil || name != "core_authority" {
		t.Fatalf("previous baseline was changed: %q, %v", name, err)
	}
}

func TestInstanceMigrationRejectsLegacyAuthority(t *testing.T) {
	for _, schema := range []string{`CREATE TABLE owner_auth(id INTEGER PRIMARY KEY)`, `CREATE TABLE devices(id TEXT PRIMARY KEY)`} {
		t.Run(schema, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(ctx, Config{Path: ":memory:"})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, err = store.DB().Exec(schema); err != nil {
				t.Fatal(err)
			}
			if err = RegisterCoreMigrations(store); err != nil {
				t.Fatal(err)
			}
			if err = store.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "fresh") {
				t.Fatalf("legacy accepted: %v", err)
			}
		})
	}
}
