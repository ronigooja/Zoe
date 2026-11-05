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

func TestTranscriptAndSummaryTablesAreIncludedInExport(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.DB().Exec(`CREATE TABLE voice_sessions(id TEXT,workspace_id TEXT,owner_id TEXT);
CREATE TABLE voice_transcript_turns(workspace_id TEXT,session_id TEXT,seq INTEGER,role TEXT,text TEXT,at TEXT,received_at TEXT);
CREATE TABLE voice_call_summaries(id TEXT,workspace_id TEXT,session_id TEXT,topic TEXT,started_at TEXT,ended_at TEXT,first_seq INTEGER,last_seq INTEGER,summary TEXT,created_at TEXT,model_call_id TEXT);
INSERT INTO voice_sessions VALUES('mine','default','owner'),('other','default','other'),('foreign','foreign','owner');
INSERT INTO voice_transcript_turns VALUES('default','mine',1,'user','  exact text  ','at','received'),('default','other',1,'assistant','private other','at','received'),('foreign','foreign',1,'user','foreign private','at','received');
INSERT INTO voice_call_summaries VALUES('summary','default','mine','topic','start','end',1,1,'summary text','created',NULL),('private','default','other','topic','start','end',1,1,'private summary','created',NULL);`)
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
	if len(data["voice_transcript_turns"]) != 1 || data["voice_transcript_turns"][0]["text"] != "  exact text  " || len(data["voice_call_summaries"]) != 1 || counts.VoiceTranscripts != 1 || counts.VoiceSummaries != 1 {
		t.Fatalf("export=%s counts=%+v", out.String(), counts)
	}
	manifest := buildExportManifest("export", p, "workspace", 0, "lineage", 1, 0, counts, 0, "")
	inventory := manifest["content"].(map[string]any)["inventory"].([]string)
	for _, table := range []string{"voice_transcript_turns", "voice_call_summaries"} {
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
	if _, err := tx.Exec(`UPDATE voice_call_summaries SET summary=? WHERE id='summary'`, strings.Repeat("x", int(maxScopedExportFieldBytes)+1)); err != nil {
		t.Fatal(err)
	}
	if err := validateVoiceTranscriptExportBounds(ctx, tx, p); err != errExportFieldTooLarge {
		t.Fatalf("oversize summary err=%v", err)
	}
}
