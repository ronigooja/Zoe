package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdatePreservesVoiceInstructionText(t *testing.T) {
	f := newUpdateFixture(t)
	path := filepath.Join(f.root, "zoe-stack.env")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	const custom = "Only delegate private history requests; do not read this aloud."
	_, err = file.WriteString("\nZOE_VOICE_INSTRUCTION_TEXT=" + custom + "\n")
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	runner := &updateRunner{}
	closeServers := fixedUpdateServers(t, filepath.Join(f.data, "opencode", "password"), false, true)
	defer closeServers()
	if err := NewManager(runner.run).Update(context.Background(), f.options(nil)); err != nil {
		t.Fatal(err)
	}
	values, err := readEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["ZOE_VOICE_INSTRUCTION_TEXT"] != custom || !isOperatorOwnedEnvKey("ZOE_VOICE_INSTRUCTION_TEXT") {
		t.Fatal("zoe update did not preserve the operator-owned voice instruction")
	}
}
