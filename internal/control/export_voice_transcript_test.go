package control

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestTranscriptTableIsIncludedInExport(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.DB().Exec(`CREATE TABLE voice_sessions(id TEXT,workspace_id TEXT,owner_id TEXT);
CREATE TABLE voice_transcript_turns(workspace_id TEXT,session_id TEXT,seq INTEGER,role TEXT,text TEXT,at TEXT,received_at TEXT);
INSERT INTO voice_sessions VALUES('mine','default','owner'),('other','default','other'),('foreign','foreign','owner');
INSERT INTO voice_transcript_turns VALUES('default','mine',1,'user','  exact text  ','at','received'),('default','other',1,'assistant','private other','at','received'),('foreign','foreign',1,'user','foreign private','at','received');`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "owner"}
	var out bytes.Buffer
	counts := exportCounter{}
	out.WriteString(`{"existing":[]`)
	if err := writeVoiceTranscriptExport(ctx, tx, &out, p, &counts); err != nil {
		t.Fatal(err)
	}
	out.WriteString(`}`)
	var data map[string][]map[string]any
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data["voice_transcript_turns"]) != 1 || data["voice_transcript_turns"][0]["text"] != "  exact text  " || counts.VoiceTranscripts != 1 {
		t.Fatalf("export=%s counts=%+v", out.String(), counts)
	}
	if _, exists := data["voice_call_summaries"]; exists {
		t.Fatal("retired summaries still exported")
	}
	manifest := buildExportManifest("export", p, "workspace", 0, "lineage", 1, 0, counts, 0, "")
	inventory := manifest["content"].(map[string]any)["inventory"].([]string)
	for _, name := range inventory {
		if name == "voice_call_summaries" || name == "voice_summary_jobs" {
			t.Fatal("retired summary inventory")
		}
	}
	for _, table := range []string{"voice_transcript_turns"} {
		found := false
		for _, name := range inventory {
			found = found || name == table
		}
		if !found {
			t.Fatalf("export inventory missing %s", table)
		}
	}
	if err := validateVoiceTranscriptExportBounds(ctx, tx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE voice_transcript_turns SET text=? WHERE session_id='mine'`, strings.Repeat("x", int(maxScopedExportFieldBytes)+1)); err != nil {
		t.Fatal(err)
	}
	if err := validateVoiceTranscriptExportBounds(ctx, tx, p); err != errExportFieldTooLarge {
		t.Fatalf("oversize transcript err=%v", err)
	}
}
