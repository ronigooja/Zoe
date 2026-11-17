package voice

import "github.com/ronigooja/Zoe/internal/prompts"

var defaultPrompt = prompts.Default

// ConfigurePrompts applies only at startup, before workers or requests.
func ConfigurePrompts(p map[string]string) {
	RegisterAttributionInstruction = p["register_attribution"]
	RegisterCompactInstruction = p["register_compact"]
	TaskUIDisplayInstruction = p["task_ui_display"]
	DefaultInstructionText = p["voice_instruction"]
}
