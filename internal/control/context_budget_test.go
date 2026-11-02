package control

import (
	"github.com/ronigooja/Zoe/contracts"
	"testing"
)

func TestContextBudgetValidation(t *testing.T) {
	if ValidateContextBudgets(0, 1) == nil || ValidateContextBudgets(1, 1) != nil {
		t.Fatal("budget validation incorrect")
	}
}

func TestAgentContextExceededUsesLastAssistantUsage(t *testing.T) {
	usage := []contracts.AssistantTokenUsage{{Output: 100}, {Input: 2, Output: 3}}
	if AgentContextExceeded(usage, 4) == false || AgentContextExceeded(usage, 5) {
		t.Fatal("last assistant token budget incorrect")
	}
}
