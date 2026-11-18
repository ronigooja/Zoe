package main

import (
	"strings"

	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/steward"
	"github.com/ronigooja/Zoe/internal/voice"
)

func retryModelsFromLoaded(register *voice.RegisterConfig, st *steward.Client, ui *modelclient.Client) map[string]voice.RetryModelConfig {
	models := make(map[string]voice.RetryModelConfig)
	if register != nil {
		for consumer, cfg := range map[string]voice.RegisterModelConfig{"register": register.Model, "compact": register.CompactModel} {
			models[consumer] = voice.RetryModelConfig{Endpoint: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"), Model: strings.TrimSpace(cfg.Model), Effort: strings.TrimSpace(cfg.ReasoningEffort), Thinking: strings.TrimSpace(cfg.Thinking), Credential: []byte(strings.TrimSpace(cfg.APIKey))}
		}
	}
	if st != nil {
		endpoint, model, effort, thinking, key := st.FingerprintConfig()
		models["steward"] = voice.RetryModelConfig{Endpoint: endpoint, Model: model, Effort: effort, Thinking: thinking, Credential: key}
	}
	if ui != nil {
		endpoint, model, effort, thinking, key := ui.FingerprintConfig()
		models["ui_controller"] = voice.RetryModelConfig{Endpoint: endpoint, Model: model, Effort: effort, Thinking: thinking, Credential: key}
	}
	return models
}
