package voice

import (
	"context"
	"testing"
)

func TestInstructionMigration214To215KeepsExistingData(t *testing.T) {
	_, store, _ := handoffHTTPServer(t)
	if _, err := store.DB().Exec(rollbackVoiceConfirmationBinding); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DROP TABLE voice_instruction_deliveries;
DELETE FROM schema_migrations WHERE namespace='voice' AND version=215;
INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,steward_decision) VALUES('legacy215','default','handoff-session',1,'legacy215',1,'keep voice data',X'01','2026-11-06T00:00:00Z','legacy215','execute')`); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := store.DB().QueryRow(`SELECT max(version) FROM schema_migrations WHERE namespace='voice'`).Scan(&version); err != nil || version != 214 {
		t.Fatalf("baseline=%d error=%v", version, err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var content, commandID string
	if err := store.DB().QueryRow(`SELECT content,command_id FROM voice_turns WHERE id='legacy215'`).Scan(&content, &commandID); err != nil || content != "keep voice data" || commandID != "legacy215" {
		t.Fatalf("data not preserved: %v", err)
	}
	assertVoiceLegacyStateRemoved(t, store.DB())
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
