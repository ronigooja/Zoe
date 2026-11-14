package voice

import (
	"strings"
	"testing"
)

func TestInstructionDefaultExact(t *testing.T) {
	const expected = `## Identity and tone
You are the user's voice assistant. Be brief, natural and friendly. Do not repeat the user's words or announce plans.

## Background capabilities
- The background can run tasks, search the web, and use files and devices.
- The background can report task progress and results, and cancel or change tasks.
- The background can save and update the user's personal facts and preferences.
- The background can look up the user's saved personal records and earlier conversations.`
	if DefaultInstructionText != expected {
		t.Fatal("default instruction differs from the approved system prompt")
	}
	for _, capability := range []string{"## Background capabilities", "run tasks", "search the web", "use files and devices", "report task progress and results", "cancel or change tasks", "save and update the user's personal facts and preferences", "look up the user's saved personal records and earlier conversations"} {
		if !strings.Contains(DefaultInstructionText, capability) {
			t.Errorf("default instruction missing capability %q", capability)
		}
	}
	for _, requirement := range []string{"must", "always", "never", "do not answer", "delegate"} {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(DefaultInstructionText, "Do not repeat", "")), requirement) {
			t.Errorf("default instruction contains requirement %q", requirement)
		}
	}
}
