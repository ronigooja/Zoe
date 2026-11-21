package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range Names() {
		value := "value\n"
		if name == "ui_controller" {
			value = "[common]\nshared\n[show]\nshow\n[task_question]\nquestion\n[chat_input]\nchat\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadFailures(t *testing.T) {
	for _, name := range Names() {
		for _, tc := range []struct{ name, data, category string }{
			{"missing", "", "prompt_missing"},
			{"empty", "", "prompt_empty"},
			{"newline", "\n", "prompt_empty"},
			{"invalid", "\xffsecret", "prompt_invalid_utf8"},
			{"large", strings.Repeat("x", 16001), "prompt_too_large"},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				dir := fixture(t)
				path := filepath.Join(dir, name+".txt")
				if tc.name == "missing" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
				_, err := Load(dir)
				if err == nil || err.Error() != tc.category+": "+name {
					t.Fatalf("error = %v", err)
				}
			})
		}
	}
}

func TestRequiredMemoryPrompts(t *testing.T) {
	if len(Names()) != 10 {
		t.Fatal("expected ten required prompts")
	}
	dir := fixture(t)
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"memory_ingest", "memory_write", "memory_custom"} {
		path, size, ok := s.Metadata(name)
		if !ok || path != filepath.Join(dir, name+".txt") || size != 6 {
			t.Fatalf("unexpected metadata for %s", name)
		}
	}
}

func TestLoadBoundaryAndNewline(t *testing.T) {
	dir := fixture(t)
	path := filepath.Join(dir, "register.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 16000)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compact.txt"), []byte("text\n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "voice_instruction.txt"), []byte("off\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Register()) != 16000 || s.Compact() != "text\n" || s.VoiceInstruction() != "" {
		t.Fatal("unexpected loaded content")
	}
	p, size, ok := s.Metadata("register")
	if !ok || p != path || size != 16000 {
		t.Fatal("unexpected metadata")
	}
}

func TestVoiceInstructionOffIsExact(t *testing.T) {
	dir := fixture(t)
	path := filepath.Join(dir, "voice_instruction.txt")
	for _, value := range []string{"off ", "off\n\n", "OFF"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		s, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.VoiceInstruction(); got != strings.TrimSuffix(value, "\n") {
			t.Fatal("non-exact off was disabled")
		}
	}
}
