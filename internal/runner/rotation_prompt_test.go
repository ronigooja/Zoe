package runner

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

// This fixture uses the flat OpenCode message shape and tool parts used by the
// production decoder and rotationFiles parser.
//
//go:embed testdata/rotation_messages.json
var rotationMessagesJSON []byte

type rotationContextControlFake struct {
	controlFake
	context contracts.RotationContext
	err     error
	calls   int
}

func (c *rotationContextControlFake) RotationContext(context.Context, string, string, uint64) (contracts.RotationContext, error) {
	c.calls++
	return c.context, c.err
}

func rotationPromptFixture(t *testing.T) []opencode.Message {
	t.Helper()
	var messages []opencode.Message
	if err := json.Unmarshal(rotationMessagesJSON, &messages); err != nil {
		t.Fatal(err)
	}
	return messages
}

func rotationClaim() contracts.JobClaim {
	c := testClaim()
	c.Goal = "finish the rotation goal"
	c.MemoryContext = contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Subject: "claim", Key: "old", Content: "claim-time X"}}}
	return c
}

func rotationReplacement(messages []opencode.Message) []opencode.Message {
	replacement := messages[len(messages)-1]
	replacement.Tokens.Input = 0
	replacement.Tokens.Output = 0
	replacement.Tokens.Cache.Read = 0
	replacement.Tokens.Cache.Write = 0
	replacement.Info.Tokens.Input = 0
	replacement.Info.Tokens.Output = 0
	replacement.Info.Tokens.Cache.Read = 0
	replacement.Info.Tokens.Cache.Write = 0
	return []opencode.Message{replacement}
}

func assertRotationSpec(t *testing.T, f *runtimeFake) string {
	t.Helper()
	if len(f.createdSpecs) != 2 {
		t.Fatalf("created specs=%d, want 2", len(f.createdSpecs))
	}
	if f.createdSpecs[0].Agent != f.createdSpecs[1].Agent || f.createdSpecs[0].Model != f.createdSpecs[1].Model || !reflect.DeepEqual(f.createdSpecs[0].Permissions, f.createdSpecs[1].Permissions) {
		t.Fatalf("replacement spec changed identity or permissions: %+v", f.createdSpecs)
	}
	return f.createdSpecs[1].Prompt
}

func TestRotationPromptUsesRefreshedMemoryAndOrderedInstructions(t *testing.T) {
	messages := rotationPromptFixture(t)
	c := &rotationContextControlFake{context: contracts.RotationContext{Instructions: []string{"instruction a", "instruction b", "instruction c"}, Memory: contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Subject: "fresh", Key: "y", Content: "memory Y"}}}}}
	f := completingRuntimeFake()
	f.messages, f.rotationMessages = messages, rotationReplacement(messages)
	e := testEngine(t, f, &c.controlFake, openTestJournal(t))
	e.Control = c
	e.ContextTokenBudget = 1000
	if err := e.Process(testContext(t), rotationClaim()); err != nil {
		t.Fatal(err)
	}
	prompt := assertRotationSpec(t, f)
	if c.calls != 1 {
		t.Fatalf("RotationContext calls=%d, want 1", c.calls)
	}
	for _, text := range []string{"finish the rotation goal", "memory Y", "internal/runner/engine.go", "internal/runner/rotation_prompt_test.go", "earliest user message", "latest user message"} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("prompt missing %q: %s", text, prompt)
		}
	}
	if strings.Contains(prompt, "claim-time X") {
		t.Fatalf("claim memory leaked: %s", prompt)
	}
	indexes := []int{strings.Index(prompt, "instruction a"), strings.Index(prompt, "instruction b"), strings.Index(prompt, "instruction c")}
	if !(indexes[0] < indexes[1] && indexes[1] < indexes[2]) {
		t.Fatalf("instructions out of order: %v", indexes)
	}
	if strings.Index(prompt, "earliest user message") > strings.Index(prompt, "latest user message") {
		t.Fatal("recent messages are not chronological")
	}
}

func TestRotationPromptOmitsClaimTimeMemoryDeletedSinceClaim(t *testing.T) {
	c := &rotationContextControlFake{context: contracts.RotationContext{}}
	f := completingRuntimeFake()
	messages := rotationPromptFixture(t)
	f.messages, f.rotationMessages = messages, rotationReplacement(messages)
	e := testEngine(t, f, &c.controlFake, openTestJournal(t))
	e.Control = c
	e.ContextTokenBudget = 1000
	if err := e.Process(testContext(t), rotationClaim()); err != nil {
		t.Fatal(err)
	}
	if prompt := assertRotationSpec(t, f); strings.Contains(prompt, "claim-time X") {
		t.Fatalf("deleted claim memory leaked: %s", prompt)
	}
}

func TestRotationPromptWhenContextFetchFails(t *testing.T) {
	messages := rotationPromptFixture(t)
	for _, tc := range []struct {
		name    string
		control RunnerAPI
	}{
		{name: "fetch error", control: &rotationContextControlFake{err: errors.New("context unavailable")}},
		{name: "unsupported control", control: &controlFake{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := completingRuntimeFake()
			f.messages, f.rotationMessages = messages, rotationReplacement(messages)
			e := testEngine(t, f, &controlFake{}, openTestJournal(t))
			e.Control = tc.control
			e.ContextTokenBudget = 1000
			if err := e.Process(testContext(t), rotationClaim()); err != nil {
				t.Fatal(err)
			}
			prompt := assertRotationSpec(t, f)
			if !strings.Contains(prompt, "Zoe memory is temporarily unavailable; do not assume any memory.") || strings.Contains(prompt, "claim-time X") {
				t.Fatalf("memory failure prompt=%q", prompt)
			}
		})
	}
}

func TestRotationPromptIsBounded(t *testing.T) {
	messages := rotationPromptFixture(t)
	for i := range messages {
		messages[i].Text = strings.Repeat("huge message ", 3000)
	}
	c := &rotationContextControlFake{context: contracts.RotationContext{Instructions: []string{"newest instruction"}}}
	f := completingRuntimeFake()
	f.messages, f.rotationMessages = messages, rotationReplacement(messages)
	e := testEngine(t, f, &c.controlFake, openTestJournal(t))
	e.Control = c
	e.ContextTokenBudget = 1000
	if err := e.Process(testContext(t), rotationClaim()); err != nil {
		t.Fatal(err)
	}
	prompt := assertRotationSpec(t, f)
	if len(prompt) > rotationPromptLimit || !strings.Contains(prompt, "finish the rotation goal") || !strings.Contains(prompt, "newest instruction") {
		t.Fatalf("bounded prompt len=%d", len(prompt))
	}
}
