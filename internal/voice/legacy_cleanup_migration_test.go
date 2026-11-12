package voice

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

// rollbackVoiceLegacyCleanup applies when fixtures reconstruct the pre-221
// schema; restore removed objects and remove the cleanup receipt together.
const rollbackVoiceLegacyCleanup = `ALTER TABLE voice_turns ADD COLUMN steward_decision TEXT NOT NULL DEFAULT '';
ALTER TABLE voice_turns ADD COLUMN pending_task_id TEXT NOT NULL DEFAULT '';
ALTER TABLE voice_turns ADD COLUMN pending_task_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE voice_turns ADD COLUMN pending_question TEXT NOT NULL DEFAULT '';
CREATE TABLE voice_instruction_deliveries (
session_id TEXT NOT NULL REFERENCES voice_sessions(id), media_generation INTEGER NOT NULL,
instruction_version TEXT NOT NULL, instruction_delivery TEXT NOT NULL, attempts INTEGER NOT NULL,
PRIMARY KEY(session_id,media_generation,instruction_version));
DELETE FROM schema_migrations WHERE namespace='voice' AND version=221;`

func assertVoiceLegacyStateRemoved(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('voice_turns') WHERE name IN ('steward_decision','pending_task_id','pending_task_version','pending_question')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retired columns=%d err=%v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='voice_instruction_deliveries'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retired table=%d err=%v", count, err)
	}
}

func TestMigration221RemovesLegacyStateAndReplaysWithoutDataLoss(t *testing.T) {
	_, store, _ := handoffHTTPServer(t)
	db := store.DB()
	if _, err := db.Exec(rollbackVoiceLegacyCleanup + `
INSERT INTO voice_instruction_deliveries VALUES('handoff-session',1,'legacy','delivered',1);
INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,provider_message_id,content,content_hash,confirmed_at,command_id,triage_category,resumed_task_id,handoff_handled,steward_action,steward_task_id,report_text,steward_decision,pending_task_id,pending_task_version,pending_question)
VALUES('cleanup','default','handoff-session',1,'cleanup',2,'provider','keep verbatim',X'0123','2026-11-12T00:00:00Z','command','task','resumed',1,'attach','assigned','keep report','ask','obsolete',3,'obsolete question');`); err != nil {
		t.Fatal(err)
	}
	var baseline int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE namespace='voice'`).Scan(&baseline); err != nil || baseline != 220 {
		t.Fatalf("pre-upgrade version=%d err=%v", baseline, err)
	}
	// Compare every surviving column and its value, not only a selected subset.
	columns := []string{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('voice_turns') WHERE name NOT IN ('steward_decision','pending_task_id','pending_task_version','pending_question') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		t.Helper()
		rows, err := db.Query(`SELECT * FROM voice_turns WHERE id='cleanup'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		names, err := rows.Columns()
		if err != nil || !rows.Next() {
			t.Fatalf("missing turn: %v", err)
		}
		values := make([]any, len(names))
		pointers := make([]any, len(names))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		all := map[string]any{}
		for i, name := range names {
			all[name] = values[i]
		}
		kept := map[string]any{}
		for _, name := range columns {
			value, ok := all[name]
			if !ok {
				t.Fatalf("surviving column missing: %s", name)
			}
			kept[name] = value
		}
		return kept
	}
	before := read()
	for i := 0; i < 3; i++ {
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertVoiceLegacyStateRemoved(t, db)
		if after := read(); !reflect.DeepEqual(before, after) {
			t.Fatalf("surviving data changed: before=%v after=%v", before, after)
		}
		var version int
		if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE namespace='voice'`).Scan(&version); err != nil || version != 221 {
			t.Fatalf("latest version=%d err=%v", version, err)
		}
		if i == 0 {
			// Replay with all removed objects absent.
			if _, err := db.Exec(`DELETE FROM schema_migrations WHERE namespace='voice' AND version=221`); err != nil {
				t.Fatal(err)
			}
		} else if i == 1 {
			// Replay a partially reconstructed schema: guard each column separately.
			if _, err := db.Exec(`ALTER TABLE voice_turns ADD COLUMN pending_question TEXT NOT NULL DEFAULT ''; DELETE FROM schema_migrations WHERE namespace='voice' AND version=221`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
