package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUIControllerModelConfiguration(t *testing.T) {
	prefix, err := menuModelPrefix("UI_Controller")
	if err != nil || prefix != "ZOE_UI_CONTROLLER" {
		t.Fatalf("prefix=%s err=%v", prefix, err)
	}
	for _, tc := range []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"", time.Minute, true}, {"500", 500 * time.Millisecond, true}, {"120000", 2 * time.Minute, true}, {"499", 0, false}, {"120001", 0, false},
	} {
		got, err := menuModelTimeout("UI_Controller", ModelConfig{TimeoutMS: tc.value})
		if (err == nil) != tc.valid || (tc.valid && got != tc.want) {
			t.Fatalf("timeout %q=%v err=%v", tc.value, got, err)
		}
	}
	path := filepath.Join(t.TempDir(), "stack.env")
	cfg := ModelConfig{BaseURL: "https://ui.invalid", Model: "ui-model", SecretPath: "/root/secrets/ui_controller-api-key", TimeoutMS: "60000"}
	if err := WriteMenuModelEnvAtomic(path, cfg, "UI_Controller"); err != nil {
		t.Fatal(err)
	}
	got, err := (Deployment{StackEnv: path}).MenuModelConfig("UI_Controller")
	if err != nil || got.BaseURL != cfg.BaseURL || got.Model != cfg.Model || got.SecretPath != cfg.SecretPath || got.TimeoutMS != cfg.TimeoutMS {
		t.Fatalf("configuration=%+v err=%v", got, err)
	}
}

func TestUIControllerRetryEnvironmentPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stack.env")
	if err := os.WriteFile(path, []byte("ZOE_UI_CONTROLLER_MAX_RETRIES=2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := ModelConfig{BaseURL: "https://ui.invalid", Model: "ui-model", SecretPath: "/root/secrets/ui_controller-api-key", TimeoutMS: "60000"}
	if err := WriteMenuModelEnvAtomic(path, cfg, "UI_Controller"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "ZOE_UI_CONTROLLER_MAX_RETRIES=2") {
		t.Fatalf("retry setting lost: %v", err)
	}
}
