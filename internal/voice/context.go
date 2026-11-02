package voice

import "strings"

type OpeningContextInput struct {
	RoleRules string
	Tasks     []string
	Summary   string
	Memory    []string
}

type OpeningContext struct {
	Text    string
	Version int64
}

func BuildOpeningContext(in OpeningContextInput, maxChars int) OpeningContext {
	if maxChars < 1 {
		return OpeningContext{}
	}
	parts := []string{}
	if strings.TrimSpace(in.RoleRules) != "" {
		parts = append(parts, "Role rules:\n"+strings.TrimSpace(in.RoleRules))
	}
	if len(in.Tasks) > 0 {
		parts = append(parts, "Active tasks:\n- "+strings.Join(in.Tasks, "\n- "))
	}
	if strings.TrimSpace(in.Summary) != "" {
		parts = append(parts, "Recent conversation summary:\n"+strings.TrimSpace(in.Summary))
	}
	if len(in.Memory) > 0 {
		parts = append(parts, "Memory:\n- "+strings.Join(in.Memory, "\n- "))
	}
	text := strings.Join(parts, "\n\n")
	runes := []rune(text)
	if len(runes) > maxChars {
		text = string(runes[:maxChars])
	}
	return OpeningContext{Text: text, Version: 1}
}

func ContextDelta(full, previous OpeningContext, resumed bool, maxChars int) OpeningContext {
	if !resumed {
		return BuildOpeningContext(OpeningContextInput{Summary: full.Text}, maxChars)
	}
	if full.Text == previous.Text {
		return OpeningContext{Version: previous.Version}
	}
	return OpeningContext{Text: full.Text, Version: previous.Version + 1}
}
