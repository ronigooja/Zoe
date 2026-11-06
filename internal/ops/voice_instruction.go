package ops

import (
	"os"
	"strconv"
	"strings"

	"github.com/ronigooja/Zoe/internal/voice"
)

// VoiceInstruction reads the effective setting for the maintenance menu.
func (d Deployment) VoiceInstruction() (string, string, error) {
	values, err := readEnv(d.StackEnv)
	if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	raw := values["ZOE_VOICE_INSTRUCTION_TEXT"]
	effective, err := voice.ResolveInstructionText(raw)
	return raw, effective, err
}

// WriteVoiceInstructionAtomic applies only to the voice instruction setting;
// empty restores the default by removing the key. Compose dollar expansion is
// escaped independently of Go string quoting, including quotes and newlines.
func WriteVoiceInstructionAtomic(path, text string) error {
	if _, err := voice.ResolveInstructionText(text); err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var kept []string
	for _, line := range strings.Split(string(old), "\n") {
		key, _, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "export ")), "=")
		if key != "ZOE_VOICE_INSTRUCTION_TEXT" {
			kept = append(kept, line)
		}
	}
	for len(kept) > 0 && kept[len(kept)-1] == "" {
		kept = kept[:len(kept)-1]
	}
	if text != "" {
		kept = append(kept, "ZOE_VOICE_INSTRUCTION_TEXT="+strings.ReplaceAll(strconv.Quote(text), "$", `\$`))
	}
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return WriteFileAtomic(path, []byte(strings.Join(kept, "\n")+"\n"), mode)
}
