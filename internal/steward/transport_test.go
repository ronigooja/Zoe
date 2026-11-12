package steward

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChooseStatelessAutoStyleAndIgnoredLegacyEnv(t *testing.T) {
	var inputs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["previous_response_id"]; ok {
			t.Error("history identifier present")
		}
		var messages []map[string]string
		_ = json.Unmarshal(body["messages"], &messages)
		if len(messages) != 2 || messages[1]["role"] != "user" {
			t.Errorf("messages=%v", messages)
			return
		}
		inputs = append(inputs, messages[1]["content"])
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": `{"action":"new","task":""}`}}}})
	}))
	defer server.Close()
	env := map[string]string{"ZOE_STEWARD_BASE_URL": server.URL, "ZOE_STEWARD_MODEL": "fixture", "ZOE_STEWARD_API_KEY_FILE": "fixture-reference", "ZOE_TRIAGE_BASE_URL": "invalid", "ZOE_TRIAGE_TIMEOUT_MS": "invalid"}
	c, err := FromEnv(func(k string) string {
		if strings.HasPrefix(k, "ZOE_TRIAGE_") {
			t.Errorf("legacy key consumed: %s", k)
		}
		return env[k]
	}, func(string) ([]byte, error) { return []byte("fixture"), nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, sentence := range []string{"first sentence", "second sentence"} {
		choice, err := c.Choose(context.Background(), sentence, "none", "", nil)
		if err != nil || choice.Action != ActionNew {
			t.Fatalf("choice=%+v error=%v", choice, err)
		}
	}
	if len(inputs) != 2 || !strings.Contains(inputs[0], "first sentence") || strings.Contains(inputs[1], "first sentence") || c.Style() != "chat" {
		t.Fatalf("stateless negotiation failed: style=%s inputs=%v", c.Style(), inputs)
	}
}

func TestStewardExplicitStyle(t *testing.T) {
	for _, style := range []string{"chat", "responses", "invalid"} {
		t.Run(style, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "/responses"
				if style == "chat" {
					want = "/chat/completions"
				}
				if r.URL.Path != want {
					t.Errorf("path=%s want=%s", r.URL.Path, want)
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			env := map[string]string{"ZOE_STEWARD_BASE_URL": server.URL, "ZOE_STEWARD_MODEL": "fixture", "ZOE_STEWARD_API_KEY_FILE": "fixture-reference", "ZOE_STEWARD_API_STYLE": style}
			c, err := FromEnv(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil })
			if style == "invalid" {
				if err == nil {
					t.Fatal("invalid style accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Choose(context.Background(), "sentence", "none", "", nil); err == nil || errorKind(err) != "http_status" {
				t.Fatalf("error=%v", err)
			}
			if c.Style() != style {
				t.Fatalf("style=%s", c.Style())
			}
		})
	}
}
