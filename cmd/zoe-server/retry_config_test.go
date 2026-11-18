package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/steward"
	"github.com/ronigooja/Zoe/internal/voice"
)

func TestRetryModelsFromLoaded(t *testing.T) {
	env := map[string]string{}
	for _, prefix := range []string{"ZOE_REGISTER", "ZOE_COMPACT", "ZOE_STEWARD", "ZOE_UI_CONTROLLER"} {
		path := filepath.Join(t.TempDir(), "credential")
		if err := os.WriteFile(path, []byte("first-fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
		env[prefix+"_BASE_URL"] = "https://example.invalid/"
		env[prefix+"_MODEL"] = "fixture-model"
		env[prefix+"_API_KEY_FILE"] = path
		env[prefix+"_REASONING_EFFORT"] = "low"
		env[prefix+"_THINKING"] = "disabled"
	}
	getenv := func(key string) string { return env[key] }
	load := func() map[string]voice.RetryModelConfig {
		register, err := registerConfigFromEnv(getenv, os.ReadFile)
		if err != nil || register == nil {
			t.Fatalf("register: %v", err)
		}
		st, err := steward.FromEnv(getenv, os.ReadFile)
		if err != nil || st == nil {
			t.Fatalf("steward: %v", err)
		}
		ui, err := uiControllerFromEnv(getenv, os.ReadFile)
		if err != nil || ui == nil {
			t.Fatalf("ui: %v", err)
		}
		return retryModelsFromLoaded(register, st, ui)
	}
	fingerprint := func(cfg voice.RetryModelConfig) string {
		return backoff.Fingerprint(cfg.Endpoint, cfg.Model, cfg.Effort, cfg.Thinking, cfg.Credential)
	}
	loaded := load()
	for consumer, prefix := range map[string]string{"register": "ZOE_REGISTER", "compact": "ZOE_COMPACT", "steward": "ZOE_STEWARD", "ui_controller": "ZOE_UI_CONTROLLER"} {
		t.Run(consumer, func(t *testing.T) {
			cfg := loaded[consumer]
			if cfg.Endpoint == "" || cfg.Model == "" || cfg.Effort == "" || cfg.Thinking == "" || len(cfg.Credential) == 0 {
				t.Fatal("missing configuration")
			}
			initial := fingerprint(cfg)
			path := env[prefix+"_API_KEY_FILE"]
			if err := os.WriteFile(path, []byte("second-fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if fingerprint(loaded[consumer]) != initial {
				t.Fatal("loaded credential changed")
			}
			if fingerprint(load()[consumer]) == initial {
				t.Fatal("different credential not detected")
			}
			if err := os.WriteFile(path, []byte("first-fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if fingerprint(load()[consumer]) != initial {
				t.Fatal("trailing newline changed fingerprint")
			}
		})
	}
}
