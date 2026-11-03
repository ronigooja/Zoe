package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderedOpenCodeConfigHasProfilesAndNoWildcardAllow(t *testing.T) {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "system"), 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(d, "root")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-runner.env"), []byte("ZOE_OPENCODE_MODEL=provider/model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	o := LifecycleOptions{Root: root, DataRoot: d}
	if err := renderOpenCodeConfig(o); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(d, "opencode", "xdg-config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	agents := cfg["agents"].(map[string]any)
	for _, id := range []string{"zoe-worker", "zoe-steward", "zoe-super"} {
		if agents[id].(map[string]any)["system"] == "" {
			t.Fatal(id)
		}
	}
	summary := agents["zoe-rotation-summary"].(map[string]any)
	if summary["system"] == "" || summary["model"] != "provider/model" {
		t.Fatalf("rotation summarizer agent=%v", summary)
	}
	if _, exists := summary["permission"]; exists {
		t.Fatalf("rotation summarizer has invalid singular permission key: %v", summary["permission"])
	}
	permissions, ok := summary["permissions"].([]any)
	if !ok || len(permissions) != 1 {
		t.Fatalf("rotation summarizer permissions=%v", summary["permissions"])
	}
	rule, ok := permissions[0].(map[string]any)
	if !ok || rule["action"] != "*" || rule["resource"] != "*" || rule["effect"] != "deny" {
		t.Fatalf("rotation summarizer permissions=%v", permissions)
	}
	for _, id := range []string{"zoe-worker", "zoe-steward", "zoe-super"} {
		if _, exists := agents[id].(map[string]any)["permissions"]; exists {
			t.Fatalf("%s unexpectedly has config permissions", id)
		}
	}
	if _, exists := cfg["permissions"]; exists {
		t.Fatalf("global permissions must be absent: %v", cfg["permissions"])
	}
}

func TestScraplingSettingsPrecedenceAndValidation(t *testing.T) {
	s, err := ParseScraplingSettings(map[string]string{})
	if err != nil || s.Mode != "off" {
		t.Fatalf("off: %#v %v", s, err)
	}
	s, err = ParseScraplingSettings(map[string]string{"ZOE_SCRAPLING_URL": "http://scraper:8000/mcp"})
	if err != nil || s.Mode != "remote" {
		t.Fatalf("implicit remote: %#v %v", s, err)
	}
	for _, raw := range []string{"ftp://host/mcp", "http:///mcp", "http://user:pass@host/mcp"} {
		if _, err := ParseScraplingSettings(map[string]string{"ZOE_SCRAPLING_MODE": "remote", "ZOE_SCRAPLING_URL": raw}); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if _, err := ParseScraplingSettings(map[string]string{"ZOE_SCRAPLING_MODE": "local", "ZOE_SCRAPLING_URL": "http://host/mcp"}); err == nil {
		t.Fatal("local accepted remote URL")
	}
}

func TestRenderedScraplingRemoteUsesFileSubstitutionWithoutToken(t *testing.T) {
	d := t.TempDir()
	root := filepath.Join(d, "root")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	const token = "never-render-this-token"
	if err := os.WriteFile(filepath.Join(root, "token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	env := "ZOE_OPENCODE_MODEL=provider/model\nZOE_SCRAPLING_MODE=remote\nZOE_SCRAPLING_URL=http://scraper:8000/mcp\nZOE_SCRAPLING_TOKEN_FILE=" + filepath.Join(root, "token") + "\n"
	if err := os.WriteFile(filepath.Join(root, "zoe-runner.env"), []byte(env), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renderOpenCodeConfig(LifecycleOptions{Root: root, DataRoot: d, OptRoot: filepath.Join(d, "opt")}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(d, "opencode", "xdg-config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), token) || !strings.Contains(string(b), "{file:"+filepath.Join(root, "token")+"}") {
		t.Fatalf("unsafe config: %s", b)
	}
}

func TestRenderedScraplingLocalStdioAndOff(t *testing.T) {
	d := t.TempDir()
	root := filepath.Join(d, "root")
	opt := filepath.Join(d, "opt")
	bin := filepath.Join(opt, "scrapling", ScraplingVersion, "venv", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "scrapling-mcp"), []byte("x"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-runner.env"), []byte("ZOE_OPENCODE_MODEL=provider/model\nZOE_SCRAPLING_MODE=local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renderOpenCodeConfig(LifecycleOptions{Root: root, DataRoot: d, OptRoot: opt}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(d, "opencode", "xdg-config", "opencode", "opencode.json"))
	if !strings.Contains(string(b), "scrapling-mcp") {
		t.Fatalf("local MCP missing: %s", b)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-runner.env"), []byte("ZOE_OPENCODE_MODEL=provider/model\nZOE_SCRAPLING_MODE=off\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renderOpenCodeConfig(LifecycleOptions{Root: root, DataRoot: d, OptRoot: opt}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(d, "opencode", "xdg-config", "opencode", "opencode.json"))
	if strings.Contains(string(b), "scrapling") {
		t.Fatalf("off still rendered: %s", b)
	}
}

func TestScraplingWheelPinAndTokenFingerprint(t *testing.T) {
	if err := ValidateScraplingWheel(ScraplingVersion, "366f0ec9f1e7e998900e6c5e3a4063ea34062326394ced64e5c389b690572726"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateScraplingWheel(ScraplingVersion, "bad"); err == nil {
		t.Fatal("accepted checksum mismatch")
	}
	d := t.TempDir()
	token := filepath.Join(d, "token")
	if err := os.WriteFile(token, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	s := ScraplingSettings{Mode: "remote", TokenFile: token}
	first := string(scraplingTokenFingerprint(s))
	if first == "" {
		t.Fatal("missing fingerprint")
	}
	if err := os.WriteFile(token, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if first == string(scraplingTokenFingerprint(s)) {
		t.Fatal("token change did not change fingerprint")
	}
}

func TestLocalScraplingInstallerSkipsExistingAndReportsChecksumFailure(t *testing.T) {
	d := t.TempDir()
	o := LifecycleOptions{OptRoot: d}
	bin := filepath.Join(d, "scrapling", ScraplingVersion, "venv", "bin", "scrapling-mcp")
	if err := os.MkdirAll(filepath.Dir(bin), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("installed"), 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m := NewManager(func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, nil })
	if err := m.ensureLocalScrapling(context.Background(), o, map[string]string{"ZOE_SCRAPLING_MODE": "local"}); err != nil || calls != 0 {
		t.Fatalf("existing install err=%v calls=%d", err, calls)
	}
	os.Remove(bin)
	m = NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "dpkg-query" {
			return nil, errors.New("missing")
		}
		if name == "apt-get" && len(args) > 0 && args[0] == "update" {
			return nil, nil
		}
		if name == "env" {
			return nil, nil
		}
		if name == "python3" && len(args) >= 3 && args[0] == "-m" && args[1] == "venv" {
			return nil, nil
		}
		if name == "python3" && len(args) >= 3 && args[1] == "pip" {
			return nil, nil
		}
		if strings.HasSuffix(name, "/pip") && len(args) > 0 && args[0] == "install" {
			return nil, nil
		}
		return nil, nil
	})
	if err := m.ensureLocalScrapling(context.Background(), o, map[string]string{"ZOE_SCRAPLING_MODE": "local"}); err == nil || !strings.Contains(err.Error(), "wheel download missing") {
		t.Fatalf("expected wheel failure, got %v", err)
	}
}

func TestLocalScraplingInstallerFailureIsWarningOnly(t *testing.T) {
	d := t.TempDir()
	o := LifecycleOptions{OptRoot: d, Out: new(strings.Builder)}
	m := NewManager(func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("install failed") })
	if err := m.ensureLocalScrapling(context.Background(), o, map[string]string{"ZOE_SCRAPLING_MODE": "local"}); err == nil {
		t.Fatal("expected installer error")
	}
	if _, _, err := scraplingMCPEntry(ScraplingSettings{Mode: "local"}, d); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeVersionOutput(t *testing.T) {
	tests := []struct {
		name, output string
		valid        bool
	}{
		{"upstream", "opencode v2.2.5", true},
		{"fork", "opencode v2.2.5-ronigooja.2", true},
		{"empty", "", false}, {"garbage", "garbage", false},
		{"multiline", "opencode v2.2.5\nnoise", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(func(context.Context, string, ...string) ([]byte, error) { return []byte(tt.output), nil })
			err := m.checkOpenCodeVersion(context.Background(), LifecycleOptions{OpenCodeBinary: "opencode"})
			if (err == nil) != tt.valid {
				t.Fatalf("checkOpenCodeVersion(%q) error = %v", tt.output, err)
			}
		})
	}
}

func TestOpenCodeVersionCommandFailure(t *testing.T) {
	m := NewManager(func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("failed") })
	if err := m.checkOpenCodeVersion(context.Background(), LifecycleOptions{OpenCodeBinary: "opencode"}); err == nil || !strings.Contains(err.Error(), "cannot determine host OpenCode version") {
		t.Fatalf("error = %v", err)
	}
}
