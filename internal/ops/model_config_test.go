package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyStdinWritesSecretFileMode0600AndNeverEchoesKey(t *testing.T) {
	injectModelKeyChown(t, func(string, int, int) error { return nil }, true)
	p := filepath.Join(t.TempDir(), "key")
	if err := WriteModelSecretAtomic(p, []byte("private-key")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestConfigureRejectsBadURLsModelsAndWorldReadableKeyFile(t *testing.T) {
	for _, raw := range []string{"http://remote/v1", "https://user:pass@gateway/v1", "https://gateway/v1?x=1"} {
		if err := ValidateModelBaseURL(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if err := ValidateModelBaseURL("https://gateway"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateKeyFile(p); err == nil {
		t.Fatal("accepted world-readable key")
	}
}

func TestModelConfigurationAcceptsNoneAndMaxEffort(t *testing.T) {
	for _, effort := range []string{"", "none", "low", "medium", "high", "max"} {
		c := ModelConfig{BaseURL: "https://gateway.example", Model: "deepseek-flash", SecretPath: "/key", ReasoningEffort: effort}
		if _, err := c.Normalized(); err != nil {
			t.Errorf("effort=%q: %v", effort, err)
		}
	}
	if err := ValidateReasoningEffort("minimal"); err == nil {
		t.Fatal("accepted unsupported effort")
	}
}

func TestMemoryEffortWrittenAndPreservedAcrossUpdate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stack.env")
	if err := os.WriteFile(p, []byte("KEEP=yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteMemoryEnvAtomic(p, MemoryConfig{BaseURL: "https://gateway.example/v1", Model: "memory", ReasoningEffort: "high", SecretPath: "/key"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "ZOE_MEM0_LLM_REASONING_EFFORT=\"high\"") || !strings.Contains(string(b), "KEEP=yes") {
		t.Fatalf("env=%s", b)
	}
}

func TestMemoryConfigureAcceptsKeyStdinAndRotatesSecretFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memory-key")
	if err := WriteSecretAtomic(p, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretAtomic(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "new" {
		t.Fatalf("key changed incorrectly: %v", err)
	}
}
