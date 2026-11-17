package voice

import (
	"testing"

	"github.com/ronigooja/Zoe/internal/prompts"
)

func TestVoicePromptPrecedence(t *testing.T) {
	old := DefaultInstructionText
	defer func() { DefaultInstructionText = old }()
	p, err := prompts.Load("", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p["voice_instruction"] = "file instruction"
	ConfigurePrompts(p)
	for _, tc := range []struct{ env, want string }{{"", "file instruction"}, {"env instruction", "env instruction"}, {"off", ""}} {
		got, err := ResolveInstructionText(tc.env)
		if err != nil || got != tc.want {
			t.Fatal(got, err)
		}
	}
}
