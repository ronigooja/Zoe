package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	_, store, _ := handoffHTTPServerWithConfig(t, Config{IngestEnabled: true, RegisterConfig: &RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: fixtureModel, CompactModel: fixtureModel, CompactCharacters: 1000}})
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
	f.r, err = NewRegister(f.db, RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: RegisterModelConfig{BaseURL: server.URL, Model: "route-fixture", APIKey: "fixture"}, CompactModel: RegisterModelConfig{BaseURL: server.URL, Model: "compact-fixture", APIKey: "fixture"}, CompactCharacters: 1000, MemoryBoundary: func(_ context.Context, o RegisterOwner) (time.Time, error) {
		if o == f.owner {
			return f.boundary, nil
		}
		return time.Time{}, nil
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
	if !strings.Contains(f.inputs[0], "user: old") || strings.Contains(f.inputs[0], "assistant: edge") || f.instructions[0] != testPrompts(t).Compact() {
		t.Fatal("incorrect compaction instruction/input")
	}
	m := f.now.Add(-2 * time.Hour)
	f.boundary = m
	s = f.layers(f.now)
	if !s.CompactFrom.IsZero() || !s.RawFrom.Equal(edge) || s.Compact == "" || strings.Contains(s.Raw, "old") {
		t.Fatalf("memory advancement discarded retained coverage: %+v", s)
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
	f.output = `{"route":"task","request":"It refers to first."}`
	if _, err := f.r.Route(context.Background(), f.owner, "that", f.now); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.inputs[1], "SECRET_") || strings.Contains(f.inputs[1], "Memory keys") || f.instructions[1] != testPrompts(t).Register() {
		t.Fatal("attribution isolation/instruction violated")
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
		{`{"route":"task","request":"Perform the complete request."}`, true},
		{`{"route":"task","request":"It refers to the user's trip."}`, true},
		{`{"route":"memory_write","request":"Save the user's travel preference."}`, true},
		{`{"route":"recall","request":"Retrieve the user's travel preference."}`, true},
		{`{"route":"memory_write","request":""}`, false},
		{`{"route":"recall","request":""}`, false},
		{`{"route":"chat","request":null}`, true},
		{`{"route":"chat","request":"none"}`, false},
		{`{"route":"chat","request":" "}`, false},
		{`{"route":"chat"}`, false},
		{`{"route":"ui","request":"Show the complete requested content."}`, true},
		{`{"route":"task_ui","request":"Show the complete requested content."}`, true},
		{`{"route":"task_ui","request":"Show the complete requested content."}`, true},
		{`{"routes":[],"request":"none"}`, false},
		{`{"routes":null,"request":"none"}`, false},
		{`{"routes":"task","request":"none"}`, false},
		{`{"routes":[1],"request":"none"}`, false},
		{`{"routes":[null],"request":"none"}`, false},
		{`{"routes":["ui","ui"],"request":"none"}`, false},
		{`{"routes":["task","task"],"request":"none"}`, false},
		{`{"routes":["chat","ui"],"request":""}`, false},
		{`{"routes":["chat","task"],"request":""}`, false},
		{`{"routes":["memory_write","ui"],"request":"Store it."}`, false},
		{`{"routes":["recall","ui"],"request":"Retrieve it."}`, false},
		{`{"routes":["memory_write","task"],"request":"Store it."}`, false},
		{`{"routes":["recall","task"],"request":"Retrieve it."}`, false},
		{`{"routes":["memory_write","recall"],"request":"Store it."}`, false},
		{`{"route":"ui","request":""}`, false},
		{`{"route":"task_ui","request":""}`, false},
		{`{"route":"unknown","request":"none"}`, false},
		{`{"route":"task","request":null}`, false},
		{`{"route":"task","request":1}`, false},
		{`{"route":"task","request":"none","extra":"x"}`, false},
		{`{"route":"task","route":"ui","request":"none"}`, false},
		{`{"route":"task","request":"none","request":"none"}`, false},
		{`{"route":"task","request":"none"} {}`, false},
		{`{"route":"task","request":"none"} extra`, false},
		{`{"memory_key":"trip"}`, false},
		{`{"route":"confirm","request":"none"}`, false},
		{`{"route":"decline","request":"none"}`, false},
		{`{"route":"cancel","request":"none"}`, false},
		{`{"route":"progress","request":"none"}`, false},
		{`{"route":"task","request":""}`, false},
		{`{"route":"task"}`, false},
		{`{"route":"recall"}`, false},
		{`{"route":"memory_write","request":"none"}`, false},
		{`{"route":"recall","request":"none"}`, false},
		{`{"route":"task","request":null}`, false},
		{`{"route":"task","request":1}`, false},
		{`{"route":"task","request":" none"}`, false},
		{`{"route":"task","request":"First complete instruction.\nSecond complete instruction."}`, true},
		{`{"route":"task","request":"- Preserve the complete structured request."}`, true},
		{`{"route":"task","request":"none","extra":"x"}`, false},
		{`{"route":"task","request":"none"}`, false},
		{`{"memory_key":"absent"}`, false},
		{`{"memory_key":"travel"}`, false},
		{`{"memory_key":""}`, false},
		{`{"memory_key":null}`, false},
		{`{"memory_key":"trip","route":"task","request":"none"}`, false},
		{`{"memory_key":"trip","extra":"x"}`, false},
		{`{"route":"task","request":"none"} {}`, false},
		{`{"route":"task","request":"none"} extra`, false},
		{`prefix {"route":"task","request":"none"}`, false},
		{`[]`, false}, {`null`, false}, {`none`, false}, {`memory trip`, false}, {``, false},
	} {
		t.Run(tc.out, func(t *testing.T) {
			got, err := parseRegisterRoute(tc.out)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v got=%+v err=%v", tc.valid, got, err)
			}
			if tc.valid {
				f.output = tc.out
				result, err := f.r.Route(context.Background(), f.owner, "that", f.now)
				if err != nil || !reflect.DeepEqual(result, got) {
					t.Fatalf("route=%+v err=%v", result, err)
				}
			}
		})
	}
}

func TestRegisterAttributionInstructionChat(t *testing.T) {
	for _, required := range []string{
		"chat: the user wants conversation only.",
		"task: the user wants work performed, modified, answered, continued, inspected or cancelled.",
		"task_ui: the user wants content that needs lookup or work first, or is an already finished task's result, to be presented to them.",
		"ui: the user wants content presented that is composed from their own words or on the spot, and needs no lookup or work.",
		"chat_input: the user wants to type text for the voice conversation.",
		"For chat, request is null.",
		"For every other route, request is a nonempty, complete, self-contained statement",
		"Never output the literal none",
		"only route and request",
	} {
		if !strings.Contains(testPrompts(t).Register(), required) {
			t.Errorf("missing instruction: %s", required)
		}
	}
	if strings.Contains(testPrompts(t).Register(), "and conversation addressed to the assistant") {
		t.Error("task still includes conversation")
	}
	for _, forbidden := range []string{"memory_key", "memory keys", "Memory keys", "memory content", "For example", "e.g.", "Example:"} {
		if strings.Contains(testPrompts(t).Register(), forbidden) {
			t.Errorf("obsolete instruction: %s", forbidden)
		}
	}
	for _, r := range testPrompts(t).Register() {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			t.Fatalf("instruction contains CJK character %U", r)
		}
	}
}

func TestRegisterRequestTextValidation(t *testing.T) {
	for _, route := range []string{"task", "memory_write", "recall", "ui"} {
		for _, text := range []string{"", "none", " NONE ", "  "} {
			output, err := json.Marshal(map[string]any{"route": route, "request": text})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseRegisterRoute(string(output)); err == nil {
				t.Errorf("accepted route=%s request=%q", route, text)
			}
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

func TestRegisterRouteSingleCall(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		valid        bool
	}{
		{"task", `{"route":"task","request":"Perform the complete request."}`, true},
		{"memory_write", `{"route":"memory_write","request":"Save the user's travel preference."}`, true},
		{"recall", `{"route":"recall","request":"Retrieve the user's travel preference."}`, true},
		{"chat", `{"route":"chat","request":null}`, true},
		{"empty_memory_write", `{"route":"memory_write","request":""}`, false},
		{"empty_recall", `{"route":"recall","request":""}`, false},
		{"nonempty_chat", `{"route":"chat","request":"Hello."}`, false},
		{"old_memory_key", `{"memory_key":"x"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRegisterFixture(t)
			f.output = tc.output
			logs := captureRegisterLogs(t)
			got, err := f.r.Route(context.Background(), f.owner, "that", f.now)
			if (err == nil) != tc.valid || (!tc.valid && !reflect.DeepEqual(got, RouteResult{})) {
				t.Fatalf("got=%+v err=%v", got, err)
			}
			if f.calls != 1 {
				t.Fatalf("calls=%d, want exactly one", f.calls)
			}
			if f.inputs[0] != "Compact:\n\nRaw:\n\nCurrent sentence:\nthat" || strings.Contains(f.inputs[0], "Memory keys") {
				t.Fatalf("unexpected input: %q", f.inputs[0])
			}
			if !tc.valid && !strings.Contains(logs.String(), "error_kind=output_invalid") {
				t.Fatalf("missing invalid-output classification: %s", logs.String())
			}
		})
	}
}
