package modelclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/modelmonitor"
)

func TestObservationTextFiltersAndBounds(t *testing.T) {
	text, truncated := observationText(`configured-secret Bearer abc.def password=hello sk-abcdefghijk`, "configured-secret")
	if truncated {
		t.Fatal("unexpected truncation")
	}
	for _, secret := range []string{"configured-secret", "abc.def", "hello", "sk-abcdefghijk"} {
		if strings.Contains(text, secret) {
			t.Fatalf("secret %q was not redacted: %s", secret, text)
		}
	}
	text, truncated = observationText(strings.Repeat("x", observationTextLimit+1), "")
	if !truncated || strings.Contains(text, "xxx") {
		t.Fatal("oversized text was not omitted")
	}
}

func TestObservedClientFallbackRouting(t *testing.T) {
	sub := modelmonitor.Subscribe()
	defer sub.Close()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/responses" {
			http.Error(w, "unsupported", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"choices":[{"finish_reason":"length","message":{"content":"answer"}}]}`)
	}))
	defer server.Close()
	values := map[string]string{"ZOE_REGISTER_BASE_URL": server.URL, "ZOE_REGISTER_MODEL": "fixture", "ZOE_REGISTER_API_KEY_FILE": "fixture"}
	c, err := FromEnv("ZOE_REGISTER", func(k string) string { return values[k] }, func(string) ([]byte, error) { return []byte("key"), nil }, 1000, 1, 5000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Complete(context.Background(), "instruction", "input")
	if err != nil || got != "answer" {
		t.Fatalf("got %q, %v", got, err)
	}
	if strings.Join(paths, ",") != "/responses,/chat/completions" || c.component != "register" {
		t.Fatalf("paths=%v component=%q", paths, c.component)
	}
	events := make([]modelmonitor.Event, 4)
	for i := range events {
		select {
		case item := <-sub.Events:
			if item.Gap {
				t.Fatalf("unexpected gap at event %d", i)
			}
			events[i] = item.Event
		default:
			t.Fatalf("missing event %d", i)
		}
	}
	for i, kind := range []string{"request", "error", "request", "response"} {
		if events[i].Type != kind || events[i].Component != "register" {
			t.Fatalf("event %d: %+v", i, events[i])
		}
	}
	if events[0].CallID == "" || events[0].CallID != events[1].CallID || events[2].CallID != events[3].CallID || events[0].CallID == events[2].CallID {
		t.Fatal("incorrect attempt correlation")
	}
	for i, e := range events {
		if e.ConversationID == "" || e.ConversationID != events[0].ConversationID || e.ConversationStart != (i == 0) {
			t.Fatalf("incorrect conversation metadata on event %d", i)
		}
		if e.Type == "request" && (len(e.RequestMessages) != 2 || e.RequestMessages[1] != (modelmonitor.RequestMessage{Role: "user", Text: "input"})) {
			t.Fatalf("incorrect ordered request messages on event %d", i)
		}
	}
	if !events[3].Incomplete || events[3].Truncated || events[1].Incomplete {
		t.Fatal("token-limit completion or HTTP rejection was misclassified")
	}
	var payload struct {
		Model   string         `json:"model"`
		Options map[string]any `json:"options"`
	}
	if err := json.Unmarshal(events[2].Payload, &payload); err != nil || payload.Model != "fixture" || payload.Options["max_tokens"] != float64(1024) || payload.Options["stream"] != false {
		t.Fatalf("request payload: %s", events[2].Payload)
	}
}

func TestObservationComponents(t *testing.T) {
	for prefix, want := range map[string]string{"ZOE_REGISTER": "register", "ZOE_COMPACT": "compact", "ZOE_STEWARD": "steward", "ZOE_UI_CONTROLLER": "ui_controller", "PROBE": ""} {
		if got := observationComponent(prefix); got != want {
			t.Errorf("%s: got %q want %q", prefix, got, want)
		}
	}
}
