package main

import (
	"errors"
	"fmt"
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

func TestRegisterConfigOptionalAndKeyFiles(t *testing.T) {
	for _, prefix := range []string{"ZOE_REGISTER", "ZOE_COMPACT"} {
		for _, suffix := range []string{"BASE_URL", "MODEL", "API_KEY_FILE"} {
			name := prefix + "_" + suffix
			t.Run(name, func(t *testing.T) {
				values := registerEnvFixture()
				delete(values, name)
				var logs []string
				cfg, err := registerConfigForStartup(func(k string) string { return values[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil }, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
				if err != nil || cfg != nil || len(logs) != 1 || logs[0] != "register status=disabled reason=not_configured missing="+name {
					t.Fatalf("missing variable did not disable register: cfg=%v err=%v logs=%v", cfg, err, logs)
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

func TestRegisterStartupOptionalGroups(t *testing.T) {
	all := "ZOE_REGISTER_BASE_URL,ZOE_REGISTER_MODEL,ZOE_REGISTER_API_KEY_FILE,ZOE_COMPACT_BASE_URL,ZOE_COMPACT_MODEL,ZOE_COMPACT_API_KEY_FILE"
	for _, tc := range []struct {
		name    string
		values  map[string]string
		missing string
	}{
		{"neither", map[string]string{}, all},
		{"partial", map[string]string{"ZOE_REGISTER_MODEL": "route"}, "ZOE_REGISTER_BASE_URL,ZOE_REGISTER_API_KEY_FILE,ZOE_COMPACT_BASE_URL,ZOE_COMPACT_MODEL,ZOE_COMPACT_API_KEY_FILE"},
		{"routing_only", map[string]string{"ZOE_REGISTER_BASE_URL": "https://route.invalid", "ZOE_REGISTER_MODEL": "route", "ZOE_REGISTER_API_KEY_FILE": "route-reference"}, "ZOE_COMPACT_BASE_URL,ZOE_COMPACT_MODEL,ZOE_COMPACT_API_KEY_FILE"},
		{"enabled", registerEnvFixture(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			cfg, err := registerConfigForStartup(func(k string) string { return tc.values[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil }, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
			if err != nil || (cfg == nil) != (tc.missing != "") {
				t.Fatalf("cfg=%v err=%v", cfg, err)
			}
			if tc.missing == "" {
				if len(logs) != 0 {
					t.Fatal(logs)
				}
				return
			}
			if len(logs) != 1 || logs[0] != "register status=disabled reason=not_configured missing="+tc.missing {
				t.Fatal(logs)
			}
		})
	}
}

func TestRegisterDisabledValidatesExplicitSettings(t *testing.T) {
	for _, name := range []string{"ZOE_REGISTER_TIMEOUT_MS", "ZOE_COMPACT_TIMEOUT_MS", "ZOE_REGISTER_REASONING_EFFORT", "ZOE_COMPACT_THINKING", "ZOE_COMPACT_MAX_CHARACTERS", "ZOE_COMPACT_INTERVAL_MINUTES", "ZOE_REGISTER_RAW_WINDOW_MINUTES"} {
		t.Run(name, func(t *testing.T) {
			_, err := registerConfigFromEnv(func(k string) string {
				if k == name {
					return "invalid"
				}
				return ""
			}, func(string) ([]byte, error) { t.Fatal("unexpected file read"); return nil, nil })
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("invalid setting not named: %v", err)
			}
		})
	}
	for _, prefix := range []string{"ZOE_REGISTER", "ZOE_COMPACT"} {
		values := map[string]string{prefix + "_BASE_URL": "https://model.invalid", prefix + "_MODEL": "model", prefix + "_API_KEY_FILE": "unreadable"}
		_, err := registerConfigFromEnv(func(k string) string { return values[k] }, func(string) ([]byte, error) { return nil, errors.New("private") })
		if err == nil || !strings.Contains(err.Error(), prefix+"_API_KEY_FILE") {
			t.Fatalf("bad configured key not named: %v", err)
		}
	}
}

func TestRegisterConfigDefaultsAndIndependentModels(t *testing.T) {
	values := registerEnvFixture()
	values["ZOE_TRIAGE_BASE_URL"] = "invalid-obsolete-endpoint"
	values["ZOE_TRIAGE_TIMEOUT_MS"] = "not-a-number"
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
