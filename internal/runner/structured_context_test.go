package runner

import (
	"github.com/ronigooja/Zoe/contracts"
	"strings"
	"testing"
)

func TestStructuredWorkerInitialResumePayloads(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "resume"}[resume], func(t *testing.T) {
			c := testClaim()
			c.Goal = "complete current request"
			c.Instructions = []string{"instruction one", "instruction two"}
			result := "prior authoritative result"
			c.PriorContexts = []contracts.TaskPriorContext{{TaskID: "prior-id", Goal: "prior goal", Status: "completed", Result: &result, Additions: []string{"old first", "old second"}}}
			c.Entries = []contracts.RunEntry{{ID: "addition-one", Content: "new first"}, {ID: "answer-two", Content: "bound answer", Question: &contracts.RunQuestionBinding{QuestionID: "server-question", RunID: "old-run", RequestID: "runtime-question", ActionDigest: "digest", Text: "which option?"}}, {ID: "addition-three", Content: "new last"}}
			if resume {
				c.ResumeQuestion = "legacy question"
				c.ResumeAnswer = "legacy answer"
			}
			f := completingRuntimeFake()
			e := testEngine(t, f, &controlFake{}, openTestJournal(t))
			if err := e.Process(testContext(t), c); err != nil {
				t.Fatal(err)
			}
			if len(f.createdSpecs) != 1 {
				t.Fatalf("creates=%d", len(f.createdSpecs))
			}
			prompt := f.createdSpecs[0].Prompt
			assertInstructionsBeforeGoal(t, prompt, c.Instructions, c.Goal)
			if prompt != f.promptText {
				t.Fatal("Create/Prompt payload mismatch")
			}
			if strings.Count(prompt, c.Goal) != 1 || strings.Contains(prompt, "Do not delegate or perform external irreversible changes.") {
				t.Fatalf("invalid goal/policy: %s", prompt)
			}
			for _, text := range []string{"prior-id", "prior goal", result, "old first", "old second", "server-question", "runtime-question", "digest", "bound answer"} {
				if !strings.Contains(prompt, text) {
					t.Fatalf("lost %q", text)
				}
			}
			if !(strings.Index(prompt, "new first") < strings.Index(prompt, "bound answer") && strings.Index(prompt, "bound answer") < strings.Index(prompt, "new last")) {
				t.Fatal("input order changed")
			}
		})
	}
}

func assertInstructionsBeforeGoal(t *testing.T, prompt string, instructions []string, goal string) {
	t.Helper()
	last := -1
	for _, instruction := range append(append([]string{}, instructions...), "\n\nGoal:\n"+goal) {
		pos := strings.Index(prompt, instruction)
		if pos <= last || strings.Count(prompt, instruction) != 1 {
			t.Fatalf("missing, repeated or out-of-order block %q", instruction)
		}
		last = pos
	}
}

func TestBuildPromptInstructionsBeforeGoal(t *testing.T) {
	c := testClaim()
	c.Instructions = []string{"instruction one", "instruction two"}
	assertInstructionsBeforeGoal(t, buildPrompt(c), c.Instructions, c.Goal)
	e := &Engine{}
	assertInstructionsBeforeGoal(t, e.buildRotationPrompt(testContext(t), c, nil), c.Instructions, c.Goal)
}

func TestStructuredWorkerRotationPayload(t *testing.T) {
	messages := rotationPromptFixture(t)
	c := &rotationContextControlFake{context: contracts.RotationContext{Entries: []contracts.RunEntry{{ID: "entry", Content: strings.Repeat("retained input", 2200)}}, PriorContexts: []contracts.TaskPriorContext{{TaskID: "prior", Goal: "old goal", Status: "completed", Additions: []string{"first", "second"}}}}}
	f := completingRuntimeFake()
	f.messages, f.rotationMessages = messages, rotationReplacement(messages)
	e := testEngine(t, f, &c.controlFake, openTestJournal(t))
	e.Control = c
	e.ContextTokenBudget = 1000
	if err := e.Process(testContext(t), rotationClaim()); err != nil {
		t.Fatal(err)
	}
	prompt := assertRotationSpec(t, f)
	if prompt != f.promptText || !strings.Contains(prompt, c.context.Entries[0].Content) || !strings.Contains(prompt, `"task_id":"prior"`) {
		t.Fatal("rotation lost accepted context")
	}
	if strings.Count(prompt, rotationClaim().Goal) != 1 || strings.Contains(prompt, "Do not delegate or perform external irreversible changes.") {
		t.Fatal("duplicated goal or blanket policy")
	}
}
