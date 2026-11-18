package main

import (
	"strings"

	"github.com/ronigooja/Zoe/internal/voice"
)

func retryModelsFromEnv(getenv func(string) string, register *voice.RegisterConfig, steward, ui bool) map[string]voice.RetryModelConfig {
	models := make(map[string]voice.RetryModelConfig)
	for consumer, prefix := range map[string]string{"register": "ZOE_REGISTER", "compact": "ZOE_COMPACT", "steward": "ZOE_STEWARD", "ui_controller": "ZOE_UI_CONTROLLER"} {
		if (consumer == "register" || consumer == "compact") && register == nil || consumer == "steward" && !steward || consumer == "ui_controller" && !ui {
			continue
		}
		cfg := voice.RetryModelConfig{Endpoint: strings.TrimRight(strings.TrimSpace(getenv(prefix+"_BASE_URL")), "/"), Model: strings.TrimSpace(getenv(prefix + "_MODEL")), Effort: strings.TrimSpace(getenv(prefix + "_REASONING_EFFORT")), Thinking: strings.TrimSpace(getenv(prefix + "_THINKING")), CredentialFile: strings.TrimSpace(getenv(prefix + "_API_KEY_FILE"))}
		if consumer == "register" || consumer == "compact" {
			loaded := register.Model
			if consumer == "compact" {
				loaded = register.CompactModel
			}
			cfg.Endpoint = strings.TrimRight(loaded.BaseURL, "/")
			cfg.Model, cfg.Effort, cfg.Thinking = loaded.Model, loaded.ReasoningEffort, loaded.Thinking
		}
		models[consumer] = cfg
	}
	return models
}
