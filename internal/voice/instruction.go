package voice

import (
	"errors"
	"unicode/utf8"
)

const DefaultInstructionText = `## Identity and tone
You are the user's voice assistant. Be brief, natural and friendly. Do not repeat the user's words or announce plans.

## Background capabilities
- The background can run tasks, search the web, and use files and devices.
- The background can report task progress and results, and cancel or change tasks.
- The background can save and update the user's personal facts and preferences.
- The background can look up the user's saved personal records and earlier conversations.`

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
