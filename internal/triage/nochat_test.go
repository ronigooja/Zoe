package triage

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestDelegationPromptDefinitionsOnly(t *testing.T) {
	for _, pending := range []bool{false, true} {
		prompt := classificationPrompt(pending)
		words := BaseWords
		if pending {
			words = PendingWords
		}
		categories := strings.Split(words, ", ")
		definitions := map[string]int{}
		for _, line := range strings.Split(prompt, "\n") {
			if strings.HasPrefix(line, "Allowed words: ") {
				continue
			}
			if category, _, ok := strings.Cut(line, ": "); ok {
				definitions[category]++
			}
		}
		if len(definitions) != len(categories) {
			t.Errorf("pending=%t definitions=%v, want %v", pending, definitions, categories)
		}
		for _, category := range categories {
			if definitions[category] != 1 {
				t.Errorf("pending=%t category=%s definitions=%d", pending, category, definitions[category])
			}
		}
		if !strings.HasSuffix(prompt, "Allowed words: "+words+".") {
			t.Errorf("pending=%t incorrect allowed words", pending)
		}
		if strings.Contains(prompt, "chat:") || strings.Contains(prompt, "memory:") {
			t.Error("legacy category defined")
		}
		if !pending && (strings.Contains(prompt, "confirm") || strings.Contains(prompt, "decline")) {
			t.Error("pending answers offered without question")
		}
		if !strings.Contains(prompt, "unsure, choose task") || !strings.Contains(prompt, "exactly one lowercase word") || !strings.Contains(prompt, "Output nothing else") {
			t.Error("missing fallback or single-word output contract")
		}
		for _, marker := range []string{"example", "e.g.", "for instance", "\"", "“", "”"} {
			if strings.Contains(strings.ToLower(prompt), marker) {
				t.Errorf("prompt contains example marker %q", marker)
			}
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
