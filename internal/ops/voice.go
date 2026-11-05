package ops

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// ReadEnvForVoice reads only literal deployment values needed by the operator CLI.
func ReadEnvForVoice(path string) (map[string]string, error) { return readEnv(path) }

func WritePrivateFile(path string, data []byte) error {
	if len(data) == 0 || strings.ContainsAny(string(data), "\r\x00") {
		return errors.New("private value is empty or invalid")
	}
	return writeFileAtomicOwnedWithChown(path, data, 0600, memoryWorkerUID, memoryWorkerGID, ModelKeyChown, effectiveUID)
}

func WriteVoiceEnvAtomic(path, baseURL, secretPath string, accountMedia bool) error {
	old, err := readEnv(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := []string{}
	if b, e := os.ReadFile(path); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && strings.HasPrefix(key, "ZOE_VOICE_") {
				continue
			}
			if strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
		}
	}
	if old == nil {
		old = map[string]string{}
	}
	lines = append(lines, "ZOE_VOICE_GATEWAY_URL="+strconv.Quote(baseURL), "ZOE_VOICE_GATEWAY_API_KEY_FILE="+strconv.Quote(secretPath), "ZOE_VOICE_SECRET_MAP=,ZOE_VOICE_GATEWAY_API_KEY=/run/secrets/voice_gateway_api_key", "ZOE_VOICE_ACCOUNT_MEDIA_ENABLED="+strconv.Quote(strconv.FormatBool(accountMedia)))
	return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0600)
}
