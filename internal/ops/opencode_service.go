package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ronigooja/Zoe/internal/profiles"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var regexpVersion = regexp.MustCompile(`^opencode v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)
var regexpUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]*[$]?$`)

func providerOpenCodeOptions(d Deployment) (LifecycleOptions, bool) {
	v, _ := readEnv(d.RunnerEnv)
	journal := v["ZOE_RUNNER_JOURNAL_PATH"]
	dataRoot := filepath.Dir(filepath.Dir(journal))
	if !filepath.IsAbs(journal) {
		return LifecycleOptions{}, false
	}
	o := LifecycleOptions{Root: d.Root, DataRoot: dataRoot, OptRoot: "/opt/zoe", OpenCodeBinary: v["ZOE_OPENCODE_BINARY"]}
	_, err := os.Stat(openCodeConfigPath(o))
	return o, err == nil
}

func (m Manager) checkOpenCodeVersion(ctx context.Context, o LifecycleOptions) error {
	b, err := m.Run(ctx, o.OpenCodeBinary, "--version")
	if err != nil || !regexpVersion.MatchString(strings.TrimSpace(string(b))) {
		return errors.New("cannot determine host OpenCode version")
	}
	if o.Out != nil {
		fmt.Fprintf(o.Out, "Host OpenCode version: %s\n", strings.TrimSpace(string(b)))
	}
	return nil
}

func openCodeUnitDir(o LifecycleOptions) string {
	if o.Root == "/etc/zoe" {
		return "/etc/systemd/system"
	}
	return filepath.Join(o.Root, "systemd")
}

func openCodeRoot(o LifecycleOptions) string { return filepath.Join(o.DataRoot, "opencode") }
func openCodeConfigPath(o LifecycleOptions) string {
	return filepath.Join(openCodeRoot(o), "xdg-config", "opencode", "opencode.json")
}

func renderOpenCodeConfig(o LifecycleOptions) error {
	v, err := readEnv(filepath.Join(o.Root, "zoe-runner.env"))
	if err != nil {
		return err
	}
	model := v["ZOE_OPENCODE_MODEL"]
	provider, modelID, ok := strings.Cut(model, "/")
	if !ok || provider == "" || modelID == "" {
		return errors.New("OpenCode model must be provider/model")
	}
	baseURL := v["ZOE_OPENCODE_PROVIDER_BASE_URL"]
	providers := map[string]any{}
	if baseURL != "" {
		if err := ValidateProviderBaseURL(baseURL); err != nil {
			return err
		}
		providers[provider] = map[string]any{"name": provider, "env": []string{"ZOE_PROVIDER_KEY"}, "package": "@opencode/ai/providers/openai-compatible", "settings": map[string]string{"baseURL": baseURL}, "models": mergeProviderModels(o, provider, modelID)}
	}
	agents := map[string]any{}
	for _, p := range profiles.Definitions {
		agents[p.ID] = map[string]string{"system": p.SystemPrompt, "model": model}
	}
	// Do not configure a global deny: OpenCode's configured deny is authoritative
	// and cannot be overridden by the per-session rules carried by a claim.
	config := map[string]any{"$schema": "https://opencode.ai/config.json", "model": model, "update": "disable", "providers": providers, "agents": agents}
	b, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(openCodeConfigPath(o), append(b, '\n'), 0600)
}

func snapshotOpenCode(o LifecycleOptions) ([]fileSnapshot, error) {
	paths := []string{openCodeConfigPath(o), modelMetadataPath(o), filepath.Join(openCodeRoot(o), "applied-fingerprint"), filepath.Join(openCodeRoot(o), "binary-version"), filepath.Join(o.Root, "zoe-opencode.env")}
	for _, n := range []string{"zoe-opencode.service", "zoe-compose.service", "zoe-runner.service"} {
		paths = append(paths, filepath.Join(openCodeUnitDir(o), n))
	}
	var saved []fileSnapshot
	for _, p := range paths {
		s, err := snapshot(p)
		if err != nil {
			return nil, err
		}
		saved = append(saved, s)
	}
	return saved, nil
}

func openCodeChanged(saved []fileSnapshot) (bool, error) {
	for _, s := range saved {
		if filepath.Base(s.path) != "applied-fingerprint" {
			continue
		}
		b, err := os.ReadFile(s.path)
		if err != nil {
			return false, err
		}
		return !s.exists || !bytes.Equal(s.data, b), nil
	}
	return false, errors.New("OpenCode applied fingerprint is missing")
}

func (m Manager) renderOpenCodeUnit(ctx context.Context, o LifecycleOptions, template []byte) ([]byte, error) {
	if o.OpenCodeBinary == "" {
		var err error
		o.OpenCodeBinary, err = m.openCodeBinary(ctx, o)
		if err != nil {
			return nil, err
		}
	}
	version, err := m.Run(ctx, o.OpenCodeBinary, "--version")
	if err != nil {
		return nil, errors.New("cannot determine host OpenCode version")
	}
	versionText := strings.TrimSpace(string(version))
	if !regexpVersion.MatchString(versionText) {
		return nil, errors.New("host OpenCode returned an invalid version")
	}
	if o.Out != nil {
		fmt.Fprintf(o.Out, "Host OpenCode version: %s\n", versionText)
	}
	v, _ := readEnv(filepath.Join(o.Root, "zoe-runner.env"))
	user := v["ZOE_OPENCODE_USER"]
	if user == "" {
		user = "root"
	}
	if !regexpUser.MatchString(user) {
		return nil, errors.New("ZOE_OPENCODE_USER must be a Unix account name")
	}
	for _, p := range []string{o.OpenCodeBinary, o.Root, o.OptRoot, o.DataRoot} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\r\n\x00%\"\\ \t") {
			return nil, errors.New("unsupported OpenCode service path")
		}
	}
	secret := v["ZOE_OPENCODE_PROVIDER_KEY_FILE"]
	if secret == "" {
		secret = filepath.Join(o.Root, "runner-secrets", "cliproxyapi-api-key")
	}
	password := filepath.Join(openCodeRoot(o), "password")
	// A newly installed service can start before provider configuration; the
	// provider mapping is added only once the operator supplies that credential.
	mapping := "OPENCODE_SERVER_PASSWORD=" + password
	if _, err := os.Stat(secret); err == nil {
		mapping += ",ZOE_PROVIDER_KEY=" + secret
	}
	for _, p := range []string{secret, password} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\r\n\x00%\"\\, \t") {
			return nil, errors.New("unsupported OpenCode secret reference")
		}
	}
	env := "ZOE_SECRET_FILE_MAP=" + mapping + "\n"
	if err := writeFileAtomic(filepath.Join(o.Root, "zoe-opencode.env"), []byte(env), 0600); err != nil {
		return nil, err
	}
	s := string(bytesReplacePaths(template, o))
	s = strings.ReplaceAll(s, "/var/lib/zoe", o.DataRoot)
	s = strings.ReplaceAll(s, "User=root\nGroup=root", "User="+user+"\nGroup="+user)
	if err := writeFileAtomic(filepath.Join(openCodeRoot(o), "binary-version"), []byte(o.OpenCodeBinary+"\n"+versionText+"\n"), 0600); err != nil {
		return nil, err
	}
	config, err := os.ReadFile(openCodeConfigPath(o))
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(s))
	h.Write(config)
	h.Write([]byte(env))
	h.Write([]byte(o.OpenCodeBinary + "\n" + versionText))
	if err := writeFileAtomic(filepath.Join(openCodeRoot(o), "applied-fingerprint"), []byte(hex.EncodeToString(h.Sum(nil))), 0600); err != nil {
		return nil, err
	}
	return []byte(s), nil
}
