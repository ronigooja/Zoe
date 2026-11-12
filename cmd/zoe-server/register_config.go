package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/voice"
)

func registerMissingEnv(getenv func(string) string) []string {
	var missing []string
	for _, prefix := range []string{"ZOE_REGISTER", "ZOE_COMPACT"} {
		for _, suffix := range []string{"BASE_URL", "MODEL", "API_KEY_FILE"} {
			name := prefix + "_" + suffix
			if strings.TrimSpace(getenv(name)) == "" {
				missing = append(missing, name)
			}
		}
	}
	return missing
}

func registerConfigForStartup(getenv func(string) string, readFile func(string) ([]byte, error), logf func(string, ...any)) (*voice.RegisterConfig, error) {
	cfg, err := registerConfigFromEnv(getenv, readFile)
	if err != nil {
		return nil, fmt.Errorf("register configuration: %w", err)
	}
	if cfg == nil {
		logf("register status=disabled reason=not_configured missing=%s", strings.Join(registerMissingEnv(getenv), ","))
	}
	return cfg, nil
}

// registerConfigFromEnv enables the register only when both model groups are complete.
func registerConfigFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (*voice.RegisterConfig, error) {
	load := func(prefix string, def, min, max int) (voice.RegisterModelConfig, error) {
		var cfg voice.RegisterModelConfig
		if _, err := modelclient.Timeout(prefix, getenv, def, min, max); err != nil {
			return cfg, err
		}
		if effort := strings.TrimSpace(getenv(prefix + "_REASONING_EFFORT")); effort != "" && effort != "none" && effort != "low" && effort != "medium" && effort != "high" && effort != "max" {
			return cfg, fmt.Errorf("%s_REASONING_EFFORT must be none, low, medium, high, max, or empty", prefix)
		}
		if thinking := strings.TrimSpace(getenv(prefix + "_THINKING")); thinking != "" && thinking != "enabled" && thinking != "disabled" {
			return cfg, fmt.Errorf("%s_THINKING must be enabled, disabled, or empty", prefix)
		}
		for _, suffix := range []string{"BASE_URL", "MODEL", "API_KEY_FILE"} {
			if strings.TrimSpace(getenv(prefix+"_"+suffix)) == "" {
				return cfg, nil
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
	if len(registerMissingEnv(getenv)) != 0 {
		return nil, nil
	}
	return &voice.RegisterConfig{Model: model, CompactModel: compact, CompactCharacters: limit, Interval: time.Duration(interval) * time.Minute, RawWindow: time.Duration(window) * time.Minute}, nil
}
