package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptStartupError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ui_controller.txt"), []byte("private-content\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOE_PROMPT_DIR", dir)
	err := run()
	if err == nil || !strings.Contains(err.Error(), "ui_controller.txt") || !strings.Contains(err.Error(), "NUL") || strings.Contains(err.Error(), "private-content") {
		t.Fatalf("unsafe/wrong startup error: %v", err)
	}
}
