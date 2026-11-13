package logging

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	writer, level := log.Writer(), CurrentLevel()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(writer); SetLevel(level) })
	return &out
}

func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		value string
		level Level
		valid bool
	}{
		{"", Debug, true}, {"debug", Debug, true}, {"INFO", Info, true},
		{" warn ", Warn, true}, {"error", Error, true}, {"invalid", Debug, false},
	} {
		level, valid := ParseLevel(tc.value)
		if level != tc.level || valid != tc.valid {
			t.Errorf("ParseLevel(%q)=%v,%v", tc.value, level, valid)
		}
	}
}

func TestEnvironmentAndFiltering(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", ""} {
		t.Run(level, func(t *testing.T) {
			out := capture(t)
			t.Setenv("ZOE_LOG_LEVEL", level)
			ConfigureFromEnv()
			Debugf("debug-event")
			Infof("info-event")
			Warnf("warn-event")
			Errorf("error-event")
			threshold, _ := ParseLevel(level)
			for l, event := range []string{"debug-event", "info-event", "warn-event", "error-event"} {
				if strings.Contains(out.String(), event) != (Level(l) >= threshold) {
					t.Errorf("incorrect filtering: %s", out)
				}
			}
		})
	}
	t.Run("invalid", func(t *testing.T) {
		out := capture(t)
		t.Setenv("ZOE_LOG_LEVEL", "private-invalid-value")
		ConfigureFromEnv()
		if CurrentLevel() != Debug || !strings.Contains(out.String(), "status=invalid_level default=debug") || strings.Contains(out.String(), "private-invalid-value") {
			t.Fatal("unsafe or missing fallback diagnostic")
		}
	})
}

func TestModelCallFullInputAndLevelBoundary(t *testing.T) {
	for _, level := range []Level{Debug, Info, Warn, Error} {
		out := capture(t)
		SetLevel(level)
		input := strings.Repeat("x", 20000)
		call := NewModelCall("register", UserHash("private-owner"), 1, "")
		call.Input("full instructions", input)
		call.Output("raw model output")
		call.Result("parsed result", "none")
		if level == Debug {
			for _, text := range []string{input, "full instructions", "raw model output", "parsed result", "model_call_input", "model_call_output", "model_call_result"} {
				if !strings.Contains(out.String(), text) {
					t.Errorf("missing complete debug field (%d characters)", len(text))
				}
			}
		} else if out.Len() != 0 {
			t.Fatal("content outside DEBUG")
		}
		if strings.Contains(out.String(), "private-owner") {
			t.Fatal("raw owner logged")
		}
	}
}
