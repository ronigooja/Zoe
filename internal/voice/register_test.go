package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

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
	boundary             time.Time
}

func newRegisterFixture(t *testing.T) *registerFixture {
	t.Helper()
	fixtureModel := RegisterModelConfig{BaseURL: "https://unused.invalid", Model: "fixture", APIKey: "fixture"}
	_, store, _ := handoffHTTPServerWithConfig(t, Config{IngestEnabled: true, RegisterConfig: &RegisterConfig{Model: fixtureModel, CompactModel: fixtureModel, CompactCharacters: 1000}})
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
	f.r, err = NewRegister(f.db, RegisterConfig{Model: RegisterModelConfig{BaseURL: server.URL, Model: "route-fixture", APIKey: "fixture"}, CompactModel: RegisterModelConfig{BaseURL: server.URL, Model: "compact-fixture", APIKey: "fixture"}, CompactCharacters: 1000, MemoryBoundary: func(_ context.Context, o RegisterOwner) (time.Time, error) {
		if o == f.owner {
			return f.boundary, nil
		}
		return time.Time{}, nil
	}, MemoryKeys: func(_ context.Context, o RegisterOwner) ([]MemoryMappingKey, error) {
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
	if !s.CompactThrough.Equal(edge) || !s.RawFrom.Equal(s.CompactThrough) || !s.CompactFrom.IsZero() || strings.Contains(s.Raw, "old") || !strings.Contains(s.Raw, "assistant: edge") {
		t.Fatalf("noncontiguous layers: %+v", s)
	}
	if !strings.Contains(f.inputs[0], "user: old") || strings.Contains(f.inputs[0], "assistant: edge") || f.instructions[0] != RegisterCompactInstruction {
		t.Fatal("incorrect compaction instruction/input")
	}
	m := f.now.Add(-2 * time.Hour)
	f.boundary = m
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
	f.output = `{"route":"task","restatement":"It refers to first."}`
	if _, err := f.r.Route(context.Background(), f.owner, "that", f.now); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.inputs[1], "SECRET_") || !strings.Contains(f.inputs[1], "key=trip subject=travel") || f.instructions[1] != RegisterAttributionInstruction {
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

func TestRegisterRouteExactParsing(t *testing.T) {
	f := newRegisterFixture(t)
	for _, tc := range []struct {
		out   string
		valid bool
	}{
		{`{"route":"task","restatement":"none"}`, true},
		{`{"route":"task","restatement":"It refers to the user's trip."}`, true},
		{`{"route":"memory_write","restatement":""}`, true},
		{`{"route":"recall","restatement":""}`, true},
		{`{"route":"chat","restatement":""}`, true},
		{`{"route":"chat","restatement":"none"}`, false},
		{`{"route":"chat","restatement":" "}`, false},
		{`{"route":"chat"}`, false},
		{`{"memory_key":"trip"}`, true},
		{`{"route":"confirm","restatement":"none"}`, false},
		{`{"route":"decline","restatement":"none"}`, false},
		{`{"route":"cancel","restatement":"none"}`, false},
		{`{"route":"progress","restatement":"none"}`, false},
		{`{"route":"task","restatement":""}`, false},
		{`{"route":"task"}`, false},
		{`{"route":"recall"}`, false},
		{`{"route":"memory_write","restatement":"none"}`, false},
		{`{"route":"recall","restatement":"none"}`, false},
		{`{"route":"task","restatement":null}`, false},
		{`{"route":"task","restatement":1}`, false},
		{`{"route":"task","restatement":" none"}`, false},
		{`{"route":"task","restatement":"one\ntwo"}`, false},
		{`{"route":"task","restatement":"- none"}`, false},
		{`{"route":"task","restatement":"none","extra":"x"}`, false},
		{`{"route":"task","route":"recall","restatement":""}`, false},
		{`{"memory_key":"absent"}`, false},
		{`{"memory_key":"travel"}`, false},
		{`{"memory_key":""}`, false},
		{`{"memory_key":null}`, false},
		{`{"memory_key":"trip","route":"task","restatement":"none"}`, false},
		{`{"memory_key":"trip","extra":"x"}`, false},
		{`{"route":"task","restatement":"none"} {}`, false},
		{`{"route":"task","restatement":"none"} extra`, false},
		{`prefix {"route":"task","restatement":"none"}`, false},
		{`[]`, false}, {`null`, false}, {`none`, false}, {`memory trip`, false}, {``, false},
	} {
		t.Run(tc.out, func(t *testing.T) {
			got, key, err := parseRegisterRoute(tc.out, []MemoryMappingKey{{Subject: "travel", Key: "trip"}})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v got=%+v key=%q err=%v", tc.valid, got, key, err)
			}
			if tc.valid && key == "" {
				f.output = tc.out
				result, err := f.r.Route(context.Background(), f.owner, "that", f.now)
				if err != nil || result != got {
					t.Fatalf("route=%+v err=%v", result, err)
				}
			}
		})
	}
}

func TestRegisterAttributionInstructionChat(t *testing.T) {
	for _, required := range []string{
		"Route chat means the sentence is conversation addressed to the assistant that asks for no work and no memory operation: greetings, small talk, thanks, acknowledgements.",
		"Route task means everything else: requests for work, changes or cancellation of work, questions about the state of work.",
		"For memory_write, recall and chat, restatement is empty.",
		`{"route":"memory_write|recall|task|chat","restatement":"<text>"}`,
	} {
		if !strings.Contains(RegisterAttributionInstruction, required) {
			t.Errorf("missing instruction: %s", required)
		}
	}
	if strings.Contains(RegisterAttributionInstruction, "and conversation addressed to the assistant") {
		t.Error("task still includes conversation")
	}
	for _, r := range RegisterAttributionInstruction {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			t.Fatalf("instruction contains CJK character %U", r)
		}
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
		f.boundary = f.now.Add(-90 * time.Minute)
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

func TestRegisterMemoryBoundaryProviderIsolationAndFailure(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "user", "old", f.now.Add(-2*time.Hour))
	f.rebuild(f.now)
	before := f.layers(f.now)
	f.boundary = f.now.Add(-90 * time.Minute)
	other, err := f.r.Layers(context.Background(), RegisterOwner{"default", "other"}, f.now)
	if err != nil || !other.MemoryBoundary.IsZero() || other.Compact != "" || other.Raw != "" {
		t.Fatal("boundary owner leaked", err)
	}
	want := errors.New("memory unavailable")
	f.r.boundary = func(context.Context, RegisterOwner) (time.Time, error) { return time.Time{}, want }
	if _, err = f.r.Route(context.Background(), f.owner, "that", f.now); !errors.Is(err, want) {
		t.Fatal("provider error ignored", err)
	}
	if _, err = f.r.RebuildCompactForOwner(context.Background(), f.owner, f.now); !errors.Is(err, want) {
		t.Fatal("rebuild provider error ignored", err)
	}
	var compact string
	if err = f.db.QueryRow(`SELECT compact FROM voice_context_registers WHERE workspace_id='default' AND owner_subject_id='owner'`).Scan(&compact); err != nil || compact != before.Compact {
		t.Fatal("failed provider lost compact", err)
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

func TestRegisterMigration219PreservesOldRowsAndReplays(t *testing.T) {
	f := newRegisterFixture(t)
	f.turn("handoff-session", 0, "assistant", "old transcript", f.now.Add(-2*time.Hour))
	// Recreate pre-219 shape while preserving real transcript and register rows.
	f.exec(`ALTER TABLE voice_context_registers DROP COLUMN source_memory_boundary;
CREATE TABLE voice_register_memory_boundaries(workspace_id TEXT,owner_subject_id TEXT,boundary_at TEXT);
INSERT INTO voice_register_memory_boundaries VALUES('default','owner','2026-11-12T09:00:00Z');
INSERT INTO voice_context_registers(workspace_id,owner_subject_id,compact,version) VALUES('default','owner','keep compact',7);
DELETE FROM schema_migrations WHERE namespace='voice' AND version>=219`)
	if err := f.store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var text, owner string
	if err := f.db.QueryRow(`SELECT t.text,v.owner_id FROM voice_transcript_turns t JOIN voice_sessions v ON v.id=t.session_id WHERE t.seq=0`).Scan(&text, &owner); err != nil || text != "old transcript" || owner != "owner" {
		t.Fatalf("old rows lost: %q %q %v", text, owner, err)
	}
	var compact, boundary string
	var version int
	if err := f.db.QueryRow(`SELECT compact,version,source_memory_boundary FROM voice_context_registers`).Scan(&compact, &version, &boundary); err != nil || compact != "keep compact" || version != 7 || boundary != "" {
		t.Fatal("migration lost old register", err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='voice_register_memory_boundaries'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("voice M survived", err)
	}
	// Replaying both receipts must tolerate retained tables/columns and drop the recreated M.
	f.exec(`UPDATE voice_context_registers SET source_memory_boundary='2026-11-12T09:00:00Z'; DELETE FROM schema_migrations WHERE namespace='voice' AND version>=218`)
	if err := f.store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT compact,version,source_memory_boundary FROM voice_context_registers`).Scan(&compact, &version, &boundary); err != nil || compact != "keep compact" || version != 7 || boundary != "2026-11-12T09:00:00Z" {
		t.Fatalf("migration replay lost register state: compact=%q version=%d boundary=%q err=%v", compact, version, boundary, err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='voice_register_memory_boundaries'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("replayed voice M survived", err)
	}
}

func TestRegisterRouteMemoryRoundTripAndFailures(t *testing.T) {
	for _, mode := range []string{"success", "repeat", "provider_error", "missing_provider", "unlisted", "subject", "foreign", "model_error", "invalid_second"} {
		t.Run(mode, func(t *testing.T) {
			f := newRegisterFixture(t)
			f.output = `{"memory_key":"trip"}`
			reads := 0
			f.r.content = func(_ context.Context, owner RegisterOwner, key string) (string, error) {
				reads++
				if owner != f.owner || key != "trip" {
					t.Fatal("memory owner/key mismatch")
				}
				if mode == "provider_error" {
					return "", errors.New("unavailable")
				}
				return "saved trip content", nil
			}
			if mode == "missing_provider" {
				f.r.content = nil
			}
			if mode == "unlisted" {
				f.output = `{"memory_key":"absent"}`
			}
			if mode == "subject" {
				f.output = `{"memory_key":"travel"}`
			}
			owner := f.owner
			if mode == "foreign" {
				owner = RegisterOwner{"default", "other"}
			}
			f.onCall = func() {
				if f.calls != 2 {
					return
				}
				if !strings.Contains(f.inputs[1], "saved trip content") || !strings.Contains(f.inputs[1], "Current sentence:\nthat") {
					t.Error("memory follow-up lost context")
				}
				switch mode {
				case "repeat":
				case "model_error":
					f.fail = true
				case "invalid_second":
					f.output = `{"route":"progress","restatement":"none"}`
				default:
					f.output = `{"route":"task","restatement":"It refers to the saved trip."}`
				}
			}
			logs := captureRegisterLogs(t)
			got, err := f.r.Route(context.Background(), owner, "that", f.now)
			for _, input := range f.inputs {
				if mode != "foreign" && !strings.HasPrefix(input, "Memory keys:\nkey=trip subject=travel\n\nCompact:\n") {
					t.Fatalf("missing labelled memory mapping: %q", input)
				}
			}
			if mode == "success" {
				if err != nil || got != (RouteResult{"task", "It refers to the saved trip."}) || reads != 1 || f.calls != 2 {
					t.Fatalf("got=%+v reads=%d calls=%d err=%v", got, reads, f.calls, err)
				}
			} else if err == nil || got != (RouteResult{}) {
				t.Fatal("accepted failure", got, err)
			}
			if f.calls > 2 || reads > 1 {
				t.Fatal("unbounded memory loop")
			}
			if (mode == "unlisted" || mode == "subject" || mode == "foreign") && reads != 0 {
				t.Fatal("unlisted memory fetched")
			}
			if mode == "subject" && (f.calls != 1 || !strings.Contains(logs.String(), "error_kind=output_invalid")) {
				t.Fatalf("subject response was not rejected in round one as output_invalid: calls=%d logs=%s", f.calls, logs.String())
			}
		})
	}
}
