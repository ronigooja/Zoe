package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/voice"
)

// registerConfigFromEnv requires independently configured routing and compaction models.
func registerConfigFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (*voice.RegisterConfig, error) {
	load := func(prefix string, def, min, max int) (voice.RegisterModelConfig, error) {
		var cfg voice.RegisterModelConfig
		for _, suffix := range []string{"BASE_URL", "MODEL", "API_KEY_FILE"} {
			if strings.TrimSpace(getenv(prefix+"_"+suffix)) == "" {
				return cfg, fmt.Errorf("%s_%s is required", prefix, suffix)
			}
		}
		key, err := readFile(strings.TrimSpace(getenv(prefix + "_API_KEY_FILE")))
		if err != nil {
			return cfg, fmt.Errorf("%s_API_KEY_FILE could not be read", prefix)
		}
		if strings.TrimSpace(string(key)) == "" {
			return cfg, fmt.Errorf("%s_API_KEY_FILE is empty", prefix)
		}
		if _, err := modelclient.FromEnv(prefix, getenv, func(string) ([]byte, error) { return key, nil }, def, min, max); err != nil {
			return cfg, fmt.Errorf("%s model configuration: %w", prefix, err)
		}
		timeout, err := modelclient.Timeout(prefix, getenv, def, min, max)
		return voice.RegisterModelConfig{BaseURL: strings.TrimSpace(getenv(prefix + "_BASE_URL")), Model: strings.TrimSpace(getenv(prefix + "_MODEL")), APIKey: strings.TrimSpace(string(key)), ReasoningEffort: strings.TrimSpace(getenv(prefix + "_REASONING_EFFORT")), Thinking: strings.TrimSpace(getenv(prefix + "_THINKING")), Timeout: timeout}, err
	}
	model, err := load("ZOE_REGISTER", 30000, 500, 60000)
	if err != nil {
		return nil, err
	}
	compact, err := load("ZOE_COMPACT", 600000, 5000, 600000)
	if err != nil {
		return nil, err
	}
	integer := func(name string, def, min, max int) (int, error) {
		if raw := strings.TrimSpace(getenv(name)); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < min || value > max {
				return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
			}
			return value, nil
		}
		return def, nil
	}
	limit, err := integer("ZOE_COMPACT_MAX_CHARACTERS", 8000, 500, 100000)
	if err != nil {
		return nil, err
	}
	interval, err := integer("ZOE_COMPACT_INTERVAL_MINUTES", 60, 1, 1440)
	if err != nil {
		return nil, err
	}
	window, err := integer("ZOE_REGISTER_RAW_WINDOW_MINUTES", 60, 5, 1440)
	if err != nil {
		return nil, err
	}
	return &voice.RegisterConfig{Model: model, CompactModel: compact, CompactCharacters: limit, Interval: time.Duration(interval) * time.Minute, RawWindow: time.Duration(window) * time.Minute}, nil
}
