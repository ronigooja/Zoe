package prompts

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These hashes were recorded from clean origin/main a23baad constants.
func TestDefaultHashes(t *testing.T) {
	for name, want := range map[string]string{
		"ui_controller":        "52e6de999e4f7572d9080c04821725ffa282b90ea2e434165211e2dfb44b4de5",
		"steward_choose":       "281806c2249f1f7bdc17572252efd7271f1796aee4d550ebfeda1e230a73ab10",
		"register_attribution": "5898ffd1b117191de1c2c8176efcf33d1045f86e84eb2bcf1d25e3fccfb572ba",
		"register_compact":     "add8545b6339033581040c702a01374a714370efe58e8588f72e70d089ae99cd",
		"task_ui_display":      "6fefd7812722ddcc56ca0111111882bb43732029a9406bf5ad00b39008f31054",
		"voice_instruction":    "0438963491114fc94fee23b59b80a482efd4824096519127a9a824621c5d8b67",
	} {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(Default(name)))); got != want {
			t.Fatalf("%s hash=%s", name, got)
		}
	}
}

func TestLoad(t *testing.T) {
	p, err := Load("", nil, nil)
	if err != nil || p["ui_controller"] != Default("ui_controller") {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, text, reason string }{
		{"override", "operator text\n", ""},
		{"utf8", "private-content\xff", "invalid UTF-8"},
		{"large", strings.Repeat("x", MaxBytes+1), "exceeds"},
		{"empty", "", "empty"},
		{"blank", " \n", "empty"},
		{"nul", "private-content\x00", "NUL"},
		{"symlink", "private-content", "regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ui_controller.txt")
			if tc.name == "symlink" {
				target := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(target, []byte(tc.text), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			var logs string
			p, err := Load(dir, func(f string, a ...any) { logs += fmt.Sprintf(f, a...) }, nil)
			if tc.reason != "" {
				if err == nil || !strings.Contains(err.Error(), "ui_controller.txt") || !strings.Contains(err.Error(), tc.reason) || strings.Contains(err.Error(), "private-content") {
					t.Fatalf("unsafe/wrong error: %v", err)
				}
				return
			}
			if err != nil || p["ui_controller"] != tc.text || p["voice_instruction"] != Default("voice_instruction") || strings.Contains(logs, tc.text) || !strings.Contains(logs, "prompt_override name=ui_controller sha256=") {
				t.Fatalf("override failed: %v", err)
			}
		})
	}
	t.Run("unknown", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "unknown.txt"), []byte("private-content"), 0600); err != nil {
			t.Fatal(err)
		}
		var warning string
		_, err := Load(dir, nil, func(f string, a ...any) { warning += fmt.Sprintf(f, a...) })
		if err != nil || !strings.Contains(warning, "unknown.txt") || strings.Contains(warning, "private-content") {
			t.Fatal(err, warning)
		}
	})
}
