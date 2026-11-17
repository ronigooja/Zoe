package voice

import (
	"errors"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/internal/prompts"
)

var DefaultInstructionText = prompts.Default("voice_instruction")

// ResolveInstructionText validates startup configuration; empty uses the default.
func ResolveInstructionText(text string) (string, error) {
	if !utf8.ValidString(text) || len(text) > 6000 {
		return "", errors.New("ZOE_VOICE_INSTRUCTION_TEXT must be valid UTF-8 and at most 6000 bytes")
	}
	if text == "off" {
		return "", nil
	}
	if text == "" {
		return DefaultInstructionText, nil
	}
	return text, nil
}
