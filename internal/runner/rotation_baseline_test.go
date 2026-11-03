package runner

import (
	"context"
	"reflect"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

type baselineRuntime struct {
	runtimeFake
	initial  []opencode.Message
	replaced [][]opencode.Message
}

func (f *baselineRuntime) Messages(_ context.Context, b RuntimeBinding) ([]opencode.Message, error) {
	if b.SessionID == "session" {
		return f.initial, nil
	}
	index := f.creates - 2
	if index >= 0 && index < len(f.replaced) {
		return f.replaced[index], nil
	}
	return []opencode.Message{{ID: "rotation_idle_succeeded", Type: "idle", Outcome: "succeeded", Time: struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	}{Created: 100, Completed: 101}}}, nil
}

func baselineAssistant(id string, created, usage int64) opencode.Message {
	return opencode.Message{ID: id, Type: "assistant", Time: struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	}{Created: created, Completed: created + 1}, Tokens: opencode.TokenUsage{Input: usage}}
}

func baselineIdle(created int64) opencode.Message {
	return opencode.Message{ID: "rotation_idle_succeeded", Type: "idle", Outcome: "succeeded", Time: struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	}{Created: created, Completed: created + 1}}
}

func baselineEngine(t *testing.T, replacement [][]opencode.Message) (*Engine, *baselineRuntime, *controlFake, contracts.JobClaim) {
	t.Helper()
	f := &baselineRuntime{runtimeFake: runtimeFake{events: make(chan opencode.Event), done: true}, replaced: replacement}
	f.initial = []opencode.Message{baselineAssistant("original", 1, 11), baselineIdle(2)}
	c := &controlFake{}
	e := testEngine(t, &f.runtimeFake, c, openTestJournal(t))
	// Use the overridden Messages method while retaining the standard fake's
	// Create, Prompt, Status, and termination behavior.
	e.Runtime = f
	e.ContextTokenBudget = 10
	e.MaxRotations = 1
	return e, f, c, testClaim()
}

func TestRotationBaselineAboveThresholdTerminatesWithoutFurtherRotation(t *testing.T) {
	e, f, c, claim := baselineEngine(t, [][]opencode.Message{{baselineAssistant("new-earliest", 10, 10), baselineIdle(11)}})
	if err := e.Process(testContext(t), claim); err != nil {
		t.Fatal(err)
	}
	if f.creates != 2 {
		t.Fatalf("creates=%d, want 2", f.creates)
	}
	entry, _, err := e.Journal.Entry(t.Context(), claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.RotationCount != 1 {
		t.Fatalf("rotation_count=%d, want 1", entry.RotationCount)
	}
	if len(c.observations) == 0 || c.observations[len(c.observations)-1].Kind != "runtime_terminated" {
		t.Fatalf("observations=%+v", c.observations)
	}
	if got := c.observations[len(c.observations)-1].Error; got != "context_threshold_below_baseline" {
		t.Fatalf("termination reason=%q", got)
	}
	if c.observations[len(c.observations)-1].RuntimeSessionID != "session-2" {
		t.Fatalf("termination session=%q", c.observations[len(c.observations)-1].RuntimeSessionID)
	}
}

func TestRotationBaselineBelowThresholdRotatesAgainLater(t *testing.T) {
	belowThenAbove := []opencode.Message{baselineAssistant("later-high", 20, 11), baselineAssistant("earliest-low", 10, 1), baselineIdle(21)}
	e, f, c, claim := baselineEngine(t, [][]opencode.Message{belowThenAbove, []opencode.Message{baselineAssistant("final-high", 30, 11), baselineAssistant("final-low", 29, 1), baselineIdle(31)}})
	if err := e.Process(testContext(t), claim); err != nil {
		t.Fatal(err)
	}
	if f.creates != 3 {
		t.Fatalf("creates=%d, want 3", f.creates)
	}
	if len(c.observations) == 0 || c.observations[len(c.observations)-1].Kind != "runtime_terminated" {
		t.Fatalf("observations=%+v", c.observations)
	}
	if got := c.observations[len(c.observations)-1].Error; got != "context_rotation_limit_exceeded" {
		t.Fatalf("termination reason=%q", got)
	}
	entry, _, err := e.Journal.Entry(t.Context(), claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.RotationCount != 1 {
		t.Fatalf("rotation_count=%d, want 1 after limit", entry.RotationCount)
	}
}

func TestRotationBaselineMessageOrderIndependence(t *testing.T) {
	data := []opencode.Message{baselineAssistant("later-high", 20, 11), baselineAssistant("earliest-low", 10, 1), baselineIdle(21)}
	run := func(messages []opencode.Message) (int, string) {
		e, f, c, claim := baselineEngine(t, [][]opencode.Message{messages, []opencode.Message{baselineAssistant("final-high", 30, 11), baselineAssistant("final-low", 29, 1), baselineIdle(31)}})
		if err := e.Process(testContext(t), claim); err != nil {
			t.Fatal(err)
		}
		last := c.observations[len(c.observations)-1]
		return f.creates, last.Error
	}
	first := append([]opencode.Message(nil), data...)
	second := append([]opencode.Message(nil), data...)
	for i, j := 0, len(second)-1; i < j; i, j = i+1, j-1 {
		second[i], second[j] = second[j], second[i]
	}
	gotCreates, gotReason := run(first)
	wantCreates, wantReason := run(second)
	got := [2]any{gotCreates, gotReason}
	want := [2]any{wantCreates, wantReason}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order changed decision: first=%v second=%v", got, want)
	}
}
