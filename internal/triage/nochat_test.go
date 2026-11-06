package triage

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestDelegationPromptFiveDefinitionsOnly(t *testing.T) {
	if strings.Contains(strings.ToLower(instructions), "chat") {
		t.Error("delegation prompt still defines chat")
	}
	lines := strings.Split(instructions, "\n")
	if len(lines) != 7 {
		t.Fatalf("definitions-only prompt must have introduction, five definitions, and output rule; got %d lines", len(lines))
	}
	for i, category := range []string{"progress", "recall", "memory_write", "memory_read", "task"} {
		if !strings.HasPrefix(lines[i+1], category+": ") {
			t.Errorf("definition %d = %q", i, lines[i+1])
		}
	}
	for _, marker := range []string{"example", "e.g.", "for instance", "\"", "“", "”"} {
		if strings.Contains(strings.ToLower(instructions), marker) {
			t.Errorf("prompt contains example marker %q", marker)
		}
	}
}

func TestLegacyMemoryCategoryIsUnknown(t *testing.T) {
	if got, err := parseCategory("memory"); err == nil || got != "" {
		t.Fatalf("legacy memory accepted: %q %v", got, err)
	}
}

func TestDelegationChatReplyIsUnknown(t *testing.T) {
	for _, style := range []string{"responses", "chat"} {
		t.Run(style, func(t *testing.T) {
			client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := response("chat")
				if style == "chat" {
					body = chatResponse("chat")
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			client.apiStyle = style
			got, err := client.Classify(context.Background(), "delegated sentence")
			if got != "" || err == nil || !strings.Contains(err.Error(), "unknown category") {
				t.Fatalf("chat reply must be unknown: category=%q err=%v", got, err)
			}
		})
	}
}
