package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/prompts"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

type coverageIngest struct{ calls int }

func (c *coverageIngest) IngestChunk(context.Context, string, string, string, string) (int, int, error) {
	c.calls++
	return 1, 0, nil
}

func TestRealMemoryBatchBoundaryRetainsRegisterCoverage(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, register := range []func(*storage.Store) error{storage.RegisterCoreMigrations, voice.RegisterMigrations, memoryapi.RegisterMigrations} {
		if err := register(store); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	stamp := now.Format(time.RFC3339Nano)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.DB().Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES('coverage','default','owner','instance','request',X'01',1,'ended',?,?)`, stamp, stamp)
	turn := func(seq int, text string, at, received time.Time) {
		exec(`INSERT INTO voice_transcript_turns(workspace_id,session_id,seq,role,text,at,received_at) VALUES('default','coverage',?,'user',?,?,?)`, seq, text, at.Format(time.RFC3339Nano), received.Format(time.RFC3339Nano))
	}
	turn(0, "old compact source", now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	turn(1, "retained raw interval", now.Add(-45*time.Minute), now.Add(-40*time.Minute))
	fail := false
	var inputs []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Input string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		inputs = append(inputs, in.Input)
		if fail {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": "retained compact"})
	}))
	defer model.Close()
	repo := NewRepository(store.DB())
	cfg := voice.RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}
	promptSet, err := prompts.Load("../../deploy/prompts")
	if err != nil {
		t.Fatal(err)
	}
	register, err := voice.NewRegister(store.DB(), voice.RegisterConfig{RegisterInstruction: promptSet.Register(), CompactInstruction: promptSet.Compact(), Model: cfg, CompactModel: cfg, CompactCharacters: 1000, MemoryBoundary: func(ctx context.Context, o voice.RegisterOwner) (time.Time, error) {
		return repo.BoundaryAt(ctx, o.WorkspaceID, o.OwnerSubjectID)
	}})
	if err != nil {
		t.Fatal(err)
	}
	owner := voice.RegisterOwner{WorkspaceID: "default", OwnerSubjectID: "owner"}
	if built, err := register.RebuildCompactForOwner(ctx, owner, now); err != nil || !built {
		t.Fatalf("initial build=%v err=%v", built, err)
	}
	before, err := register.Layers(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	ingest := &coverageIngest{}
	if _, err := repo.runBatch(ctx, ingest, "default", "owner", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	boundary, err := repo.BoundaryAt(ctx, "default", "owner")
	if err != nil || !boundary.Equal(now) || ingest.calls == 0 {
		t.Fatalf("boundary=%v calls=%d err=%v", boundary, ingest.calls, err)
	}
	turn(2, "late backdated unconsumed", now.Add(-2*time.Hour), now.Add(time.Minute))
	fail = true
	if _, err := register.RebuildCompactForOwner(ctx, owner, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expected rebuild failure")
	}
	after, err := register.Layers(ctx, owner, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if after.Compact != before.Compact || after.RawFrom != before.RawFrom || !strings.Contains(after.Raw, "retained raw interval") || !strings.Contains(after.Raw, "late backdated unconsumed") {
		t.Fatalf("coverage=%+v", after)
	}
	fail = false
	if built, err := register.RebuildCompactForOwner(ctx, owner, now.Add(2*time.Hour)); err != nil || !built {
		t.Fatalf("rebuild=%v err=%v", built, err)
	}
	input := inputs[len(inputs)-1]
	if !strings.Contains(input, "late backdated unconsumed") || strings.Contains(input, "old compact source") {
		t.Fatalf("membership source=%s", input)
	}
}
