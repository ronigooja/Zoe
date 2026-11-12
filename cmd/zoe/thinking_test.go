package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceMenuCompactAcceptsEffortNone(t *testing.T) {
	menuManager(t)
	containerKeyFixture(t)
	root := t.TempDir()
	keyPath := filepath.Join(root, "key")
	old := terminalEcho
	terminalEcho = func(*os.File) (func(), bool, error) { return func() {}, true, nil }
	t.Cleanup(func() { terminalEcho = old })
	input := strings.NewReader("10\nhttps://gateway.example\ndeepseek-flash\nnone\ndisabled\n" + keyPath + "\nsecret\nn\n0\n")
	var out, errOut bytes.Buffer
	if err := menuWithRoot(input, &out, &errOut, root); err != nil {
		t.Fatal(err)
	}
	if errOut.Len() != 0 || !strings.Contains(out.String(), "Reasoning effort (none/low/medium/high/max; empty keeps default)") {
		t.Fatalf("out=%s err=%s", out.String(), errOut.String())
	}
	b, err := os.ReadFile(filepath.Join(root, "zoe-stack.env"))
	if err != nil || !strings.Contains(string(b), `ZOE_COMPACT_REASONING_EFFORT="none"`) || !strings.Contains(string(b), `ZOE_COMPACT_THINKING="disabled"`) {
		t.Fatalf("env=%s err=%v", b, err)
	}
}

func TestCompactTestUsesDisabledThinkingEffortNone(t *testing.T) {
	fakeServices(t, false)
	containerKeyFixture(t)
	var body map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, `{"output_text":"task"}`)
	}))
	defer s.Close()
	root := t.TempDir()
	key := filepath.Join(root, "key")
	if err := os.WriteFile(key, []byte("key"), 0600); err != nil {
		t.Fatal(err)
	}
	env := "ZOE_COMPACT_BASE_URL=" + s.URL + "\nZOE_COMPACT_MODEL=deepseek-flash\nZOE_COMPACT_API_KEY_FILE=" + key + "\nZOE_COMPACT_THINKING=disabled\nZOE_COMPACT_REASONING_EFFORT=high\n"
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte(env), 0600); err != nil {
		t.Fatal(err)
	}
	if err := menuTestModel(root, io.Discard, "Compact"); err != nil {
		t.Fatal(err)
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "none" || body["thinking"] != nil {
		t.Fatalf("request=%v", body)
	}
}
