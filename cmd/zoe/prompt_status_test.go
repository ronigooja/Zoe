package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "register.txt")
	if err := os.WriteFile(path, []byte("private text"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task_summary.txt"), []byte("private instruction"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writePromptStatus(&out, dir)
	text := out.String()
	if !strings.Contains(text, "name=task_summary path="+filepath.Join(dir, "task_summary.txt")+" bytes=19") || strings.Contains(text, "private instruction") {
		t.Fatal("incorrect task summary prompt status")
	}
	if !strings.Contains(text, "name=register path="+path+" bytes=12") || !strings.Contains(text, "name=compact path="+filepath.Join(dir, "compact.txt")+" missing") || strings.Contains(text, "private text") {
		t.Fatal("incorrect prompt status")
	}
}
