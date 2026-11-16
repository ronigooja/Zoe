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
PRIMARY KEY(workspace_id,principal_id));
CREATE TABLE memory_ingest_batches (
id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, principal_id TEXT NOT NULL,
from_ns INTEGER NOT NULL, until_ns INTEGER NOT NULL,
UNIQUE(workspace_id,principal_id,from_ns));
CREATE TABLE memory_ingest_chunks (
batch_id TEXT NOT NULL REFERENCES memory_ingest_batches(id), position INTEGER NOT NULL,
id TEXT NOT NULL UNIQUE, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0,
PRIMARY KEY(batch_id,position));
CREATE TABLE memory_ingest_members (
workspace_id TEXT NOT NULL, principal_id TEXT NOT NULL, source_id TEXT NOT NULL,
batch_id TEXT NOT NULL REFERENCES memory_ingest_batches(id),
PRIMARY KEY(workspace_id,principal_id,source_id));
CREATE TABLE memory_write_outcomes (
op_id TEXT PRIMARY KEY, status TEXT NOT NULL, error_kind TEXT NOT NULL DEFAULT '');`)
		return err
	}})
}
