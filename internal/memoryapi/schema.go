package memoryapi

import (
	"context"
	"database/sql"

	"github.com/ronigooja/Zoe/internal/storage"
)

// RegisterMigrations keeps worker ingestion progress independent of legacy claims.
func RegisterMigrations(store *storage.Store) error {
	return store.RegisterMigration(storage.Migration{Namespace: "memory_ingest", Version: 222, Name: "memory_ingest_state", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE memory_ingest_state (
workspace_id TEXT NOT NULL, principal_id TEXT NOT NULL,
boundary_ns INTEGER NOT NULL, next_attempt_ms INTEGER NOT NULL DEFAULT 0,
PRIMARY KEY(workspace_id,principal_id));`)
		return err
	}})
}
