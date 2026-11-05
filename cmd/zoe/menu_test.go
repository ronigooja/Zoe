package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/ops"
)

func menuManager(t *testing.T) {
	t.Helper()
	old := opsManager
	opsManager = func() ops.Manager {
		return ops.NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "systemctl" && strings.Contains(strings.Join(args, " "), "show") {
				return []byte("LoadState=not-found\n"), os.ErrNotExist
			}
			return nil, nil
		})
	}
	t.Cleanup(func() { opsManager = old })
}

func TestMaintenanceMenuListsTriageModelItems(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := menuWithRoot(strings.NewReader("0\n"), &out, &errOut, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "10) Configure triage model") || !strings.Contains(out.String(), "11) Test triage model") {
		t.Fatalf("menu=%s", out.String())
	}
}

func TestMaintenanceMenuTriageFlowPromptsModelAndEffortAndWritesBoth(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"technical"}]}]}`))
	}))
	defer s.Close()
	keyPath := filepath.Join(root, "triage-key")
	old := terminalEcho
	terminalEcho = func(*os.File) (func(), bool, error) { return func() {}, true, nil }
	t.Cleanup(func() { terminalEcho = old })
	input := strings.NewReader("10\n" + s.URL + "\ntriage\n\n\n" + keyPath + "\nsecret-value\ny\n0\n")
	var out, errOut bytes.Buffer
	if err := menuWithRoot(input, &out, &errOut, root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "zoe-stack.env"))
	if err != nil || !strings.Contains(string(data), "ZOE_TRIAGE_MODEL") {
		t.Fatalf("env err=%v data=%s", err, data)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil || string(key) != "secret-value\n" {
		t.Fatalf("key=%q err=%v", key, err)
	}
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("key mode=%v err=%v", info.Mode().Perm(), err)
	}
	if strings.Contains(out.String()+errOut.String(), "secret-value") {
		t.Fatalf("secret leaked: out=%s err=%s", out.String(), errOut.String())
	}
}

func TestMaintenanceMenuTriagePromptsForThinkingAndWritesIt(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	keyPath := filepath.Join(root, "key")
	old := terminalEcho
	terminalEcho = func(*os.File) (func(), bool, error) { return func() {}, true, nil }
	t.Cleanup(func() { terminalEcho = old })
	input := strings.NewReader("10\nhttps://gateway.example\nm\nlow\ndisabled\n" + keyPath + "\nsecret\nn\n0\n")
	var out, errOut bytes.Buffer
	if err := menuWithRoot(input, &out, &errOut, root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Thinking (Enter = provider default, disabled = faster, enabled)") {
		t.Fatal(out.String())
	}
	b, _ := os.ReadFile(filepath.Join(root, "zoe-stack.env"))
	if !strings.Contains(string(b), `ZOE_TRIAGE_THINKING="disabled"`) {
		t.Fatalf("env=%s", b)
	}
}

func TestMaintenanceMenuTriageDoesNotAskForAPIStyle(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	var out, errOut bytes.Buffer
	_ = menuWithRoot(strings.NewReader("0\n"), &out, &errOut, root)
	if strings.Contains(out.String(), "API style") {
		t.Fatal(out.String())
	}
}

func TestMaintenanceMenuTriageValidationErrorSaysNothingSavedAndReprompts(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	keyPath := filepath.Join(root, "key")
	old := terminalEcho
	terminalEcho = func(*os.File) (func(), bool, error) { return func() {}, true, nil }
	t.Cleanup(func() { terminalEcho = old })
	in := strings.NewReader("10\nhttp://remote.invalid\nhttps://gateway.example\nm\nlow\n\n" + keyPath + "\nsecret\nn\n0\n")
	var out, errOut bytes.Buffer
	if err := menuWithRoot(in, &out, &errOut, root); err != nil {
		t.Fatal(err)
	}
	if strings.Count(errOut.String(), "Nothing was saved.") != 1 || !strings.HasSuffix(strings.TrimSpace(out.String()), "Select an option:") {
		t.Fatalf("out=%s err=%s", out.String(), errOut.String())
	}
}

func TestMaintenanceMenuTriageRejectsUnknownThinkingAndSaysNothingSaved(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	keyPath := filepath.Join(root, "key")
	old := terminalEcho
	terminalEcho = func(*os.File) (func(), bool, error) { return func() {}, true, nil }
	t.Cleanup(func() { terminalEcho = old })
	in := strings.NewReader("10\nhttps://gateway.example\nm\nlow\ninvalid\ndisabled\n" + keyPath + "\nsecret\nn\n0\n")
	var out, errOut bytes.Buffer
	if err := menuWithRoot(in, &out, &errOut, root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "Nothing was saved.") {
		t.Fatal(errOut.String())
	}
}

func TestMaintenanceMenuTriageEOFSaysCancelled(t *testing.T) {
	menuManager(t)
	var out, errOut bytes.Buffer
	_ = menuWithRoot(strings.NewReader("10\n"), &out, &errOut, t.TempDir())
	if !strings.Contains(out.String(), "Cancelled. Nothing was saved.") {
		t.Fatal(out.String())
	}
}

func TestMaintenanceMenuTestTriageReportsPass(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"task"}]}]}`))
	}))
	defer s.Close()
	keyPath := filepath.Join(root, "triage-key")
	if err := os.WriteFile(keyPath, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_TRIAGE_BASE_URL=\""+s.URL+"/v1\"\nZOE_TRIAGE_MODEL=\"triage\"\nZOE_TRIAGE_API_KEY_FILE=\""+keyPath+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := menuWithRoot(strings.NewReader("11\n0\n"), &out, &errOut, root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "PASS style=responses HTTP 2xx") {
		t.Fatalf("out=%s err=%s", out.String(), errOut.String())
	}
}

func TestZoeConfigureCommandIsGone(t *testing.T) {
	err := run([]string{"configure"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "usage: zoe") {
		t.Fatalf("err=%v", err)
	}
}
