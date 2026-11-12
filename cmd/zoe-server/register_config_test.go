package main

import (
	"strings"
	"testing"
	"time"
)

func TestRegisterConfigFallbackAndValidation(t *testing.T) {
	for _, raw := range []string{"", "500", "60000", "499", "60001", "invalid"} {
		t.Run(raw, func(t *testing.T) {
			values := map[string]string{"ZOE_STEWARD_BASE_URL": "https://model.invalid", "ZOE_STEWARD_MODEL": "steward", "ZOE_STEWARD_API_KEY_FILE": "secret-reference", "ZOE_REGISTER_TIMEOUT_MS": raw, "ZOE_TRIAGE_TIMEOUT_MS": "invalid-unused"}
			cfg, err := registerConfigFromEnv(func(k string) string { return values[k] }, func(path string) ([]byte, error) {
				if path != "secret-reference" {
					t.Fatal("wrong secret reference")
				}
				return []byte("fixture"), nil
			})
			bad := raw == "499" || raw == "60001" || raw == "invalid"
			if bad {
				if err == nil {
					t.Fatal("invalid timeout accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Model.Model != "steward" || cfg.Model.APIKey != "fixture" {
				t.Fatal("fallback failed")
			}
			if raw == "" && cfg.Model.Timeout != 30*time.Second {
				t.Fatal("default timeout")
			}
			values["ZOE_REGISTER_MODEL"] = "register"
			values["ZOE_STEWARD_TIMEOUT_MS"] = "15000"
			cfg, err = registerConfigFromEnv(func(k string) string { return values[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil })
			if err != nil || cfg.Model.Model != "register" {
				t.Fatal("register override failed")
			}
			if raw == "" && cfg.Model.Timeout != 15*time.Second {
				t.Fatal("timeout fallback failed")
			}
			values["ZOE_REGISTER_BASE_URL"] = ""
			values["ZOE_STEWARD_BASE_URL"] = ""
			_, err = registerConfigFromEnv(func(k string) string { return values[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil })
			if err == nil || !strings.Contains(err.Error(), "requires") {
				t.Fatal("partial config accepted")
			}
		})
	}
}
