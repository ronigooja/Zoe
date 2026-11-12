package triage

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode"
)

func TestUnclearParserPreservesExactCategoryMatching(t *testing.T) {
	for _, output := range []string{"unclear", "UNCLEAR", " unclear. "} {
		if got, err := parseCategory(output); err != nil || got != Unclear {
			t.Errorf("parseCategory(%q) = %q, %v", output, got, err)
		}
	}
	for _, output := range []string{"unclearly", "unclear_context", "un-clear", "unclear/task", "unclear task", "not unclear", "unclear\ntask", "[unclear]"} {
		if got, err := parseCategory(output); err == nil || got != "" {
			t.Errorf("parseCategory(%q) = %q, %v; want rejection", output, got, err)
		}
	}
}

func TestUnclearDefinitionAndAllowedWordsInBothPromptModes(t *testing.T) {
	for _, pending := range []bool{false, true} {
		prompt := classificationPrompt(pending)
		for _, definition := range []string{
			"unclear: the sentence refers to something not stated in the sentence itself and cannot be classified into another category without earlier conversation.",
			"A sentence that is clear on its own must not be unclear; prefer the specific category.",
		} {
			if !strings.Contains(prompt, definition) {
				t.Errorf("pending=%t prompt lacks definition %q", pending, definition)
			}
		}
		_, allowed, ok := strings.Cut(prompt, "\nAllowed words: ")
		if !ok {
			t.Fatalf("pending=%t missing allowed words", pending)
		}
		found := false
		for _, word := range strings.Split(strings.TrimSuffix(allowed, "."), ", ") {
			if word == "unclear" {
				found = true
			}
		}
		if !found {
			t.Errorf("pending=%t unclear missing from allowed words", pending)
		}
		for _, r := range prompt {
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
				t.Errorf("pending=%t prompt contains CJK rune %U", pending, r)
			}
		}
	}
}

func TestClassifyUnclearModelOutputInBothStylesAndPromptModes(t *testing.T) {
	for _, style := range []string{"responses", "chat"} {
		for _, question := range []string{"", "Proceed with the pending request?"} {
			t.Run(style+"/"+question, func(t *testing.T) {
				client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					prompt, _ := body["instructions"].(string)
					if style == "chat" {
						messages, _ := body["messages"].([]any)
						if len(messages) != 2 {
							t.Errorf("messages count=%d; want stateless system and user messages", len(messages))
							return
						}
						message, _ := messages[0].(map[string]any)
						prompt, _ = message["content"].(string)
					}
					if prompt != classificationPrompt(question != "") || !strings.Contains(prompt, "unclear:") {
						t.Error("request missing unclear prompt in selected mode")
					}
					output := response("unclear")
					if style == "chat" {
						output = chatResponse("unclear")
					}
					_, _ = w.Write([]byte(output))
				}))
				defer server.Close()
				client.apiStyle = style
				got, err := client.ClassifyPending(context.Background(), "delegated sentence", question)
				if err != nil || got != Unclear {
					t.Fatalf("category=%q err=%v; want unclear", got, err)
				}
			})
		}
	}
}
