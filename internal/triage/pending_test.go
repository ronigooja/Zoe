package triage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPendingClassifierStatelessBinding(t *testing.T) {
	for _, reply := range []Category{Confirm, Decline} {
		for _, question := range []string{"", "Proceed with this exact request?"} {
			t.Run(string(reply)+"/"+question, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					input, _ := body["input"].(string)
					prompt, _ := body["instructions"].(string)
					if question == "" {
						if input != "yes" || strings.Contains(prompt, "confirm:") || strings.Contains(prompt, "decline:") {
							t.Errorf("unbound request offered pending answers")
						}
					} else {
						var data map[string]string
						if err := json.Unmarshal([]byte(input), &data); err != nil || len(data) != 2 || data["utterance"] != "yes" || data["pending_question"] != question {
							t.Errorf("input is not bounded question data: %q", input)
						}
						if !strings.Contains(prompt, "confirm:") || strings.Contains(prompt, question) {
							t.Error("question leaked into instructions")
						}
					}
					for _, key := range []string{"previous_response_id", "conversation", "history"} {
						if _, ok := body[key]; ok {
							t.Errorf("history field %s", key)
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"output_text": string(reply)})
				}))
				defer server.Close()
				env := map[string]string{"ZOE_TRIAGE_BASE_URL": server.URL, "ZOE_TRIAGE_MODEL": "fake", "ZOE_TRIAGE_API_KEY_FILE": "/fake/key"}
				client, err := FromEnv(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("fake"), nil })
				if err != nil {
					t.Fatal(err)
				}
				category, err := client.ClassifyPending(context.Background(), "yes", question)
				if question == "" {
					if err == nil {
						t.Fatal("accepted confirm without pending question")
					}
				} else if err != nil || category != reply {
					t.Fatalf("category=%s err=%v", category, err)
				}
			})
		}
	}
}
