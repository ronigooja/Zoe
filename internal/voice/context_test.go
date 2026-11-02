package voice

import "testing"

func TestOpeningContextBoundedAndDeterministic(t *testing.T) {
	a := BuildOpeningContext(OpeningContextInput{RoleRules: "be concise", Tasks: []string{"task:active"}, Summary: "summary", Memory: []string{"fact"}}, 32)
	b := BuildOpeningContext(OpeningContextInput{RoleRules: "be concise", Tasks: []string{"task:active"}, Summary: "summary", Memory: []string{"fact"}}, 32)
	if a != b || len([]rune(a.Text)) > 32 {
		t.Fatalf("context not bounded/deterministic: %#v %#v", a, b)
	}
}

func TestResumedContextIsDeltaAndNewContextIsFull(t *testing.T) {
	full := BuildOpeningContext(OpeningContextInput{Summary: "old"}, 100)
	if got := ContextDelta(full, OpeningContext{}, false, 100); got.Text == "" {
		t.Fatal("new conversation lost full context")
	}
	if got := ContextDelta(full, full, true, 100); got.Text != "" {
		t.Fatal("resumed conversation repeated full context")
	}
}

func TestContextCharsOverBudgetRequiresRotation(t *testing.T) {
	if !RotateRequired(101, 100) || RotateRequired(100, 100) {
		t.Fatal("rotation threshold incorrect")
	}
}
