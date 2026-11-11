package progressreply

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestReplyFactsAndEmptyFacts(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "facts", true: "empty"}[empty], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Instructions string `json:"instructions"`
					Input        string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Instructions != instructions {
					t.Error("instruction changed")
				}
				var input struct {
					Sentence, Restatement string
					Tasks                 []map[string]any
				}
				if err := json.Unmarshal([]byte(body.Input), &input); err != nil {
					t.Error(err)
				}
				if input.Sentence != "How is it?" || input.Restatement != "The report" {
					t.Error("missing input")
				}
				if input.Tasks == nil {
					t.Error("tasks must be a JSON array")
				}
				if empty && len(input.Tasks) != 0 {
					t.Error("expected empty facts")
				}
				if !empty {
					if len(input.Tasks) != 1 {
						t.Error("missing facts")
					} else {
						fact := input.Tasks[0]
						for _, key := range []string{"id", "task_id", "ref", "alias"} {
							if _, ok := fact[key]; ok {
								t.Errorf("identifier %s", key)
							}
						}
						if len(fact) != 8 || fact["request"] != "Write report" || fact["result"] != "Draft" || fact["memory_text"] != "Earlier work" {
							t.Errorf("incomplete fact: %v", fact)
						}
					}
				}
				json.NewEncoder(w).Encode(map[string]string{"output_text": "  Work is underway. \n"})
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Model: "test", APIKey: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if client.timeout != 20*time.Second {
				t.Fatal("wrong default")
			}
			var tasks []Fact
			if !empty {
				tasks = []Fact{{Request: "Write report", Status: "active", CreatedAt: "2026-11-12T00:00:00Z", Result: "Draft", Additions: []string{"Add references"}, MemoryText: "Earlier work"}}
			}
			got, err := client.Reply(context.Background(), "How is it?", "The report", tasks)
			if err != nil || got != "Work is underway." {
				t.Fatalf("reply=%q err=%v", got, err)
			}
		})
	}
}

func TestReplyOutputAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		status       int
		delay        time.Duration
		wantError    bool
	}{
		{name: "empty", output: " \n", wantError: true},
		{name: "long", output: strings.Repeat("界", 650)},
		{name: "http_error", status: 503, wantError: true},
		{name: "timeout", delay: 700 * time.Millisecond, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.delay > 0 {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(tc.delay):
					}
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				json.NewEncoder(w).Encode(map[string]string{"output_text": tc.output})
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Model: "test", APIKey: "test", Timeout: 500 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.Reply(context.Background(), "question", "restatement", nil)
			if (err != nil) != tc.wantError {
				t.Fatalf("reply=%q err=%v", got, err)
			}
			if tc.name == "long" && (!utf8.ValidString(got) || utf8.RuneCountInString(got) != 600) {
				t.Fatal("not bounded by runes")
			}
		})
	}
}

func TestNewValidation(t *testing.T) {
	for _, timeout := range []time.Duration{-time.Second, 499 * time.Millisecond, 60001 * time.Millisecond} {
		if _, err := New(Config{BaseURL: "http://localhost", Model: "test", APIKey: "test", Timeout: timeout}); err == nil {
			t.Errorf("accepted %v", timeout)
		}
	}
	for _, cfg := range []Config{{}, {BaseURL: "http://example.com", Model: "test", APIKey: "test"}, {BaseURL: "http://localhost", APIKey: "test"}, {BaseURL: "http://localhost", Model: "test"}} {
		if _, err := New(cfg); err == nil {
			t.Error("accepted invalid config")
		}
	}
}
