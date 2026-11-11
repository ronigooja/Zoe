package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/storage"
)

type registerFixture struct {
	t                    *testing.T
	r                    *ContextRegister
	db                   *sql.DB
	store                *storage.Store
	now                  time.Time
	owner                RegisterOwner
	output               string
	fail                 bool
	calls                int
	inputs, instructions []string
	onCall               func()
}

func newRegisterFixture(t *testing.T) *registerFixture {
	t.Helper()
	_, store, _ := transcriptHTTPServer(t)
	f := &registerFixture{t: t, db: store.DB(), store: store, now: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC), owner: RegisterOwner{"default", "owner"}, output: "compact record"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Instructions, Input string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		f.calls++
		f.inputs = append(f.inputs, request.Input)
		f.instructions = append(f.instructions, request.Instructions)
		if f.onCall != nil {
			f.onCall()
		}
		if f.fail {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": f.output})
	}))
	t.Cleanup(server.Close)
	var err error
	f.r, err = NewRegister(f.db, RegisterConfig{Model: RegisterModelConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"}, CompactCharacters: 1000, MemoryKeys: func(_ context.Context, o RegisterOwner) ([]MemoryMappingKey, error) {
		if o != f.owner {
			return nil, nil
		}
		return []MemoryMappingKey{{Subject: "travel", Key: "trip"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *registerFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *registerFixture) session(id, workspace, owner string) {
	f.t.Helper()
	f.exec(`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,generation,state,created_at,updated_at) VALUES(?,?,?,'fixture',?,X'01',1,'ended',?,?)`, id, workspace, owner, id, f.now.Format(time.RFC3339Nano), f.now.Format(time.RFC3339Nano))
}
func (f *registerFixture) turn(session string, seq int, role, text string, at time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO voice_transcript_turns(workspace_id,session_id,seq,role,text,at,received_at) SELECT workspace_id,id,?,?,?,?,? FROM voice_sessions WHERE id=?`, seq, role, text, at.Format(time.RFC3339Nano), f.now.Format(time.RFC3339Nano), session)
}
func (f *registerFixture) layers(now time.Time) RegisterLayers {
	f.t.Helper()
	s, err := f.r.Layers(context.Background(), f.owner, now)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f *registerFixture) rebuild(now time.Time) bool {
	f.t.Helper()
	ok, err := f.r.RebuildCompactForOwner(context.Background(), f.owner, now)
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func TestRegisterContiguousLayersAndMemoryBoundary(t *testing.T) {
	f := newRegisterFixture(t)
	ctx := context.Background()
	edge := f.now.Add(-time.Hour)
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-3*time.Hour))
	f.turn("handoff-session", 1, "assistant", "edge", edge)
	f.turn("handoff-session", 2, "user", "recent", f.now.Add(-time.Minute))
	if !f.rebuild(f.now) {
		t.Fatal("no rebuild")
	}
	s := f.layers(f.now)
	if !s.CompactThrough.Equal(edge) || !s.RawFrom.Equal(s.CompactThrough) || s.CompactFrom.Unix() != 0 || strings.Contains(s.Raw, "old") || !strings.Contains(s.Raw, "assistant: edge") {
		t.Fatalf("noncontiguous layers: %+v", s)
	}
	if !strings.Contains(f.inputs[0], "user: old") || strings.Contains(f.inputs[0], "assistant: edge") || f.instructions[0] != RegisterCompactInstruction {
		t.Fatal("incorrect compaction instruction/input")
	}
	m := f.now.Add(-2 * time.Hour)
	if err := f.r.SetMemoryBoundary(ctx, f.owner, m); err != nil {
		t.Fatal(err)
	}
	s = f.layers(f.now)
	if !s.CompactFrom.Equal(m) || !s.RawFrom.Equal(m) || s.Compact != "" || strings.Contains(s.Raw, "old") {
		t.Fatalf("memory did not move left edge: %+v", s)
	}
	f.turn("handoff-session", 3, "user", "between", f.now.Add(-90*time.Minute))
	if !f.rebuild(f.now) {
		t.Fatal("new M source not rebuilt")
	}
	s = f.layers(f.now)
	if !s.CompactFrom.Equal(m) || !s.RawFrom.Equal(edge) || strings.Contains(f.inputs[1], "user: old") || !strings.Contains(f.inputs[1], "user: between") {
		t.Fatal("rebuild ignored M")
	}
	text, err := f.r.AssembleContext(ctx, f.owner, f.now, 15)
	if err != nil || len([]rune(text)) != 15 {
		t.Fatalf("budget: %q %v", text, err)
	}
}

func TestRegisterCrossCallStableOrderingAndOwnerIsolation(t *testing.T) {
	f := newRegisterFixture(t)
	at := f.now.Add(-2 * time.Hour)
	f.session("a", "default", "owner")
	f.session("z", "default", "owner")
	f.session("foreign", "default", "other")
	f.exec(`INSERT INTO workspaces(id,name,created_at) SELECT 'elsewhere',name,created_at FROM workspaces WHERE id='default'`)
	f.session("foreign-workspace", "elsewhere", "owner")
	f.turn("z", 0, "assistant", "third", at)
	f.turn("a", 4, "assistant", "second", at)
	f.turn("a", 2, "user", "first", at)
	f.turn("foreign", 0, "user", "SECRET_OTHER_OWNER", at)
	f.turn("foreign-workspace", 0, "user", "SECRET_OTHER_WORKSPACE", at)
	f.rebuild(f.now)
	input := f.inputs[0]
	if strings.Index(input, "user: first") > strings.Index(input, "assistant: second") || strings.Index(input, "assistant: second") > strings.Index(input, "assistant: third") || strings.Contains(input, "SECRET_") {
		t.Fatal("source order/isolation violated")
	}
	f.output = "It refers to first."
	if _, err := f.r.Restate(context.Background(), f.owner, "that", f.now); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.inputs[1], "SECRET_") || !strings.Contains(f.inputs[1], "travel: trip") || f.instructions[1] != RegisterAttributionInstruction {
		t.Fatal("attribution isolation/instruction violated")
	}
	keys, err := f.r.MemoryKeys(context.Background(), RegisterOwner{"default", "other"})
	if err != nil || len(keys) != 0 {
		t.Fatal("mapping owner leaked")
	}
}

func TestRegisterNoRebuildWithoutNewConversation(t *testing.T) {
	f := newRegisterFixture(t)
	if f.rebuild(f.now) || f.calls != 0 {
		t.Fatal("empty source called model")
	}
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-2*time.Hour))
	f.rebuild(f.now)
	before := f.layers(f.now)
	if f.rebuild(f.now.Add(time.Hour)) || f.calls != 1 {
		t.Fatal("unchanged source called model")
	}
	after := f.layers(f.now.Add(time.Hour))
	if before != after {
		t.Fatal("unchanged source advanced metadata")
	}
	// Late turns are detected by source fingerprint, even behind the old edge.
	f.turn("handoff-session", 1, "assistant", "late", f.now.Add(-3*time.Hour))
	if !f.rebuild(f.now.Add(time.Hour)) || f.calls != 2 || !strings.Contains(f.inputs[1], "late") {
		t.Fatal("late source ignored")
	}
}

func TestRegisterFailedRebuildPreservesCoverageAndSuccessAtomic(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-2*time.Hour))
	f.rebuild(f.now)
	before := f.layers(f.now)
	f.turn("handoff-session", 1, "assistant", "new", f.now.Add(-30*time.Minute))
	later := f.now.Add(time.Hour)
	f.fail = true
	if _, err := f.r.RebuildCompactForOwner(context.Background(), f.owner, later); err == nil {
		t.Fatal("model error accepted")
	}
	failed := f.layers(later)
	if failed.Compact != before.Compact || failed.CompactThrough != before.CompactThrough || failed.BuiltAt != before.BuiltAt || failed.Version != before.Version || !strings.Contains(failed.Raw, "assistant: new") {
		t.Fatal("failed rebuild lost coverage or advanced state")
	}
	f.fail = false
	f.output = "rebuilt from source"
	// A trigger rejects any write that does not advance all coverage metadata with the new compact.
	f.exec(`CREATE TRIGGER register_atomic BEFORE UPDATE OF compact ON voice_context_registers WHEN NEW.compact<>OLD.compact AND (NEW.version<>OLD.version+1 OR NEW.built_at=OLD.built_at OR NEW.compact_through=OLD.compact_through) BEGIN SELECT RAISE(ABORT,'non-atomic register update'); END`)
	if !f.rebuild(later) {
		t.Fatal("success not committed")
	}
	after := f.layers(later)
	if after.Compact != f.output || !after.CompactThrough.Equal(later.Add(-time.Hour)) || !after.BuiltAt.Equal(later) || after.Version != before.Version+1 || after.Raw != "" {
		t.Fatalf("wrong success state: %+v", after)
	}
	if !strings.Contains(f.inputs[2], "user: old") || !strings.Contains(f.inputs[2], "assistant: new") || strings.Contains(f.inputs[2], before.Compact) {
		t.Fatal("incremental instead of source rebuild")
	}
}

func TestRegisterRestateExactParsing(t *testing.T) {
	f := newRegisterFixture(t)
	for _, tc := range []struct {
		out   string
		valid bool
	}{{"none", true}, {"memory trip", true}, {"It refers to the user's trip.", true}, {"", false}, {" none", false}, {"none\n", false}, {"memory absent", false}, {"memory", false}, {"memory trip extra", false}, {"{\"text\":\"none\"}", false}, {"```none```", false}, {"one\ntwo", false}, {"- none", false}} {
		t.Run(tc.out, func(t *testing.T) {
			f.output = tc.out
			got, err := f.r.Restate(context.Background(), f.owner, "that", f.now)
			if (err == nil) != tc.valid || tc.valid && string(got) != tc.out {
				t.Fatalf("valid=%v got=%q err=%v", tc.valid, got, err)
			}
		})
	}
}

func TestRegisterDurableClaimAndMemoryFence(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-2*time.Hour))
	f.exec(`INSERT INTO voice_context_registers(workspace_id,owner_subject_id,claim_token,lease_until_ms) VALUES('default','owner','another',?)`, f.now.Add(time.Minute).UnixMilli())
	if f.rebuild(f.now) || f.calls != 0 {
		t.Fatal("live claim stolen")
	}
	f.onCall = func() {
		if err := f.r.SetMemoryBoundary(context.Background(), f.owner, f.now.Add(-90*time.Minute)); err != nil {
			t.Error(err)
		}
	}
	if f.rebuild(f.now.Add(2 * time.Minute)) {
		t.Fatal("stale claimant committed after M moved")
	}
	s := f.layers(f.now)
	if s.Compact != "" || !s.CompactThrough.Equal(s.MemoryBoundary) {
		t.Fatal("M fence lost")
	}
	f.r.Start(context.Background())
	f.r.Stop()
	f.r.Stop()
}

func TestRegisterSourceArrivalFencesInFlightBuild(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-2*time.Hour))
	f.onCall = func() { f.turn("handoff-session", 1, "assistant", "arrived during model", f.now.Add(-90*time.Minute)) }
	if f.rebuild(f.now) {
		t.Fatal("build committed an incomplete source snapshot")
	}
	s := f.layers(f.now)
	if s.Version != 0 || s.Compact != "" || !strings.Contains(s.Raw, "arrived during model") {
		t.Fatal("source arrival left a gap")
	}
	f.onCall = nil
	if !f.rebuild(f.now) || !strings.Contains(f.inputs[1], "arrived during model") {
		t.Fatal("retry did not rebuild entire source")
	}
}

func TestRegisterMemoryBoundaryTransactionRollback(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-2*time.Hour))
	f.rebuild(f.now)
	before := f.layers(f.now)
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = SetRegisterMemoryBoundaryTx(context.Background(), tx, f.owner, f.now.Add(-90*time.Minute)); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if after := f.layers(f.now); after != before {
		t.Fatal("failed memory batch advanced boundary")
	}
}

func TestRegisterRejectsOversizedCompact(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-2*time.Hour))
	f.output = strings.Repeat("界", 1001)
	if _, err := f.r.RebuildCompactForOwner(context.Background(), f.owner, f.now); err == nil {
		t.Fatal("oversized compact accepted")
	}
	if s := f.layers(f.now); s.Version != 0 || s.Compact != "" || !strings.Contains(s.Raw, "user: old") {
		t.Fatal("invalid model output discarded source")
	}
}

func TestRegisterMigration218PreservesOldRows(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "assistant", "old transcript", f.now.Add(-2*time.Hour))
	// Recreate the exact pre-218 shape on a real migrated fixture, retaining existing voice data.
	f.exec(`DROP TABLE voice_context_registers; DROP TABLE voice_register_memory_boundaries; DELETE FROM schema_migrations WHERE version=218`)
	if err := f.store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var text, owner string
	if err := f.db.QueryRow(`SELECT t.text,v.owner_id FROM voice_transcript_turns t JOIN voice_sessions v ON v.id=t.session_id WHERE t.seq=0`).Scan(&text, &owner); err != nil || text != "old transcript" || owner != "owner" {
		t.Fatalf("old rows lost: %q %q %v", text, owner, err)
	}
	f.exec(`INSERT INTO voice_context_registers(workspace_id,owner_subject_id) VALUES('default','owner')`)
	var compact, through, built string
	var version int
	if err := f.db.QueryRow(`SELECT compact,compact_through,built_at,version FROM voice_context_registers`).Scan(&compact, &through, &built, &version); err != nil || compact != "" || through != registerEpoch || built != registerEpoch || version != 0 {
		t.Fatal("migration defaults invalid", err)
	}
}
