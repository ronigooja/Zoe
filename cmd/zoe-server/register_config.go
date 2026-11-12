package main

import (
	"errors"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/voice"
)

// registerConfigFromEnv applies field-by-field Steward fallback to unset register settings.
func registerConfigFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (*voice.RegisterConfig, error) {
	get := func(name string) string {
		if value := strings.TrimSpace(getenv(name)); value != "" {
			return value
		}
		return strings.TrimSpace(getenv(strings.Replace(name, "ZOE_REGISTER_", "ZOE_STEWARD_", 1)))
	}
	timeout, err := modelclient.Timeout("ZOE_REGISTER", get, 30000, 500, 60000)
	if err != nil {
		return nil, err
	}
	base, model, path := get("ZOE_REGISTER_BASE_URL"), get("ZOE_REGISTER_MODEL"), get("ZOE_REGISTER_API_KEY_FILE")
	if base == "" && model == "" && path == "" {
		return nil, nil
	}
	if base == "" || model == "" || path == "" {
		return nil, errors.New("register requires BASE_URL, MODEL and API_KEY_FILE")
	}
	key, err := readFile(path)
	if err != nil {
		return nil, errors.New("could not read register API key file")
	}
	return &voice.RegisterConfig{Model: voice.RegisterModelConfig{BaseURL: base, Model: model, APIKey: strings.TrimSpace(string(key)), ReasoningEffort: get("ZOE_REGISTER_REASONING_EFFORT"), Timeout: timeout}, CompactCharacters: 8000, Interval: time.Hour}, nil
}
