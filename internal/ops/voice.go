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

func WriteVoiceEnvAtomic(path, baseURL, secretPath string) error {
	lines := []string{}
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok {
				switch key {
				case "ZOE_VOICE_GATEWAY_URL", "ZOE_VOICE_GATEWAY_API_KEY_FILE", "ZOE_VOICE_SECRET_MAP", "ZOE_VOICE_ACCOUNT_MEDIA_ENABLED":
					continue
				}
			}
			if strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	lines = append(lines, "ZOE_VOICE_GATEWAY_URL="+strconv.Quote(baseURL), "ZOE_VOICE_GATEWAY_API_KEY_FILE="+strconv.Quote(secretPath), "ZOE_VOICE_SECRET_MAP=,ZOE_VOICE_GATEWAY_API_KEY=/run/secrets/voice_gateway_api_key")
	return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0600)
}
