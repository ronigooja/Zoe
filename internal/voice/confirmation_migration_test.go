package voice

import (
	"context"
	"testing"
)

// rollbackVoiceConfirmationBinding applies when fixtures reconstruct a schema
// before migration 216; remove its schema and receipt together.
const rollbackVoiceConfirmationBinding = `ALTER TABLE voice_turns DROP COLUMN pending_task_id;
ALTER TABLE voice_turns DROP COLUMN pending_task_version;
ALTER TABLE voice_turns DROP COLUMN pending_question;
ALTER TABLE voice_turns DROP COLUMN handoff_handled;
DELETE FROM schema_migrations WHERE namespace='voice' AND version=216;`

func TestConfirmationMigrationStartupAndReopen(t *testing.T) {
	_, store, _ := handoffHTTPServer(t)
	// Recheck history as startup does on an existing database. This does not
	// invent a missing migration 215 or claim a 215-to-216 upgrade was tested.
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM schema_migrations WHERE namespace='voice' AND version=216`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration 216 receipts=%d err=%v", count, err)
	}
}

func TestConfirmationMigrationUpgrades215AndKeepsExistingVoiceData(t *testing.T) {
	_, store, _ := handoffHTTPServer(t)
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM schema_migrations WHERE namespace='voice' AND version=215`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Skip("real voice migration 215 must be merged before verifying the 215-to-216 upgrade")
	}
	if _, err := store.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,steward_decision,resumed_task_id) VALUES('confirmation-upgrade','default','handoff-session',1,'confirmation-upgrade',1,'keep delegated request',X'01','2026-11-06T00:00:00Z','confirmation-upgrade','ask','');` + rollbackVoiceConfirmationBinding); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := store.DB().QueryRow(`SELECT max(version) FROM schema_migrations WHERE namespace='voice'`).Scan(&version); err != nil || version != 215 {
		t.Fatalf("pre-upgrade version=%d err=%v", version, err)
	}
	for i := 0; i < 2; i++ {
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var content, decision, pendingID, question string
	var pendingVersion, handled int
	if err := store.DB().QueryRow(`SELECT content,steward_decision,pending_task_id,pending_task_version,pending_question,handoff_handled FROM voice_turns WHERE id='confirmation-upgrade'`).Scan(&content, &decision, &pendingID, &pendingVersion, &question, &handled); err != nil {
		t.Fatal(err)
	}
	if content != "keep delegated request" || decision != "ask" || pendingID != "" || pendingVersion != 0 || question != "" || handled != 0 {
		t.Fatalf("upgrade did not preserve data/defaults: content=%q decision=%q pending=%q/%d question=%q handled=%d", content, decision, pendingID, pendingVersion, question, handled)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM schema_migrations WHERE namespace='voice' AND version=216`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration 216 receipts=%d err=%v", count, err)
	}
}
