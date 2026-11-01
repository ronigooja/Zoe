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
	prompts, eventCalls         int
	calls                       []string
	promptText                  string
	promptBinding               RuntimeBinding
	promptErr                   error
	events                      chan opencode.Event
	done                        bool
	result                      opencode.Message
	messages                    []opencode.Message
}

func (f *runtimeFake) Create(context.Context, RuntimeSpec) (RuntimeBinding, error) {
	f.creates++
	f.calls = append(f.calls, "Create")
	return RuntimeBinding{SessionID: "session"}, nil
}
func (f *runtimeFake) Prompt(_ context.Context, b RuntimeBinding, prompt string) error {
	f.prompts++
	f.calls = append(f.calls, "Prompt")
	f.promptBinding, f.promptText = b, prompt
	return f.promptErr
}
func (f *runtimeFake) Status(context.Context, RuntimeBinding) (map[string]opencode.Status, error) {
	if f.done {
		return map[string]opencode.Status{"session": {Type: "idle"}}, nil
	}
	return map[string]opencode.Status{"session": {Type: "busy"}}, nil
}
func (f *runtimeFake) Messages(context.Context, RuntimeBinding) ([]opencode.Message, error) {
	if f.done {
		if f.messages != nil {
			return f.messages, nil
		}
		return []opencode.Message{f.result}, nil
	}
	return nil, nil
}
func (f *runtimeFake) Events(context.Context, RuntimeBinding) (<-chan opencode.Event, <-chan error) {
	f.eventCalls++
	f.calls = append(f.calls, "Events")
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

// Catches the old newest-first assumption: the terminal idle item is first.
func TestRuntimeProcessNewestFirstMessagesReachesResultAndTermination(t *testing.T) {
	f := completingRuntimeFake()
	f.messages = []opencode.Message{
		{ID: "msg_002", Type: "idle", Outcome: "succeeded", Time: struct {
			Created   int64 `json:"created"`
			Completed int64 `json:"completed"`
		}{Created: 2}},
		{ID: "msg_001", Type: "assistant", Time: struct {
			Created   int64 `json:"created"`
			Completed int64 `json:"completed"`
		}{Created: 1}},
	}
	c := &controlFake{}
	e := testEngine(t, f, c, openTestJournal(t))
	if err := e.Process(testContext(t), testClaim()); err != nil {
		t.Fatal(err)
	}
	if got := kinds(c); got != "dispatch_started,dispatch_bound,result,runtime_terminated" {
		t.Fatalf("observation kinds=%s", got)
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

func testEngine(t *testing.T, f *runtimeFake, c *controlFake, j *Journal) *Engine {
	t.Helper()
	return &Engine{Control: c, Runtime: f, Journal: j, Routes: map[string]Route{"profile": {ProfileID: "profile", Harness: "opencode", IsolationPolicy: OpenCodeWorkspaceWritePolicy, Model: "model"}}, PollInterval: time.Millisecond}
}

func completingRuntimeFake() *runtimeFake {
	f := &runtimeFake{events: make(chan opencode.Event), done: true}
	f.result.Info.Role = "assistant"
	f.result.Info.Time.Completed = 1
	f.result.Info.Finish = "stop"
	return f
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func seedRuntimePhase(t *testing.T, j *Journal, phase string) contracts.JobClaim {
	t.Helper()
	claim := testClaim()
	spec := RuntimeSpec{RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: "1", Prompt: buildPrompt(claim), Workspace: claim.WorkspaceID}
	if _, err := j.SaveClaim(context.Background(), claim, spec, "claimed"); err != nil {
		t.Fatal(err)
	}
	if err := j.SetSession(context.Background(), claim.RunID, claim.AttemptID, claim.Fence, "session", "bound"); err != nil {
		t.Fatal(err)
	}
	if phase != "bound" {
		if err := j.SetPhase(context.Background(), claim.RunID, claim.AttemptID, claim.Fence, phase); err != nil {
			t.Fatal(err)
		}
	}
	return claim
}

// Catches the old missing-Prompt path: a successful dispatch must prompt before monitoring.
func TestRuntimeProcessPromptsBeforeMonitoring(t *testing.T) {
	f := completingRuntimeFake()
	c := &controlFake{}
	j := openTestJournal(t)
	e := testEngine(t, f, c, j)
	claim := testClaim()
	claim.Goal = "do not log this prompt"
	if err := e.Process(testContext(t), claim); err != nil {
		t.Fatal(err)
	}
	wantPrompt := buildPrompt(claim)
	if strings.Join(f.calls, ",") != "Create,Prompt,Events" || f.prompts != 1 || f.promptText != wantPrompt || f.promptBinding.SessionID != "session" {
		t.Fatalf("runtime calls=%v prompts=%d text=%q binding=%+v", f.calls, f.prompts, f.promptText, f.promptBinding)
	}
}

// Catches replay of a failed prompt: the session exists but the prompt outcome is fenced unknown.
func TestRuntimePromptErrorIsUnknownAndNotMonitored(t *testing.T) {
	f := &runtimeFake{events: make(chan opencode.Event), promptErr: errors.New("request failed")}
	c := &controlFake{}
	j := openTestJournal(t)
	e := testEngine(t, f, c, j)
	if err := e.Process(testContext(t), testClaim()); !errors.Is(err, ErrOutcomePending) {
		t.Fatalf("error=%v, want ErrOutcomePending", err)
	}
	if f.prompts != 1 || f.eventCalls != 0 || kinds(c) != "dispatch_started,dispatch_bound,effect_unknown" {
		t.Fatalf("prompts=%d events=%d observations=%s", f.prompts, f.eventCalls, kinds(c))
	}
}

// Catches restart after binding but before the prompt-attempt journal write.
func TestRuntimeRecoveryFromBoundSendsPrompt(t *testing.T) {
	f := completingRuntimeFake()
	c := &controlFake{}
	j := openTestJournal(t)
	e := testEngine(t, f, c, j)
	if err := e.Process(testContext(t), seedRuntimePhase(t, j, "bound")); err != nil {
		t.Fatal(err)
	}
	if f.prompts != 1 || f.eventCalls != 1 || strings.Join(f.calls, ",") != "Prompt,Events" {
		t.Fatalf("runtime calls=%v prompts=%d events=%d", f.calls, f.prompts, f.eventCalls)
	}
}

// Catches unsafe replay after a crash with an in-flight prompt request.
func TestRuntimeRecoveryFromPromptAttemptedDoesNotResend(t *testing.T) {
	f := &runtimeFake{events: make(chan opencode.Event)}
	c := &controlFake{}
	j := openTestJournal(t)
	e := testEngine(t, f, c, j)
	if err := e.Process(testContext(t), seedRuntimePhase(t, j, "prompt_attempted")); !errors.Is(err, ErrOutcomePending) {
		t.Fatalf("error=%v, want ErrOutcomePending", err)
	}
	if f.prompts != 0 || f.eventCalls != 0 {
		t.Fatalf("prompt was replayed or monitoring started: prompts=%d events=%d", f.prompts, f.eventCalls)
	}
}

// Catches unnecessary replay after the prompt was durably acknowledged as sent.
func TestRuntimeRecoveryFromPromptSentOnlyMonitors(t *testing.T) {
	f := completingRuntimeFake()
	c := &controlFake{}
	j := openTestJournal(t)
	e := testEngine(t, f, c, j)
	if err := e.Process(testContext(t), seedRuntimePhase(t, j, "prompt_sent")); err != nil {
		t.Fatal(err)
	}
	if f.prompts != 0 || f.eventCalls != 1 || strings.Join(f.calls, ",") != "Events" {
		t.Fatalf("runtime calls=%v prompts=%d events=%d", f.calls, f.prompts, f.eventCalls)
	}
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
