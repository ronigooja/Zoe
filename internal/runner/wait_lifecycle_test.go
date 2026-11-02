package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

func TestBuildPromptUsesResumeClaimOnlyWhenQuestionAndAnswerPresent(t *testing.T) {
	plain := testClaim()
	plain.Goal = "goal"
	if got := buildPrompt(plain); strings.Contains(got, "user was asked") {
		t.Fatalf("plain prompt resumed: %q", got)
	}
	resumed := plain
	resumed.ResumeQuestion = "What?"
	resumed.ResumeAnswer = "Do it"
	f := completingRuntimeFake()
	e := testEngine(t, f, &controlFake{}, openTestJournal(t))
	if err := e.Process(context.Background(), resumed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.promptText, "What?") || !strings.Contains(f.promptText, "Do it") {
		t.Fatalf("production resume prompt=%q", f.promptText)
	}
}

func TestWaitingPermissionStopAbortsAndEmitsProof(t *testing.T) {
	f := completingRuntimeFake()
	f.done = false
	f.pendingPermissions = []opencode.PermissionRequest{{ID: "p", Action: "write"}}
	c := &stoppingControl{}
	e := &Engine{Control: c, Runtime: f, Journal: openTestJournal(t), Routes: map[string]Route{"profile": {ProfileID: "profile", Harness: "opencode", IsolationPolicy: OpenCodeWorkspaceWritePolicy, Model: "model"}}, PollInterval: time.Millisecond}
	if err := e.Process(context.Background(), testClaim()); err != nil {
		t.Fatal(err)
	}
	if f.aborts != 1 || f.terminates != 1 || kinds(&c.controlFake) != "dispatch_started,dispatch_bound,stop_confirmed" {
		t.Fatalf("aborts=%d terminates=%d observations=%s", f.aborts, f.terminates, kinds(&c.controlFake))
	}
	if c.observations[len(c.observations)-1].TerminationProof == nil {
		t.Fatal("stop observation lacks termination proof")
	}
}

func TestWaitingRunUsesWaitLimitInsteadOfGenericDeadline(t *testing.T) {
	f := completingRuntimeFake()
	f.done = false
	f.pendingPermissions = []opencode.PermissionRequest{{ID: "p", Action: "write"}}
	c := &controlFake{}
	j := openTestJournal(t)
	claim := testClaim()
	claim.Budget.HardDeadline = time.Now().Add(-time.Hour)
	e := testEngine(t, f, c, j)
	e.WaitLimit = time.Hour
	if err := e.Process(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if f.aborts != 0 || kinds(c) != "dispatch_started,dispatch_bound,permission_requested" {
		t.Fatalf("waiting deadline behavior aborts=%d observations=%s", f.aborts, kinds(c))
	}
}

func TestWaitingJournalRecoveryDoesNotReportPendingActionAgain(t *testing.T) {
	f := completingRuntimeFake()
	f.done = false
	c := &controlFake{}
	j := openTestJournal(t)
	claim := seedRuntimePhase(t, j, "waiting_question")
	e := testEngine(t, f, c, j)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := e.Process(ctx, claim); err == nil {
		t.Fatal("waiting recovery did not remain monitored")
	}
	if f.creates != 0 || f.prompts != 0 || kinds(c) != "" {
		t.Fatalf("recovery replayed action: creates=%d prompts=%d observations=%s", f.creates, f.prompts, kinds(c))
	}
}
