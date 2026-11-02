package control

import (
	"errors"
	"strings"

	"github.com/ronigooja/Zoe/contracts"
)

func ValidateContextBudgets(agentTokens, voiceChars int) error {
	if agentTokens < 1 || agentTokens > 10_000_000 {
		return errors.New("agent context budget must be between 1 and 10000000 tokens")
	}
	if voiceChars < 1 || voiceChars > 10_000_000 {
		return errors.New("voice context budget must be between 1 and 10000000 characters")
	}
	return nil
}

func AgentContextExceeded(messages []contracts.AssistantTokenUsage, budget int) bool {
	if budget < 1 || len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	return last.Input+last.Output+last.Reasoning+last.CacheRead+last.CacheWrite > int64(budget)
}

func RenderMemoryContext(c contracts.MemoryContext, maxChars int) string {
	if maxChars < 1 || strings.TrimSpace(c.Text) == "" {
		return ""
	}
	text := strings.TrimSpace(c.Text)
	if len([]rune(text)) > maxChars {
		text = string([]rune(text)[:maxChars])
	}
	return text
}
