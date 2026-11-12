package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func registerEnvFixture() map[string]string {
	return map[string]string{
		"ZOE_REGISTER_BASE_URL": "https://route.invalid", "ZOE_REGISTER_MODEL": "route", "ZOE_REGISTER_API_KEY_FILE": "route-reference",
		"ZOE_COMPACT_BASE_URL": "https://compact.invalid", "ZOE_COMPACT_MODEL": "compact", "ZOE_COMPACT_API_KEY_FILE": "compact-reference",
		"ZOE_STEWARD_BASE_URL": "https://unused.invalid", "ZOE_STEWARD_MODEL": "unused", "ZOE_STEWARD_API_KEY_FILE": "unused-reference", "ZOE_STEWARD_TIMEOUT_MS": "invalid",
	}
}

func TestRegisterConfigRequiredAndKeyFiles(t *testing.T) {
	for _, prefix := range []string{"ZOE_REGISTER", "ZOE_COMPACT"} {
		for _, suffix := range []string{"BASE_URL", "MODEL", "API_KEY_FILE"} {
			name := prefix + "_" + suffix
			t.Run(name, func(t *testing.T) {
				values := registerEnvFixture()
				delete(values, name)
				_, err := registerConfigFromEnv(func(k string) string { return values[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil })
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("missing variable not named: %v", err)
				}
			})
		}
		for _, mode := range []string{"unreadable", "empty"} {
			t.Run(prefix+mode, func(t *testing.T) {
				values := registerEnvFixture()
				name := prefix + "_API_KEY_FILE"
				_, err := registerConfigFromEnv(func(k string) string { return values[k] }, func(path string) ([]byte, error) {
					if path == values[name] {
						if mode == "unreadable" {
							return nil, errors.New("PRIVATE_PATH_ERROR")
						}
						return []byte(" \n"), nil
					}
					return []byte("fixture"), nil
				})
				if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "PRIVATE_PATH_ERROR") {
					t.Fatalf("unsafe or missing error: %v", err)
				}
			})
		}
	}
}

func TestRegisterStartupFailsBeforeOpeningDatabase(t *testing.T) {
	values := registerEnvFixture()
	key := t.TempDir() + "/key"
	if err := os.WriteFile(key, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"ZOE_REGISTER", "ZOE_COMPACT"} {
		values[prefix+"_API_KEY_FILE"] = key
	}
	for name := range values {
		if strings.HasPrefix(name, "ZOE_STEWARD_") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			for k, v := range values {
				t.Setenv(k, v)
			}
			t.Setenv(name, "")
			t.Setenv("ZOE_DATABASE_PATH", "/nonexistent/register-startup/db")
			if err := run(); err == nil || !strings.Contains(err.Error(), "register configuration: "+name+" is required") {
				t.Fatalf("startup did not fail by variable name: %v", err)
			}
		})
	}
}

func TestRegisterConfigDefaultsAndIndependentModels(t *testing.T) {
	values := registerEnvFixture()
	cfg, err := registerConfigFromEnv(func(k string) string { return values[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Model != "route" || cfg.CompactModel.Model != "compact" || cfg.Model.Timeout != 30*time.Second || cfg.CompactModel.Timeout != 10*time.Minute || cfg.RawWindow != time.Hour || cfg.Interval != time.Hour || cfg.CompactCharacters != 8000 {
		t.Fatal("defaults or independent settings changed")
	}
	for _, prefix := range []string{"ZOE_REGISTER", "ZOE_COMPACT"} {
		values[prefix+"_REASONING_EFFORT"] = "high"
		values[prefix+"_THINKING"] = "disabled"
	}
	cfg, err = registerConfigFromEnv(func(k string) string { return values[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil })
	if err != nil || cfg.Model.Thinking != "disabled" || cfg.CompactModel.Thinking != "disabled" || cfg.Model.ReasoningEffort != "high" || cfg.CompactModel.ReasoningEffort != "high" {
		t.Fatal("thinking/effort not propagated")
	}
}

func TestRegisterConfigRanges(t *testing.T) {
	for _, field := range []struct {
		name     string
		min, max int
	}{
		{"ZOE_REGISTER_TIMEOUT_MS", 500, 60000}, {"ZOE_COMPACT_TIMEOUT_MS", 5000, 600000},
		{"ZOE_COMPACT_MAX_CHARACTERS", 500, 100000}, {"ZOE_COMPACT_INTERVAL_MINUTES", 1, 1440}, {"ZOE_REGISTER_RAW_WINDOW_MINUTES", 5, 1440},
	} {
		for _, raw := range []string{strconv.Itoa(field.min), strconv.Itoa(field.max), strconv.Itoa(field.min - 1), strconv.Itoa(field.max + 1), "invalid"} {
			t.Run(field.name+raw, func(t *testing.T) {
				values := registerEnvFixture()
				values[field.name] = raw
				cfg, err := registerConfigFromEnv(func(k string) string { return values[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil })
				bad := raw != strconv.Itoa(field.min) && raw != strconv.Itoa(field.max)
				if bad {
					if err == nil || !strings.Contains(err.Error(), field.name) {
						t.Fatalf("invalid range not named: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				value, _ := strconv.Atoi(raw)
				switch field.name {
				case "ZOE_REGISTER_TIMEOUT_MS":
					if cfg.Model.Timeout != time.Duration(value)*time.Millisecond {
						t.Fatal("routing timeout ignored")
					}
				case "ZOE_COMPACT_TIMEOUT_MS":
					if cfg.CompactModel.Timeout != time.Duration(value)*time.Millisecond {
						t.Fatal("compact timeout ignored")
					}
				case "ZOE_COMPACT_MAX_CHARACTERS":
					if cfg.CompactCharacters != value {
						t.Fatal("limit ignored")
					}
				case "ZOE_COMPACT_INTERVAL_MINUTES":
					if cfg.Interval != time.Duration(value)*time.Minute {
						t.Fatal("interval ignored")
					}
				case "ZOE_REGISTER_RAW_WINDOW_MINUTES":
					if cfg.RawWindow != time.Duration(value)*time.Minute {
						t.Fatal("window ignored")
					}
				}
			})
		}
	}
}
