package modelclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/responses" {
			http.Error(w, "unsupported", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"answer"}}]}`)
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
}

func TestObservationComponents(t *testing.T) {
	for prefix, want := range map[string]string{"ZOE_REGISTER": "register", "ZOE_COMPACT": "compact", "ZOE_STEWARD": "steward", "ZOE_UI_CONTROLLER": "ui_controller", "PROBE": ""} {
		if got := observationComponent(prefix); got != want {
			t.Errorf("%s: got %q want %q", prefix, got, want)
		}
	}
}
