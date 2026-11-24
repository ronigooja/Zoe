package limits

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedLoaderCases(t *testing.T) {
	defaultsPath := filepath.Join("..", "..", "deploy", "limits.json")
	shipped, err := Load(defaultsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if shipped.Int64(key) != Defaults().Int64(key) {
			t.Fatalf("default mismatch: %s", key)
		}
	}
	source, err := os.ReadFile(defaultsPath)
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Key, Raw, Hex, Append, Value, Omit, Error string
		Size                                          int
	}
	if err := json.Unmarshal(fixtures, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			data := source
			if tc.Raw != "" {
				data = []byte(tc.Raw)
			}
			if tc.Hex != "" {
				data, _ = hex.DecodeString(tc.Hex)
			}
			if tc.Size > 0 {
				data = bytes.Repeat([]byte{' '}, tc.Size)
			}
			if tc.Value != "" || tc.Omit != "" {
				key := tc.Key
				if key == "" {
					key = "opening_context_runes"
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(source, &fields); err != nil {
					t.Fatal(err)
				}
				if tc.Omit != "" {
					delete(fields, tc.Omit)
				} else {
					fields[key] = json.RawMessage(tc.Value)
				}
				data, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err := Decode(append(bytes.Clone(data), tc.Append...))
			if tc.Error == "" && err != nil || tc.Error != "" && (err == nil || err.Error() != tc.Error) {
				t.Fatalf("got %v; want %q", err, tc.Error)
			}
		})
	}
}

func TestLoadPathAndReadFailures(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "limits.json")
	t.Setenv("ZOE_LIMITS_FILE", file)
	if Path() != file {
		t.Fatal(Path())
	}
	if _, err := Load(file); err != Error("limits_missing") {
		t.Fatal(err)
	}
	if _, err := Load(root); err != Error("limits_unreadable") {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err != Error("limits_unreadable") {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err != Error("limits_empty") {
		t.Fatal(err)
	}
}

func TestRejectLegacyEnvEvenWhenEmpty(t *testing.T) {
	for env, key := range map[string]string{
		"ZOE_COMPACT_MAX_CHARACTERS": "compact_max_characters", "ZOE_MAX_RUNS": "max_concurrent_runs",
		"ZOE_UI_CONTROLLER_MAX_RETRIES": "ui_output_attempts", "ZOE_AGENT_CONTEXT_TOKEN_BUDGET": "runner_context_max_tokens",
		"ZOE_AGENT_MAX_ROTATIONS": "runner_max_rotations",
	} {
		err := RejectLegacyEnv(func(name string) (string, bool) { return "", name == env })
		if err == nil || !strings.Contains(err.Error(), env) || !strings.Contains(err.Error(), key) {
			t.Fatal(err)
		}
	}
}
