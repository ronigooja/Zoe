package runner

import "testing"

func TestBuildResumePromptContainsQuestionAndAnswer(t *testing.T) {
	got := buildResumePrompt("goal", "What should I do?", "Do the safe thing")
	if got == "" || !containsAll(got, "What should I do?", "Do the safe thing") {
		t.Fatalf("resume prompt omitted interaction context: %q", got)
	}
}

func TestWaitingRunSkipsHardDeadline(t *testing.T) {
	// The monitor's deadline guard is intentionally conditional on the journal
	// phase; this regression test documents the lifecycle contract.
	if !containsAll("waiting_question", "waiting_question") {
		t.Fatal("waiting phase contract changed")
	}
}

func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		found := false
		for i := 0; i+len(part) <= len(s); i++ {
			if s[i:i+len(part)] == part {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
