package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderedOpenCodeConfigHasProfilesAndNoWildcardAllow(t *testing.T) {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "system"), 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(d, "root")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-runner.env"), []byte("ZOE_OPENCODE_MODEL=provider/model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	o := LifecycleOptions{Root: root, DataRoot: d}
	if err := renderOpenCodeConfig(o); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(d, "opencode", "xdg-config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	agents := cfg["agents"].(map[string]any)
	for _, id := range []string{"zoe-worker", "zoe-steward", "zoe-super"} {
		if agents[id].(map[string]any)["system"] == "" {
			t.Fatal(id)
		}
	}
	if _, exists := cfg["permissions"]; exists {
		t.Fatalf("global permissions must be absent: %v", cfg["permissions"])
	}
}

func TestOpenCodeVersionOutput(t *testing.T) {
	tests := []struct {
		name, output string
		valid        bool
	}{
		{"upstream", "opencode v2.2.5", true},
		{"fork", "opencode v2.2.5-ronigooja.2", true},
		{"empty", "", false}, {"garbage", "garbage", false},
		{"multiline", "opencode v2.2.5\nnoise", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(func(context.Context, string, ...string) ([]byte, error) { return []byte(tt.output), nil })
			err := m.checkOpenCodeVersion(context.Background(), LifecycleOptions{OpenCodeBinary: "opencode"})
			if (err == nil) != tt.valid {
				t.Fatalf("checkOpenCodeVersion(%q) error = %v", tt.output, err)
			}
		})
	}
}

func TestOpenCodeVersionCommandFailure(t *testing.T) {
	m := NewManager(func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("failed") })
	if err := m.checkOpenCodeVersion(context.Background(), LifecycleOptions{OpenCodeBinary: "opencode"}); err == nil || !strings.Contains(err.Error(), "cannot determine host OpenCode version") {
		t.Fatalf("error = %v", err)
	}
}
