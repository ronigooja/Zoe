package voice

import (
	"context"
	"database/sql"

	"github.com/ronigooja/Zoe/internal/storage"
)

func registerUIMigrations(store *storage.Store) error {
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 223, Name: "voice_multi_exit", Up: func(ctx context.Context, tx *sql.Tx) error {
		// Upgrade fixtures may retain the latest columns while replaying receipts.
		for _, column := range []struct{ name, definition string }{
			{"exits", "TEXT"},
			{"ui_handled", "INTEGER NOT NULL DEFAULT 0 CHECK(ui_handled IN (0,1))"},
		} {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('voice_turns') WHERE name=?)`, column.name).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				if _, err := tx.ExecContext(ctx, `ALTER TABLE voice_turns ADD COLUMN `+column.name+` `+column.definition); err != nil {
					return err
				}
			}
		}
		var legacy bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('voice_turns') WHERE name='triage_category')`).Scan(&legacy); err != nil {
			return err
		}
		if legacy {
			_, err := tx.ExecContext(ctx, `ALTER TABLE voice_turns DROP COLUMN triage_category`)
			return err
		}
		return nil
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 224, Name: "task_ui_display", Up: func(ctx context.Context, tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='ui_display')`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		_, err := tx.ExecContext(ctx, `ALTER TABLE tasks ADD COLUMN ui_display INTEGER NOT NULL DEFAULT 0 CHECK(ui_display IN (0,1))`)
		return err
	}}); err != nil {
		return err
	}
	if err := store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 225, Name: "ui_display_storage", Up: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS ui_surfaces (
workspace_id TEXT NOT NULL REFERENCES workspaces(id), owner_subject_id TEXT NOT NULL,
surface_id TEXT NOT NULL, container TEXT NOT NULL CHECK(container IN ('statusItem','popover','window')),
state_json TEXT NOT NULL CHECK(json_valid(state_json) AND json_type(state_json)='array'),
version INTEGER NOT NULL CHECK(version >= 0), created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
PRIMARY KEY(workspace_id,owner_subject_id,surface_id));
CREATE TABLE IF NOT EXISTS ui_requests (
id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), owner_subject_id TEXT NOT NULL,
source TEXT NOT NULL CHECK(source IN ('register','task')), voice_turn_id TEXT REFERENCES voice_turns(id),
task_id TEXT REFERENCES tasks(id), surface_id TEXT,
awaiting_input INTEGER NOT NULL DEFAULT 0 CHECK(awaiting_input IN (0,1)), input_note TEXT,
status TEXT NOT NULL DEFAULT 'generated' CHECK(status IN ('generated','rendered','input_received','feedback_delivered','failed')),
created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS ui_requests_voice_turn ON ui_requests(workspace_id,voice_turn_id) WHERE voice_turn_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS ui_feedback_deliveries (
request_id TEXT PRIMARY KEY REFERENCES ui_requests(id), workspace_id TEXT NOT NULL REFERENCES workspaces(id),
owner_subject_id TEXT NOT NULL, text TEXT NOT NULL,
status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','delivered','failed')),
attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0), next_attempt_at TEXT, last_error TEXT,
created_at TEXT NOT NULL, updated_at TEXT NOT NULL);`)
		return err
	}}); err != nil {
		return err
	}
	return registerUIJobMigration(store)
}
