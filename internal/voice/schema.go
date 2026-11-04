package voice

import (
	"context"
	"database/sql"

	"github.com/ronigooja/Zoe/internal/storage"
)

func RegisterMigrations(store *storage.Store) error {
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 200, Name: "voice_sessions_and_turns_v1", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE voice_sessions (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  owner_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  request_hash BLOB NOT NULL,
  gateway_session_id TEXT NOT NULL DEFAULT '',
  generation INTEGER NOT NULL CHECK(generation > 0),
  state TEXT NOT NULL CHECK(state IN ('allocating','active','ending','ended','failed','quarantined')),
  offer_answer BLOB NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  heartbeat_at TEXT,
  ended_at TEXT,
  last_error_code TEXT NOT NULL DEFAULT '',
  UNIQUE(workspace_id,owner_id,request_id)
);
CREATE UNIQUE INDEX voice_sessions_owner_lease ON voice_sessions(workspace_id,owner_id)
  WHERE state IN ('allocating','active','ending');
CREATE INDEX voice_sessions_device ON voice_sessions(workspace_id,device_id,state);
CREATE TABLE voice_turns (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  session_id TEXT NOT NULL REFERENCES voice_sessions(id),
  generation INTEGER NOT NULL,
  turn_id TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK(revision > 0),
  provider_message_id TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL,
  content_hash BLOB NOT NULL,
  confirmed_at TEXT NOT NULL,
  command_id TEXT NOT NULL,
  UNIQUE(session_id,generation,turn_id,revision)
);
CREATE INDEX voice_turns_session ON voice_turns(session_id,generation,confirmed_at);
CREATE TABLE voice_task_links (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  session_id TEXT NOT NULL REFERENCES voice_sessions(id),
  generation INTEGER NOT NULL,
  turn_id TEXT NOT NULL,
  turn_revision INTEGER NOT NULL,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  linked_by_device_id TEXT NOT NULL,
  command_id TEXT NOT NULL,
  linked_at TEXT NOT NULL,
  PRIMARY KEY(session_id,generation,turn_id,turn_revision),
  UNIQUE(workspace_id,task_id)
);
CREATE INDEX voice_task_links_task ON voice_task_links(workspace_id,task_id);
CREATE TABLE voice_pool_state (
  id INTEGER PRIMARY KEY CHECK(id=1),
  blocked INTEGER NOT NULL CHECK(blocked IN (0,1)),
  reason_code TEXT NOT NULL DEFAULT '',
  source_session_id TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  updated_by TEXT NOT NULL DEFAULT ''
);
INSERT INTO voice_pool_state(id,blocked,updated_at) VALUES(1,0,strftime('%Y-%m-%dT%H:%M:%fZ','now'));
CREATE TABLE voice_pool_reconciliations (
  id TEXT PRIMARY KEY,
  actor_id TEXT NOT NULL,
  source_session_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  reason TEXT NOT NULL,
  method TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE voice_account_terminations (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES voice_sessions(id),
  generation INTEGER NOT NULL,
  account_id_hash BLOB NOT NULL,
  lease_id_hash BLOB NOT NULL,
  evidence_id TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  adapter_kind TEXT NOT NULL
);
`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 201, Name: "voice_stable_turn_task_binding", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX voice_task_links_stable_turn ON voice_task_links(session_id,generation,turn_id)`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 202, Name: "voice_account_lease_identity", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE voice_sessions ADD COLUMN account_lease_id TEXT NOT NULL DEFAULT '';
ALTER TABLE voice_sessions ADD COLUMN account_lease_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE voice_sessions ADD COLUMN account_id_hash BLOB NOT NULL DEFAULT '';`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 203, Name: "voice_gateway_unbind_receipt", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE voice_sessions ADD COLUMN gateway_unbound INTEGER NOT NULL DEFAULT 0 CHECK(gateway_unbound IN (0,1))`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 204, Name: "voice_gateway_unbind_unknown_fence", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE voice_sessions ADD COLUMN gateway_unbind_unknown INTEGER NOT NULL DEFAULT 0 CHECK(gateway_unbind_unknown IN (0,1));
UPDATE voice_sessions SET gateway_unbind_unknown=1 WHERE state='quarantined' AND last_error_code IN ('gateway_release_unknown','gateway_release_failed')`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 205, Name: "voice_gateway_media_generation", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE voice_sessions ADD COLUMN gateway_media_generation INTEGER NOT NULL DEFAULT 0`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 206, Name: "voice_opening_context_delivery", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE voice_sessions ADD COLUMN opening_context_delivery TEXT NOT NULL DEFAULT 'unsupported' CHECK(opening_context_delivery IN ('delivered','unverified','unsupported'));
ALTER TABLE voice_sessions ADD COLUMN opening_context_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE voice_sessions ADD COLUMN opening_context_chars INTEGER NOT NULL DEFAULT 0;
ALTER TABLE voice_sessions ADD COLUMN opening_context_manifest BLOB NOT NULL DEFAULT '';
ALTER TABLE voice_sessions ADD COLUMN gateway_context_chars INTEGER NOT NULL DEFAULT 0`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 207, Name: "voice_result_deliveries", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE voice_result_deliveries (
  entry_id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  subject_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','delivered','failed','skipped_no_call','skipped_stale')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error_code TEXT NOT NULL DEFAULT '',
  voice_session_id TEXT NOT NULL DEFAULT '',
  generation INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

 CREATE INDEX voice_result_deliveries_due ON voice_result_deliveries(status,next_attempt_at,created_at);
 CREATE TABLE voice_result_cursor (id INTEGER PRIMARY KEY CHECK(id=1), last_cursor INTEGER NOT NULL);
 INSERT INTO voice_result_cursor(id,last_cursor) VALUES(1,0);`)
		return err
	}}); err != nil {
		return err
	}
	return nil
}
