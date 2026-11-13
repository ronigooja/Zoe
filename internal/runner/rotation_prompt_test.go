package runner

import (
	"context"
	_ "embed"
	"encoding/json"
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
	var idle opencode.Message
	idle.ID = "rotation_idle_succeeded"
	idle.Type = "idle"
	idle.Outcome = "succeeded"
	idle.Time.Created = messageCreated(replacement) + 100
	return []opencode.Message{replacement, idle}
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
