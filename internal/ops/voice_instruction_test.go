package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

func TestUpdatePreservesComposeEscapedVoiceInstruction(t *testing.T) {
	f := newUpdateFixture(t)
	path := filepath.Join(f.root, "zoe-stack.env")
	const text = `中文 "quotes" 'apostrophe' ${HOME} $$ \$value`
	if err := WriteVoiceInstructionAtomic(path, text); err != nil {
		t.Fatal(err)
	}
	runner := &updateRunner{}
	closeServers := fixedUpdateServers(t, filepath.Join(f.data, "opencode", "password"), false, true)
	defer closeServers()
	if err := NewManager(runner.run).Update(context.Background(), f.options(nil)); err != nil {
		t.Fatal(err)
	}
	values, err := dotenv.ReadWithLookup(func(string) (string, bool) { return "EXPANDED", true }, path)
	if err != nil || values["ZOE_VOICE_INSTRUCTION_TEXT"] != text {
		t.Fatalf("update changed Compose instruction: %q error=%v", values["ZOE_VOICE_INSTRUCTION_TEXT"], err)
	}
}

func TestVoiceInstructionComposeRoundTripAndRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zoe-stack.env")
	for _, text := range []string{`说 "你好" and 'hello' $HOME ${SECRET:-expanded} $$ \$money`, "first\nsecond\r\n\twith dollar $VALUE and \\", "off", ""} {
		if err := WriteVoiceInstructionAtomic(path, text); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		values, err := dotenv.ParseWithLookup(strings.NewReader(string(data)), func(string) (string, bool) { return "SHOULD_NOT_EXPAND", true })
		if err != nil {
			t.Fatal(err)
		}
		if values["ZOE_VOICE_INSTRUCTION_TEXT"] != text {
			t.Fatalf("Compose round trip=%q want=%q", values["ZOE_VOICE_INSTRUCTION_TEXT"], text)
		}
		local, err := readEnv(path)
		if err != nil || local["ZOE_VOICE_INSTRUCTION_TEXT"] != text {
			t.Fatalf("local round trip=%q want=%q error=%v", local["ZOE_VOICE_INSTRUCTION_TEXT"], text, err)
		}
		if text == "" && strings.Contains(string(data), "ZOE_VOICE_INSTRUCTION_TEXT") {
			t.Fatal("default did not remove key")
		}
	}
}

func TestVoiceInstructionInvalidSavesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stack.env")
	const original = "# operator line\nZOE_VOICE_INSTRUCTION_TEXT=off\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string([]byte{255}), strings.Repeat("界", 2001)} {
		if err := WriteVoiceInstructionAtomic(path, text); err == nil {
			t.Fatal("invalid instruction accepted")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != original {
			t.Fatal("invalid input changed environment")
		}
	}
}
