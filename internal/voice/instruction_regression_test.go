package voice

import (
	"strings"
	"testing"
)

func TestInstructionDescribesOnly(t *testing.T) {
	for _, requirement := range []string{"must", "always", "never", "do not answer", "delegate"} {
		if strings.Contains(strings.ToLower(testPrompts(t).VoiceInstruction()), requirement) {
			t.Errorf("default instruction contains requirement %q", requirement)
		}
	}
}
