package ops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderReasoningEffortRoundTripAndClear(t *testing.T) {
	d := testDeployment(t)
	c := ProviderConfig{BaseURL: "https://example.test", Model: "p/model", SecretPath: filepath.Join(d.Root, "key")}
	for _, effort := range []string{"", "none", "low", "medium", "high", "max", ""} {
		c.ReasoningEffort = effort
		if err := WriteRunnerEnvAtomic(d.RunnerEnv, c); err != nil {
			t.Fatal(err)
		}
		got, err := d.ProviderConfig()
		if err != nil || got.ReasoningEffort != effort {
			t.Fatalf("effort=%q got=%q err=%v", effort, got.ReasoningEffort, err)
		}
		b, err := os.ReadFile(d.RunnerEnv)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "ZOE_OPENCODE_REASONING_EFFORT=") != (effort != "") {
			t.Fatal("effort key presence mismatch")
		}
	}
	c.ReasoningEffort = "invalid"
	before, _ := os.ReadFile(d.RunnerEnv)
	if err := WriteRunnerEnvAtomic(d.RunnerEnv, c); err == nil {
		t.Fatal("accepted invalid effort")
	}
	after, _ := os.ReadFile(d.RunnerEnv)
	if string(before) != string(after) {
		t.Fatal("invalid effort changed environment")
	}
	if _, err := c.Normalized(); err == nil {
		t.Fatal("normalization accepted invalid effort")
	}
	r := Probe(context.Background(), c, nil)
	if r.ErrorCategory != "invalid_configuration" {
		t.Fatal("probe accepted invalid effort")
	}
}

func TestOpenCodeReasoningEffortSelectedModelAndFingerprint(t *testing.T) {
	o, _ := catalogOptions(t)
	o.OpenCodeBinary = "/fixture/opencode"
	o.OptRoot = filepath.Join(o.Root, "opt")
	if err := persistProviderModels(o, "p", []ProviderModelMetadata{{ID: "model", Name: "Selected", Limit: ModelLimit{Context: 100}}, {ID: "other", Name: "Other"}}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(func(context.Context, string, ...string) ([]byte, error) { return []byte("opencode v2.2.5"), nil })
	var previous []fileSnapshot
	for _, effort := range []string{"", "none", "low", "medium", "high", "max", ""} {
		c := ProviderConfig{BaseURL: "https://example.test", Model: "p/model", ReasoningEffort: effort, SecretPath: filepath.Join(o.Root, "key")}
		if err := WriteRunnerEnvAtomic(filepath.Join(o.Root, "zoe-runner.env"), c); err != nil {
			t.Fatal(err)
		}
		if err := renderOpenCodeConfig(o); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(openCodeConfigPath(o))
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(b, &cfg); err != nil {
			t.Fatal(err)
		}
		models := cfg["providers"].(map[string]any)["p"].(map[string]any)["models"].(map[string]any)
		selected := models["model"].(map[string]any)
		if selected["name"] != "Selected" || selected["limit"].(map[string]any)["context"] != float64(100) {
			t.Fatal("metadata lost")
		}
		if effort == "" {
			if _, ok := selected["settings"]; ok {
				t.Fatal("default must omit settings")
			}
		} else if selected["settings"].(map[string]any)["reasoningEffort"] != effort {
			t.Fatal("wrong effort")
		}
		if _, ok := models["other"].(map[string]any)["settings"]; ok {
			t.Fatal("effort affected another model")
		}
		if _, err := m.renderOpenCodeUnit(context.Background(), o, []byte("[Service]\nUser=root\nGroup=root\n")); err != nil {
			t.Fatal(err)
		}
		if previous != nil {
			changed, err := openCodeChanged(previous)
			if err != nil || !changed {
				t.Fatalf("effort-only change not detected: %v", err)
			}
		}
		previous, err = snapshotOpenCode(o)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestProviderReasoningEffortPreservedByDeploymentUpdate(t *testing.T) {
	o, _ := catalogOptions(t)
	path := filepath.Join(o.Root, "zoe-runner.env")
	if err := WriteRunnerEnvAtomic(path, ProviderConfig{BaseURL: "https://example.test", Model: "p/model", ReasoningEffort: "high", SecretPath: filepath.Join(o.Root, "key")}); err != nil {
		t.Fatal(err)
	}
	manifest := ReleaseManifest{Images: map[string]ReleaseImage{
		"zoe-server":        {Ref: "ghcr.io/test/server", Digest: "sha256:" + strings.Repeat("1", 64)},
		"zoe-memory-worker": {Ref: "ghcr.io/test/memory", Digest: "sha256:" + strings.Repeat("2", 64)},
	}}
	if err := (Manager{}).writeDeploymentFiles(o, manifest); err != nil {
		t.Fatal(err)
	}
	got, err := (Deployment{RunnerEnv: path}).ProviderConfig()
	if err != nil || got.ReasoningEffort != "high" {
		t.Fatalf("update lost effort: %q %v", got.ReasoningEffort, err)
	}
	if !isOperatorOwnedEnvKey("ZOE_OPENCODE_REASONING_EFFORT") || !validOperatorEnvValue("ZOE_OPENCODE_REASONING_EFFORT", `"none"`) || validOperatorEnvValue("ZOE_OPENCODE_REASONING_EFFORT", "invalid") {
		t.Fatal("effort whitelist validation mismatch")
	}
}

func TestProviderEffortOnlyChangeRendersAndRestartsOpenCode(t *testing.T) {
	d := testDeployment(t)
	o := LifecycleOptions{Root: d.Root, DataRoot: d.Root, OptRoot: "/opt/zoe", OpenCodeBinary: "/fixture/opencode"}
	server := modelServer(t)
	catalog := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer catalog.Close()
	t.Setenv("ZOE_OPENCODE_OFFICIAL_CATALOG_URL", catalog.URL)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "127.0.0.1:4096" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{}}`)), Header: make(http.Header)}, nil
		}
		return catalog.Client().Transport.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	c := ProviderConfig{BaseURL: server.URL, Model: "p/test-model", ReasoningEffort: "low", SecretPath: filepath.Join(d.Root, "key"), Secret: "next"}
	if err := WriteRunnerEnvAtomic(d.RunnerEnv, c); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(d.RunnerEnv, append(mustReadWorkerFile(t, d.RunnerEnv), []byte("ZOE_RUNNER_JOURNAL_PATH="+filepath.Join(d.Root, "runner", "journal.sqlite")+"\nZOE_OPENCODE_BINARY=/fixture/opencode\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renderOpenCodeConfig(o); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(openCodeRoot(o), "password"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	unit := []byte("[Service]\nUser=root\nGroup=root\n")
	if err := writeFileAtomic(filepath.Join(openCodeUnitDir(o), "zoe-opencode.service"), unit, 0644); err != nil {
		t.Fatal(err)
	}
	var commands []string
	m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == o.OpenCodeBinary {
			return []byte("opencode v2.2.5"), nil
		}
		return nil, nil
	})
	if _, err := m.renderOpenCodeUnit(context.Background(), o, unit); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotOpenCode(o)
	if err != nil {
		t.Fatal(err)
	}
	c.ReasoningEffort = "high"
	if _, err := m.ConfigureProvider(context.Background(), d, c, server.Client()); err != nil {
		t.Fatal(err)
	}
	changed, err := openCodeChanged(before)
	if err != nil || !changed {
		t.Fatalf("effort-only change did not alter fingerprint: %v", err)
	}
	if strings.Count(strings.Join(commands, "\n"), "systemctl restart zoe-opencode.service") != 1 {
		t.Fatal("effort-only change did not restart OpenCode exactly once")
	}
	if !strings.Contains(string(mustReadWorkerFile(t, openCodeConfigPath(o))), `"reasoningEffort": "high"`) {
		t.Fatal("effort-only change was not rendered")
	}
}

func mustReadWorkerFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
