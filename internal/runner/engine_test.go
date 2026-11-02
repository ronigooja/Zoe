package runner

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

type runtimeFake struct {
	aborts, creates, terminates int
	prompts, eventCalls         int
	calls                       []string
	promptText                  string
	promptBinding               RuntimeBinding
	createdSpecs                []RuntimeSpec
	live                        map[string]bool
	liveAtBoundary              []string
	createErr                   error
	promptErr                   error
	events                      chan opencode.Event
	done                        bool
	result                      opencode.Message
	messages                    []opencode.Message
	rotationMessages            []opencode.Message
	pendingPermissions          []opencode.PermissionRequest
	pendingForms                []opencode.FormRequest
	formsReplied                []string
	formAnswers                 []map[string]any
	permissionsReplied          []string
	onStatus                    func()
}

func (f *runtimeFake) PendingPermissions(context.Context, RuntimeBinding) ([]opencode.PermissionRequest, error) {
	return f.pendingPermissions, nil
}
func (f *runtimeFake) PendingForms(context.Context, RuntimeBinding) ([]opencode.FormRequest, error) {
	return f.pendingForms, nil
}
func (f *runtimeFake) ReplyForm(_ context.Context, _ RuntimeBinding, formID string, answer map[string]any) error {
	f.formsReplied = append(f.formsReplied, formID)
	f.formAnswers = append(f.formAnswers, answer)
	f.pendingForms = nil
	f.done = true
	return nil
}
func (f *runtimeFake) ReplyPermission(_ context.Context, _ RuntimeBinding, requestID string, _ opencode.PermissionDecision, _ string) error {
	f.permissionsReplied = append(f.permissionsReplied, requestID)
	f.pendingPermissions = nil
	f.done = true
	return nil
}

func (f *runtimeFake) Create(_ context.Context, spec RuntimeSpec) (RuntimeBinding, error) {
	if f.createErr != nil {
		return RuntimeBinding{}, f.createErr
	}
	f.creates++
	f.createdSpecs = append(f.createdSpecs, spec)
	f.calls = append(f.calls, "Create")
	id := "session"
	if f.creates > 1 {
		id += "-" + fmt.Sprint(f.creates)
	}
	if f.live == nil {
		f.live = map[string]bool{}
	}
	f.live[id] = true
	return RuntimeBinding{SessionID: id}, nil
}
func (f *runtimeFake) Prompt(_ context.Context, b RuntimeBinding, prompt string) error {
	f.prompts++
	f.calls = append(f.calls, "Prompt")
	f.promptBinding, f.promptText = b, prompt
	if b.SessionID != "session" {
		f.liveAtBoundary = liveSessions(f)
	}
	return f.promptErr
}
func (f *runtimeFake) Status(context.Context, RuntimeBinding) (map[string]opencode.Status, error) {
	if f.onStatus != nil {
		f.onStatus()
	}
	if f.done {
		return map[string]opencode.Status{"session": {Type: "idle"}}, nil
	}
	return map[string]opencode.Status{"session": {Type: "busy"}}, nil
}
func (f *runtimeFake) Messages(_ context.Context, b RuntimeBinding) ([]opencode.Message, error) {
	if b.SessionID != "session" && f.rotationMessages != nil {
		return f.rotationMessages, nil
	}
	if f.done {
		if f.messages != nil {
			return f.messages, nil
		}
		return []opencode.Message{f.result}, nil
	}
	if f.messages != nil {
		return f.messages, nil
	}
	return nil, nil
}
func (f *runtimeFake) Events(context.Context, RuntimeBinding) (<-chan opencode.Event, <-chan error) {
	f.eventCalls++
	f.calls = append(f.calls, "Events")
	return f.events, make(chan error)
}
func (f *runtimeFake) Abort(context.Context, RuntimeBinding) error { f.aborts++; return nil }
func (f *runtimeFake) Terminate(_ context.Context, b RuntimeBinding) (string, error) {
	f.terminates++
	if f.live != nil {
		f.live[b.SessionID] = false
	}
	return b.SessionID, nil
}
func (f *runtimeFake) FindRotationSession(context.Context, string, string) (RuntimeBinding, error) {
	for id, live := range f.live {
		if live && id != "session" {
			return RuntimeBinding{SessionID: id}, nil
		}
	}
	return RuntimeBinding{}, errors.New("replacement session not found")
}

type controlFake struct {
	observations []contracts.RunnerObservation
	onObserve    func(contracts.RunnerObservation)
}

func (*controlFake) Claim(context.Context) (*contracts.JobClaim, error)    { return nil, ErrNoClaim }
func (*controlFake) Current(context.Context) ([]contracts.JobClaim, error) { return nil, nil }
func (*controlFake) Control(context.Context, string) (contracts.RunControl, error) {
	return contracts.RunControl{}, nil
}
func (c *controlFake) Observe(_ context.Context, _ string, o contracts.RunnerObservation) error {
	c.observations = append(c.observations, o)
	if c.onObserve != nil {
		c.onObserve(o)
	}
	return nil
}
func testClaim() contracts.JobClaim {
	return contracts.JobClaim{RunID: "run", TaskID: "task", AttemptID: "attempt", WorkspaceID: "workspace", ProfileID: "profile", Harness: "opencode", RuntimeRouteID: "runtime", ModelRouteID: "model", IsolationPolicy: OpenCodeWorkspaceWritePolicy, Role: "worker", Agent: "zoe-worker", PermissionTier: "workspace_read_write", PermissionRules: []contracts.PermissionRule{{Action: "*", Resource: "*", Effect: "deny"}, {Action: "read", Resource: "*", Effect: "allow"}}, Fence: 1, Budget: contracts.RunBudgetSnapshot{MaxToolSteps: 1}}
}

func TestValidateClaimFailsClosedWithoutPermissionRuleset(t *testing.T) {
	c := testClaim()
	c.PermissionRules = nil
	if err := validateClaim(c); err == nil {
		t.Fatal("claim without ruleset was accepted")
	}
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
	if err := e.Process(testContext(t), testClaim()); err != nil {
		t.Fatal(err)
	}
	if f.aborts == 0 {
		t.Fatal("tool-step limit did not abort")
	}
}

func assistantUsage(input, output, read, write int64) opencode.Message {
	var m opencode.Message
	m.Info.Role = "assistant"
	m.Info.Time.Created = 1
	m.Info.Time.Completed = 2
	m.Info.Finish = "stop"
	m.Info.Tokens.Input = input
	m.Info.Tokens.Output = output
	m.Info.Tokens.Cache.Read = read
	m.Info.Tokens.Cache.Write = write
	m.Parts = []map[string]any{{"type": "text", "text": "summary from prior session"}}
	return m
}

func TestContextUsageBelowBudgetDoesNotRotate(t *testing.T) {
	f := completingRuntimeFake()
	f.result = assistantUsage(2, 2, 1, 1)
	e := testEngine(t, f, &controlFake{}, openTestJournal(t))
	e.ContextTokenBudget = 10
	if err := e.Process(testContext(t), testClaim()); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 {
		t.Fatalf("creates=%d, want one session", f.creates)
	}
}

func TestContextUsageRotatesWithSameRuntimeSpecAndCompletes(t *testing.T) {
	f := completingRuntimeFake()
	f.live = map[string]bool{"session": true}
	f.result = assistantUsage(8, 2, 1, 1)
	f.rotationMessages = []opencode.Message{assistantUsage(1, 1, 0, 0)}
	c := &controlFake{}
	e := testEngine(t, f, c, openTestJournal(t))
	e.ContextTokenBudget = 10
	claim := testClaim()
	claim.Goal = "finish the important goal"
	if err := e.Process(testContext(t), claim); err != nil {
		t.Fatal(err)
	}
	if f.creates != 2 || f.terminates != 2 {
		t.Fatalf("creates=%d terminates=%d", f.creates, f.terminates)
	}
	if len(f.createdSpecs) != 2 || f.createdSpecs[0].Agent != f.createdSpecs[1].Agent || f.createdSpecs[0].Model != f.createdSpecs[1].Model || f.createdSpecs[0].Workspace != f.createdSpecs[1].Workspace || f.createdSpecs[0].RunID != f.createdSpecs[1].RunID || f.createdSpecs[0].AttemptID != f.createdSpecs[1].AttemptID || f.createdSpecs[0].Fence != f.createdSpecs[1].Fence || !reflect.DeepEqual(f.createdSpecs[0].Permissions, f.createdSpecs[1].Permissions) {
		t.Fatalf("replacement spec widened or changed: %+v", f.createdSpecs)
	}
	if live := f.liveAtBoundary; len(live) != 1 || live[0] != "session-2" {
		t.Fatalf("live sessions=%v, want replacement only", live)
	}
	if !strings.Contains(f.createdSpecs[1].Prompt, "summary from prior session") || !strings.Contains(f.createdSpecs[1].Prompt, claim.Goal) {
		t.Fatalf("seed prompt=%q", f.createdSpecs[1].Prompt)
	}
	if !strings.Contains(kinds(c), "progress") {
		t.Fatalf("rotation observation missing: %s", kinds(c))
	}
}

func liveSessions(f *runtimeFake) []string {
	var out []string
	for id, live := range f.live {
		if live {
			out = append(out, id)
		}
	}
	return out
}

func seedRotation(t *testing.T, j *Journal, phase, summary, old, replacement string) contracts.JobClaim {
	t.Helper()
	c := testClaim()
	spec := RuntimeSpec{RunID: c.RunID, AttemptID: c.AttemptID, Fence: "1", Prompt: buildPrompt(c), Workspace: c.WorkspaceID, Agent: c.Agent, Model: "model", Permissions: c.PermissionRules}
	if _, err := j.SaveClaim(context.Background(), c, spec, "claimed"); err != nil {
		t.Fatal(err)
	}
	if err := j.SetSession(context.Background(), c.RunID, c.AttemptID, c.Fence, old, "prompt_sent"); err != nil {
		t.Fatal(err)
	}
	if err := j.BeginRotation(context.Background(), c.RunID, c.AttemptID, c.Fence, old, summary, 1); err != nil {
		t.Fatal(err)
	}
	if phase != "rotating" {
		if replacement != "" {
			if err := j.SetRotationSession(context.Background(), c.RunID, c.AttemptID, c.Fence, replacement, phase); err != nil {
				t.Fatal(err)
			}
		} else if err := j.SetPhase(context.Background(), c.RunID, c.AttemptID, c.Fence, phase); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestRotationRecoveryFromRotatingSummaryKeepsOneLiveSession(t *testing.T) {
	f := completingRuntimeFake()
	f.creates = 1
	f.live = map[string]bool{"session": true}
	f.rotationMessages = []opencode.Message{assistantUsage(1, 1, 0, 0)}
	j := openTestJournal(t)
	c := seedRotation(t, j, "rotating", "durable summary", "session", "")
	if err := testEngine(t, f, &controlFake{}, j).Process(testContext(t), c); err != nil {
		t.Fatal(err)
	}
	if got := f.liveAtBoundary; len(got) != 1 || got[0] != "session-2" {
		t.Fatalf("live sessions=%v", got)
	}
	e, _, _ := j.Entry(context.Background(), c.RunID)
	if e.RotationSummary != "durable summary" {
		t.Fatalf("summary=%q", e.RotationSummary)
	}
}

func TestRotationRecoveryFromCreateAttemptedMarksUnknownWithoutSecondCreate(t *testing.T) {
	f := completingRuntimeFake()
	f.live = map[string]bool{"session": true}
	j := openTestJournal(t)
	c := seedRotation(t, j, "rotation_create_attempted", "durable summary", "session", "")
	e := testEngine(t, f, &controlFake{}, j)
	if err := e.Process(testContext(t), c); !errors.Is(err, ErrOutcomePending) {
		t.Fatalf("error=%v, want fenced recovery", err)
	}
	if f.creates != 0 {
		t.Fatalf("recovery created a second replacement: %d", f.creates)
	}
	if got := liveSessions(f); len(got) != 1 || got[0] != "session" {
		t.Fatalf("live sessions=%v", got)
	}
}

func TestRotationRecoveryFromCreatedAbortsOldAndCompletes(t *testing.T) {
	f := completingRuntimeFake()
	f.live = map[string]bool{"session": true, "session-2": true}
	f.rotationMessages = []opencode.Message{assistantUsage(1, 1, 0, 0)}
	j := openTestJournal(t)
	c := seedRotation(t, j, "rotation_created", "durable summary", "session", "session-2")
	if err := testEngine(t, f, &controlFake{}, j).Process(testContext(t), c); err != nil {
		t.Fatal(err)
	}
	if got := f.liveAtBoundary; len(got) != 1 || got[0] != "session-2" {
		t.Fatalf("live sessions=%v", got)
	}
}

func TestRotationRecoveryFromPromptSentCompletesWithOneLiveSession(t *testing.T) {
	f := completingRuntimeFake()
	f.live = map[string]bool{"session-2": true}
	f.rotationMessages = []opencode.Message{assistantUsage(1, 1, 0, 0)}
	j := openTestJournal(t)
	c := seedRotation(t, j, "prompt_sent", "durable summary", "session", "session-2")
	initial := liveSessions(f)
	if err := testEngine(t, f, &controlFake{}, j).Process(testContext(t), c); err != nil {
		t.Fatal(err)
	}
	if got := initial; len(got) != 1 || got[0] != "session-2" {
		t.Fatalf("live sessions=%v", got)
	}
}

func TestRotationLimitTerminatesWithProofWithoutCreating(t *testing.T) {
	f := completingRuntimeFake()
	f.live = map[string]bool{"session": true}
	f.result = assistantUsage(20, 0, 0, 0)
	cntl := &controlFake{}
	j := openTestJournal(t)
	c := testClaim()
	c.Budget.MaxContextTokens = 10
	if _, err := j.SaveClaim(context.Background(), c, RuntimeSpec{RunID: c.RunID, AttemptID: c.AttemptID, Fence: "1", Prompt: buildPrompt(c), Workspace: c.WorkspaceID}, "claimed"); err != nil {
		t.Fatal(err)
	}
	if err := j.SetSession(context.Background(), c.RunID, c.AttemptID, c.Fence, "session", "prompt_sent"); err != nil {
		t.Fatal(err)
	}
	if err := j.BeginRotation(context.Background(), c.RunID, c.AttemptID, c.Fence, "session", "already rotated", 1); err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, f, cntl, j)
	e.MaxRotations = 1
	if err := e.monitor(testContext(t), c, RuntimeBinding{SessionID: "session"}); err != nil {
		t.Fatal(err)
	}
	last := cntl.observations[len(cntl.observations)-1]
	if last.TerminationProof == nil || f.creates != 0 {
		t.Fatalf("proof=%+v creates=%d", last.TerminationProof, f.creates)
	}
}

func TestWaitingQuestionOverBudgetDoesNotRotate(t *testing.T) {
	f := completingRuntimeFake()
	f.done = false
	f.live = map[string]bool{"session": true}
	f.messages = []opencode.Message{assistantUsage(20, 0, 0, 0)}
	f.pendingForms = []opencode.FormRequest{{ID: "q", Title: "question"}}
	cntl := &controlFake{}
	reported := make(chan struct{})
	cntl.onObserve = func(o contracts.RunnerObservation) {
		if o.Kind == "worker_question" {
			close(reported)
		}
	}
	e := testEngine(t, f, cntl, openTestJournal(t))
	e.ContextTokenBudget = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-reported; cancel() }()
	if err := e.Process(ctx, testClaim()); err == nil {
		t.Fatal("waiting run completed")
	}
	if f.creates != 1 || len(liveSessions(f)) != 1 {
		t.Fatalf("creates=%d live=%v", f.creates, liveSessions(f))
	}
}

func TestWaitingPermissionOverBudgetDoesNotRotate(t *testing.T) {
	f := completingRuntimeFake()
	f.done = false
	f.live = map[string]bool{"session": true}
	f.messages = []opencode.Message{assistantUsage(20, 0, 0, 0)}
	f.pendingPermissions = []opencode.PermissionRequest{{ID: "p", Action: "write"}}
	cntl := &controlFake{}
	reported := make(chan struct{})
	cntl.onObserve = func(o contracts.RunnerObservation) {
		if o.Kind == "permission_requested" {
			close(reported)
		}
	}
	e := testEngine(t, f, cntl, openTestJournal(t))
	e.ContextTokenBudget = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-reported; cancel() }()
	if err := e.Process(ctx, testClaim()); err == nil {
		t.Fatal("waiting run completed")
	}
	if f.creates != 1 || len(liveSessions(f)) != 1 {
		t.Fatalf("creates=%d live=%v", f.creates, liveSessions(f))
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

func TestPendingPermissionIsReportedWithoutLocalReply(t *testing.T) {
	f := completingRuntimeFake()
	f.done = false
	f.pendingPermissions = []opencode.PermissionRequest{{ID: "p1", Action: "write", Resources: []string{"x"}}}
	c := &controlFake{}
	j := openTestJournal(t)
	e := testEngine(t, f, c, j)
	e.Control = struct {
		*controlFake
		*authorizationFake
	}{c, &authorizationFake{decision: "once", request: "p1"}}
	if err := e.Process(testContext(t), testClaim()); err != nil {
		t.Fatal(err)
	}
	if f.prompts != 1 || len(f.permissionsReplied) != 1 || kinds(c) != "dispatch_started,dispatch_bound,permission_requested,result,runtime_terminated" {
		t.Fatalf("prompts=%d observations=%s", f.prompts, kinds(c))
	}
}

type authorizationFake struct {
	calls    int
	decision string
	request  string
}

func (f *authorizationFake) RuntimeAuthorization(context.Context, string, string, uint64) (*contracts.RuntimeAuthorization, error) {
	f.calls++
	if f.decision == "" || f.calls == 1 {
		return nil, nil
	}
	return &contracts.RuntimeAuthorization{RequestID: f.request, Decision: f.decision, Message: "answer"}, nil
}

func TestWorkerQuestionAnswerUsesSameSessionFormReply(t *testing.T) {
	f := completingRuntimeFake()
	f.done = false
	f.pendingForms = []opencode.FormRequest{{ID: "form-1", Title: "Questions", Fields: []map[string]any{{"key": "real-key", "description": "What?"}}}}
	c := &controlFake{}
	j := openTestJournal(t)
	claim := testClaim()
	e := testEngine(t, f, c, j)
	auth := &authorizationFake{request: "form-1", decision: "answer"}
	e.Control = struct {
		*controlFake
		*authorizationFake
	}{c, auth}
	if err := e.Process(testContext(t), claim); err != nil {
		t.Fatal(err)
	}
	f.result.Info.Role = "assistant"
	f.result.Info.Time.Completed = 1
	f.result.Info.Finish = "stop"
	if len(f.formsReplied) != 1 || f.formsReplied[0] != "form-1" || f.formAnswers[0]["real-key"] != "answer" {
		t.Fatalf("form replies=%v answers=%v", f.formsReplied, f.formAnswers)
	}
}

func TestBuildPromptIsStableForAuthorizationBoundary(t *testing.T) {
	if !strings.Contains(buildPrompt(testClaim()), "assigned goal") {
		t.Fatal("prompt boundary changed")
	}
}
