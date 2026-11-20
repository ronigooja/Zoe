package voice

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestInstructionDefaultExact(t *testing.T) {
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(testPrompts(t).VoiceInstruction()))); got != "1410cb54b83395608c14d5160cb6ea124b589987627350fa5f50e13f97bba47c" {
		t.Fatal("default instruction differs from the approved system prompt")
	}
	for _, capability := range []string{"## Background capabilities", "run tasks", "search the web", "use files", "report task progress and results", "cancel or change tasks", "save and update the user's personal facts and preferences", "look up the user's saved personal records and what was noted from earlier conversations"} {
		if !strings.Contains(testPrompts(t).VoiceInstruction(), capability) {
			t.Errorf("default instruction missing capability %q", capability)
		}
	}
	for _, requirement := range []string{"must", "always", "never", "do not answer", "delegate"} {
		if strings.Contains(strings.ToLower(testPrompts(t).VoiceInstruction()), requirement) {
			t.Errorf("default instruction contains requirement %q", requirement)
		}
	}
}
