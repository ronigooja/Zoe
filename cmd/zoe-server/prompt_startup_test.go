package main

import (
	"testing"
)

func TestStartupRejectsMissingPrompt(t *testing.T) {
	t.Setenv("ZOE_LIMITS_FILE", "../../deploy/limits.json")
	t.Setenv("ZOE_PROMPT_DIR", t.TempDir())
	if err := run(); err == nil || err.Error() != "prompt_missing: ui_controller" {
		t.Fatalf("unexpected startup error: %v", err)
	}
}
