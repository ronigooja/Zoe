package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/steward"
)

func TestRetryModelsFromEnv(t *testing.T) {
	env := map[string]string{}
	for _, prefix := range []string{"ZOE_REGISTER", "ZOE_COMPACT", "ZOE_STEWARD", "ZOE_UI_CONTROLLER"} {
		path := filepath.Join(t.TempDir(), "credential")
		if err := os.WriteFile(path, []byte("first-fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		env[prefix+"_BASE_URL"] = "https://example.invalid/"
		env[prefix+"_MODEL"] = "fixture-model"
		env[prefix+"_API_KEY_FILE"] = path
		env[prefix+"_REASONING_EFFORT"] = "low"
		env[prefix+"_THINKING"] = "disabled"
	}
	getenv := func(key string) string { return env[key] }
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
	models := retryModelsFromEnv(getenv, register, st != nil, ui != nil)
	for _, consumer := range []string{"register", "compact", "steward", "ui_controller"} {
		t.Run(consumer, func(t *testing.T) {
			cfg, ok := models[consumer]
			if !ok || cfg.Endpoint == "" || cfg.Model == "" || cfg.Effort == "" || cfg.Thinking == "" || cfg.CredentialFile == "" {
				t.Fatal("missing configuration")
			}
			first, err := os.ReadFile(cfg.CredentialFile)
			if err != nil {
				t.Fatal(err)
			}
			a := backoff.Fingerprint(cfg.Endpoint, cfg.Model, cfg.Effort, cfg.Thinking, first)
			if err = os.WriteFile(cfg.CredentialFile, []byte("second-fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			second, err := os.ReadFile(cfg.CredentialFile)
			if err != nil {
				t.Fatal(err)
			}
			if a == backoff.Fingerprint(cfg.Endpoint, cfg.Model, cfg.Effort, cfg.Thinking, second) {
				t.Fatal("credential change not observed")
			}
		})
	}
}
