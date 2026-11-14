package voice

import (
	"context"
	"database/sql"
	"github.com/ronigooja/Zoe/internal/storage"
)

func registerUIJobMigration(store *storage.Store) error {
	return store.RegisterMigration(storage.Migration{Namespace: "voice", Version: 226, Name: "ui_controller_jobs", Up: func(ctx context.Context, tx *sql.Tx) error {
		for _, c := range []struct{ table, name string }{{"voice_turns", "restatement"}, {"ui_requests", "restatement"}, {"ui_requests", "utterance"}} {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name=?)`, c.table, c.name).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				if _, err := tx.ExecContext(ctx, `ALTER TABLE `+c.table+` ADD COLUMN `+c.name+` TEXT`); err != nil {
					return err
				}
			}
		}
		_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS ui_jobs (
id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), owner_subject_id TEXT NOT NULL,
kind TEXT NOT NULL CHECK(kind IN ('register','task_entry')), dedupe_key TEXT NOT NULL UNIQUE,
ui_request_id TEXT NOT NULL REFERENCES ui_requests(id), status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','done','failed')),
attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0), next_attempt_at TEXT NOT NULL, last_error TEXT,
created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS ui_jobs_due ON ui_jobs(status,next_attempt_at);
CREATE TABLE IF NOT EXISTS ui_task_cursor(workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),last_cursor INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS ui_feedback_due ON ui_feedback_deliveries(status,next_attempt_at);`)
		return err
	}})
}
