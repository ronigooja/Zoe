package steward

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func configured(t *testing.T, base string, timeout string) *Client {
	t.Helper()
	env := map[string]string{"ZOE_STEWARD_BASE_URL": base, "ZOE_STEWARD_MODEL": "configured-model", "ZOE_STEWARD_API_KEY_FILE": "/test/key", "ZOE_STEWARD_TIMEOUT_MS": timeout, "ZOE_STEWARD_THINKING": "disabled", "ZOE_STEWARD_REASONING_EFFORT": "none"}
	c, err := FromEnv(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("private-fixture-key"), nil })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStatelessStrictContractAndAutoStyle(t *testing.T) {
	var inputs []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var b map[string]json.RawMessage
		_ = json.Unmarshal(body, &b)
		if _, ok := b["previous_response_id"]; ok {
			t.Error("history identifier present")
		}
		var messages []map[string]string
		_ = json.Unmarshal(b["messages"], &messages)
		if len(messages) != 2 || messages[1]["role"] != "user" {
			t.Errorf("messages=%s", b["messages"])
		}
		inputs = append(inputs, messages[1]["content"])
		_, _ = fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ask"}}]}`)
	}))
	defer s.Close()
	c := configured(t, s.URL, "")
	for _, text := range []string{"first private sentence", "second sentence"} {
		d, e := c.Decide(context.Background(), text)
		if d != Ask || e != nil {
			t.Fatalf("%s %v", d, e)
		}
	}
	if len(inputs) != 2 || inputs[0] != "first private sentence" || inputs[1] != "second sentence" {
		t.Fatalf("request history leaked: %v", inputs)
	}
	if c.Style() != "chat" {
		t.Fatalf("style=%s", c.Style())
	}
}

func TestErrorsTimeoutAndUnknownFallback(t *testing.T) {
	for _, mode := range []string{"error", "timeout", "unknown", "punctuation", "uppercase"} {
		t.Run(mode, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "error" {
					w.WriteHeader(500)
					_, _ = fmt.Fprint(w, "private-fixture-key")
					return
				}
				if mode == "timeout" {
					time.Sleep(100 * time.Millisecond)
					return
				}
				word := map[string]string{"unknown": "wait", "punctuation": "ask.", "uppercase": "ASK"}[mode]
				_, _ = fmt.Fprintf(w, `{"output_text":%q}`, word)
			}))
			defer s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			d, err := configured(t, s.URL, "200").Decide(ctx, "ordinary sentence")
			if err == nil || Resolve(d, err, false) != Dispatch || Resolve(d, err, true) != Ask {
				t.Fatalf("decision=%s err=%v", d, err)
			}
			if strings.Contains(err.Error(), "private-fixture-key") {
				t.Fatal("secret exposed")
			}
		})
	}
}
