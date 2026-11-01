package runner

import (
	"context"
	"errors"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
	"strings"
	"testing"
	"time"
)

type runtimeFake struct {
	aborts, creates, terminates int
	events                      chan opencode.Event
	done                        bool
	result                      opencode.Message
}

func (f *runtimeFake) Create(context.Context, RuntimeSpec) (RuntimeBinding, error) {
	f.creates++
	return RuntimeBinding{SessionID: "session"}, nil
}
func (f *runtimeFake) Prompt(context.Context, RuntimeBinding, string) error { return nil }
func (f *runtimeFake) Status(context.Context, RuntimeBinding) (map[string]opencode.Status, error) {
	if f.done {
		return map[string]opencode.Status{"session": {Type: "idle"}}, nil
	}
	return map[string]opencode.Status{"session": {Type: "busy"}}, nil
}
func (f *runtimeFake) Messages(context.Context, RuntimeBinding) ([]opencode.Message, error) {
	if f.done {
		return []opencode.Message{f.result}, nil
	}
	return nil, nil
}
func (f *runtimeFake) Events(context.Context, RuntimeBinding) (<-chan opencode.Event, <-chan error) {
	return f.events, make(chan error)
}
func (f *runtimeFake) Abort(context.Context, RuntimeBinding) error { f.aborts++; return nil }
func (f *runtimeFake) Terminate(context.Context, RuntimeBinding) (string, error) {
	f.terminates++
	return "session", nil
}

type controlFake struct{ observations []contracts.RunnerObservation }

func (*controlFake) Claim(context.Context) (*contracts.JobClaim, error)    { return nil, ErrNoClaim }
func (*controlFake) Current(context.Context) ([]contracts.JobClaim, error) { return nil, nil }
func (*controlFake) Control(context.Context, string) (contracts.RunControl, error) {
	return contracts.RunControl{}, nil
}
func (c *controlFake) Observe(_ context.Context, _ string, o contracts.RunnerObservation) error {
	c.observations = append(c.observations, o)
	return nil
}
func testClaim() contracts.JobClaim {
	return contracts.JobClaim{RunID: "run", TaskID: "task", AttemptID: "attempt", WorkspaceID: "workspace", ProfileID: "profile", Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model", IsolationPolicy: OpenCodeWorkspaceWritePolicy, Role: "worker", Fence: 1, Budget: contracts.RunBudgetSnapshot{MaxToolSteps: 1}}
}
func TestRuntimeProcessUsesHostSessionInterruptionProof(t *testing.T) {
	f := &runtimeFake{events: make(chan opencode.Event, 1), done: true}
	m := opencode.Message{}
	m.Info.Role = "assistant"
	m.Info.Time.Completed = 1
	m.Info.Finish = "stop"
	f.result = m
	c := &controlFake{}
	j := openTestJournal(t)
	e := &Engine{Control: c, Runtime: f, Journal: j, Routes: map[string]Route{"profile": {ProfileID: "profile", Harness: "opencode", IsolationPolicy: OpenCodeWorkspaceWritePolicy, Model: "model"}}, PollInterval: time.Millisecond}
	if err := e.Process(context.Background(), testClaim()); err != nil {
		t.Fatal(err)
	}
	want := []string{"dispatch_started", "dispatch_bound", "result", "runtime_terminated"}
	var got []string
	for _, o := range c.observations {
		got = append(got, o.Kind)
	}
	if f.creates != 1 || f.terminates != 1 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("runtime lifecycle=%+v observation kinds=%v, want %v", f, got, want)
	}
	last := c.observations[len(c.observations)-1]
	bound := c.observations[1]
	if last.TerminationProof == nil || last.TerminationProof.Method != "session_interrupted" || last.TerminationProof.Source != "runner_host_session" || last.TerminationProof.RuntimeSessionID != "session" || last.TerminationProof.RuntimeContainerID != "" {
		t.Fatalf("termination proof=%+v", last.TerminationProof)
	}
	if bound.RuntimeSessionID != "session" || bound.RuntimeContainerID != "" || len(bound.ExternalEffects) != 1 || bound.ExternalEffects[0].Kind != "runtime_create" || bound.ExternalEffects[0].Status != "created" {
		t.Fatalf("dispatch_bound=%+v", bound)
	}
}
func TestRuntimeAbortOnToolStepLimit(t *testing.T) {
	f := &runtimeFake{events: make(chan opencode.Event, 2)}
	f.events <- opencode.Event{Type: "message.part.updated"}
	f.events <- opencode.Event{Type: "message.part.updated"}
	c := &controlFake{}
	j := openTestJournal(t)
	e := &Engine{Control: c, Runtime: f, Journal: j, Routes: map[string]Route{"profile": {ProfileID: "profile", Harness: "opencode", IsolationPolicy: OpenCodeWorkspaceWritePolicy, Model: "model"}}, PollInterval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = e.Process(ctx, testClaim())
	if f.aborts == 0 {
		t.Fatal("tool-step limit did not abort")
	}
}

type failingCreateRuntime struct {
	runtimeFake
	err error
}

func (f *failingCreateRuntime) Create(context.Context, RuntimeSpec) (RuntimeBinding, error) {
	f.creates++
	return RuntimeBinding{}, f.err
}

func kinds(c *controlFake) string {
	var out []string
	for _, o := range c.observations {
		out = append(out, o.Kind)
	}
	return strings.Join(out, ",")
}

func TestUnknownCreateOutcomeIsFencedAndNeverReplayed(t *testing.T) {
	f := &failingCreateRuntime{err: errors.New("docker timeout")}
	c := &controlFake{}
	j := openTestJournal(t)
	e := &Engine{Control: c, Runtime: f, Journal: j, Routes: map[string]Route{"profile": {ProfileID: "profile", Harness: "opencode", IsolationPolicy: OpenCodeWorkspaceWritePolicy, Model: "model"}}, PollInterval: time.Millisecond}
	if err := e.Process(context.Background(), testClaim()); !errors.Is(err, ErrOutcomePending) {
		t.Fatalf("first process error=%v, want ErrOutcomePending", err)
	}
	if got := kinds(c); got != "dispatch_started,effect_unknown" {
		t.Fatalf("observation kinds=%s", got)
	}
	last := c.observations[len(c.observations)-1]
	if len(last.ExternalEffects) != 1 || last.ExternalEffects[0].Kind != "runtime_create" || last.ExternalEffects[0].Status != "unknown" {
		t.Fatalf("effect_unknown effects=%+v", last.ExternalEffects)
	}
	if err := e.Process(context.Background(), testClaim()); !errors.Is(err, ErrOutcomePending) {
		t.Fatalf("restart process error=%v, want ErrOutcomePending", err)
	}
	if f.creates != 1 {
		t.Fatalf("unknown create was replayed: Create called %d times", f.creates)
	}
}

func TestAllowlistedCreateRejectionIsReportedAndNotReplayed(t *testing.T) {
	f := &failingCreateRuntime{err: RuntimeCreateRejectedError{Code: "session_create_rejected"}}
	c := &controlFake{}
	j := openTestJournal(t)
	e := &Engine{Control: c, Runtime: f, Journal: j, Routes: map[string]Route{"profile": {ProfileID: "profile", Harness: "opencode", IsolationPolicy: OpenCodeWorkspaceWritePolicy, Model: "model"}}, PollInterval: time.Millisecond}
	_ = e.Process(context.Background(), testClaim())
	if got := kinds(c); got != "dispatch_started,create_rejected" {
		t.Fatalf("observation kinds=%s", got)
	}
	if rej := c.observations[len(c.observations)-1].CreateRejection; rej == nil || rej.Code != "session_create_rejected" {
		t.Fatalf("create rejection=%+v", rej)
	}
	_ = e.Process(context.Background(), testClaim())
	if f.creates != 1 {
		t.Fatalf("rejected create was replayed: Create called %d times", f.creates)
	}
}

func TestServerStopAbortsAndConfirmsAfterContainerRemoval(t *testing.T) {
	f := &runtimeFake{events: make(chan opencode.Event)}
	c := &stoppingControl{}
	j := openTestJournal(t)
	e := &Engine{Control: c, Runtime: f, Journal: j, Routes: map[string]Route{"profile": {ProfileID: "profile", Harness: "opencode", IsolationPolicy: OpenCodeWorkspaceWritePolicy, Model: "model"}}, PollInterval: time.Millisecond}
	if err := e.Process(context.Background(), testClaim()); err != nil {
		t.Fatal(err)
	}
	if got := kinds(&c.controlFake); got != "dispatch_started,dispatch_bound,stop_confirmed" {
		t.Fatalf("observation kinds=%s", got)
	}
	if f.aborts != 1 || f.terminates != 1 {
		t.Fatalf("aborts=%d terminates=%d", f.aborts, f.terminates)
	}
}

type stoppingControl struct{ controlFake }

func (*stoppingControl) Control(context.Context, string) (contracts.RunControl, error) {
	return contracts.RunControl{RunID: "run", DesiredControl: "stop"}, nil
}

func openTestJournal(t *testing.T) *Journal {
	t.Helper()
	j, err := OpenJournal(context.Background(), t.TempDir()+"/journal.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func TestToolEvidenceClassifiesCallsAndStaysUnverified(t *testing.T) {
	var m opencode.Message
	m.Info.ID = "msg"
	m.Parts = []map[string]any{
		{"type": "text", "text": "ignored"},
		{"type": "tool", "tool": "bash", "callID": "c1", "state": map[string]any{"status": "completed", "title": "ls"}},
		{"type": "tool", "tool": "edit", "callID": "c2", "state": map[string]any{"status": "error"}},
		{"type": "tool", "tool": "custom_mcp", "callID": "c3"},
		{"type": "tool", "tool": "no_call_id"},
	}
	got := toolEvidence("sess", m)
	if len(got) != 3 {
		t.Fatalf("evidence=%+v, want 3 tool entries", got)
	}
	for i, kind := range []string{"command", "file_change", "mcp_tool"} {
		if got[i].Kind != kind || got[i].Verified || got[i].Source == "" {
			t.Fatalf("evidence[%d]=%+v, want kind %s unverified with source", i, got[i], kind)
		}
	}
}
