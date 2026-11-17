package uictl

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/prompts"
)

func TestPromptRuntimeHash(t *testing.T) {
	// Recorded from the exact runtime expression on clean origin/main a23baad.
	const want = "c9cfd2d82a58b1622e9af7f4575700aa11c5d048a2d887b693a08a1b6e3c4643"
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(Prompt+"\n"+SubsetDefinitions))); got != want {
		t.Fatal(got)
	}
}

func TestPromptOverrideComposition(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ui_controller.txt"), []byte("operator text"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := prompts.Load(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := Prompt
	Prompt = p["ui_controller"]
	defer func() { Prompt = old }()
	got := Prompt + "\n" + SubsetDefinitions
	if !strings.HasPrefix(got, "operator text\n") {
		t.Fatal("override missing")
	}
	for _, fact := range []string{requiredDefinitions, OptionalProperties, RenderingEnvironment} {
		if !strings.Contains(got, fact) {
			t.Fatal("code facts missing")
		}
	}
}
