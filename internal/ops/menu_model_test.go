package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMenuModelEnvIsolationTimeoutAndLegacyPreservation(t *testing.T) {
	for _, kind := range []string{"Register", "Compact"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stack.env")
			legacy := "# untouched\nZOE_TRIAGE_TIMEOUT_MS=not-a-number\nZOE_TRIAGE_MODEL=obsolete\n"
			if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
				t.Fatal(err)
			}
			c := ModelConfig{BaseURL: "https://gateway.example/v1", Model: "model", SecretPath: "/etc/zoe/secrets/key", TimeoutMS: "10000"}
			if err := WriteMenuModelEnvAtomic(path, c, kind); err != nil {
				t.Fatal(err)
			}
			d := Deployment{StackEnv: path}
			loaded, err := d.MenuModelConfig(kind)
			if err != nil || loaded != c {
				t.Fatalf("readback mismatch: %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(before), legacy) || !strings.Contains(string(before), "ZOE_"+strings.ToUpper(kind)+"_TIMEOUT_MS=\"10000\"") {
				t.Fatal("timeout or legacy lines lost")
			}
			c.TimeoutMS = "invalid"
			if err := WriteMenuModelEnvAtomic(path, c, kind); err == nil {
				t.Fatal("invalid timeout accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("invalid config modified env")
			}
			if err := WriteMenuModelEnvAtomic(path, c, "Triage"); err == nil {
				t.Fatal("removed model accepted")
			}
		})
	}
}

func TestWriteEnvKeepsIgnoredTriageLinesUnchanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stack.env")
	legacy := "ZOE_TRIAGE_BASE_URL=http://ignored.invalid\nZOE_TRIAGE_TIMEOUT_MS=invalid\nZOE_TRIAGE_MODEL=old\n"
	if err := os.WriteFile(p, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeEnv(p, map[string]string{"ZOE_REGISTER_MODEL": "register", "ZOE_COMPACT_MODEL": "compact"}, 0600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || !strings.HasPrefix(string(b), legacy) {
		t.Fatal("legacy values were changed")
	}
}
