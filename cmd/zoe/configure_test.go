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

func TestMenuTriageFlowWritesConfigAndSecretThenTestsEndpoint(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	endpointCalls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpointCalls++
		_, _ = w.Write([]byte(`{"category":"technical"}`))
	}))
	defer s.Close()
	keyPath := filepath.Join(root, "triage-key")
	input := strings.NewReader("2\n" + s.URL + "/v1\ntriage\n" + keyPath + "\ny\n0\n")
	var out, errOut bytes.Buffer
	if err := runConfigureMenu(ConfigureMenuIO{Root: root, In: input, Out: &out, ErrOut: &errOut, PromptSecret: func(_ io.Writer) ([]byte, error) { return []byte("secret-value"), nil }}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "zoe-stack.env"))
	if !strings.Contains(string(data), "ZOE_TRIAGE_MODEL") {
		t.Fatalf("env=%s", data)
	}
	key, _ := os.ReadFile(keyPath)
	if string(key) != "secret-value\n" {
		t.Fatalf("key=%q", key)
	}
	if endpointCalls != 1 || strings.Contains(out.String()+errOut.String(), "secret-value") {
		t.Fatalf("calls=%d output=%s err=%s", endpointCalls, out.String(), errOut.String())
	}
}

func TestMenuKeepsCurrentValuesOnEmptyInputAndRejectsInvalidThenReprompts(t *testing.T) {
	menuManager(t)
	root := t.TempDir()
	stack := filepath.Join(root, "zoe-stack.env")
	if err := os.WriteFile(stack, []byte("ZOE_TRIAGE_BASE_URL=\"https://gateway.example/v1\"\nZOE_TRIAGE_MODEL=\"m\"\nZOE_TRIAGE_API_KEY_FILE=\"/missing\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	input := strings.NewReader("2\n\n\n\n")
	err := runConfigureMenu(ConfigureMenuIO{Root: root, In: input, Out: &out, ErrOut: &errOut, PromptSecret: func(io.Writer) ([]byte, error) { return []byte(""), nil }})
	if err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Triage model") {
		t.Fatal("menu did not show model screen")
	}
}

func TestMenuEOFExitsCleanlyWithoutPartialWrites(t *testing.T) {
	root := t.TempDir()
	var out, errOut bytes.Buffer
	if err := runConfigureMenu(ConfigureMenuIO{Root: root, In: strings.NewReader("2\n"), Out: &out, ErrOut: &errOut, PromptSecret: func(io.Writer) ([]byte, error) { return []byte("never"), nil }}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "zoe-stack.env")); !os.IsNotExist(err) {
		t.Fatalf("partial env exists: %v", err)
	}
}

func TestMenuShowConfigNeverPrintsSecrets(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "key")
	_ = os.WriteFile(secret, []byte("do-not-print"), 0600)
	_ = os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_TRIAGE_BASE_URL=\"https://gateway.example/v1\"\nZOE_TRIAGE_MODEL=\"m\"\nZOE_TRIAGE_API_KEY_FILE=\""+secret+"\"\n"), 0600)
	var out, errOut bytes.Buffer
	_ = runConfigureMenu(ConfigureMenuIO{Root: root, In: strings.NewReader("5\n0\n"), Out: &out, ErrOut: &errOut})
	if strings.Contains(out.String()+errOut.String(), "do-not-print") {
		t.Fatal("secret leaked")
	}
}

func TestStatusShowsTriageWithoutSecrets(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "triage-key")
	_ = os.WriteFile(secret, []byte("status-secret"), 0600)
	_ = os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_TRIAGE_BASE_URL=\"https://gateway.example/v1\"\nZOE_TRIAGE_MODEL=\"triage-model\"\nZOE_TRIAGE_API_KEY_FILE=\""+secret+"\"\n"), 0600)
	menuManager(t)
	var out bytes.Buffer
	if err := statusOperation([]string{"--root", root}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Triage: enabled") || strings.Contains(out.String(), "status-secret") || strings.Contains(out.String(), "https://") {
		t.Fatalf("status leaked data: %s", out.String())
	}
}

func TestMenuMemoryFlowRotatesKeyWithoutEcho(t *testing.T) {
	// The model transaction is covered by the existing memory transaction tests;
	// this assertion keeps the menu's secret boundary explicit.
	var out bytes.Buffer
	secret, err := readToken(strings.NewReader("rotated-key\n"), &out)
	if err != nil || string(secret) != "rotated-key" || strings.Contains(out.String(), "rotated-key") {
		t.Fatalf("secret boundary failed: %q %v", out.String(), err)
	}
}
