// Package ops contains deployment-facing, secret-safe operational helpers.
package ops

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const DefaultProviderSecretPath = "/etc/zoe/runner-secrets/cliproxyapi-api-key"

const DefaultMemorySecretPath = "/etc/zoe/secrets/mem0-llm-api-key"

const memoryWorkerUID = 65532
const memoryWorkerGID = 65532

var (
	chownFile    = os.Chown
	effectiveUID = os.Geteuid
)

// ProviderConfig is the deployment configuration consumed by the Zoe runner.
// Secret contains the credential only while configuring; it is never rendered
// into an environment file or included in ProbeResult.
type ProviderConfig struct {
	BaseURL        string
	Model          string
	SecretPath     string
	Secret         string
	Image          string
	ProviderSocket string
	WorkspaceRoot  string
}

// MemoryConfig is the credential and endpoint consumed by the memory worker.
// Secret is held only while staging a configuration and is never rendered into
// a deployment environment file or a probe result.
type MemoryConfig struct {
	BaseURL    string
	Model      string
	SecretPath string
	Secret     string
}

func (c MemoryConfig) Normalized() (MemoryConfig, error) {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.Model = strings.TrimSpace(c.Model)
	c.SecretPath = strings.TrimSpace(c.SecretPath)
	if c.SecretPath == "" {
		c.SecretPath = DefaultMemorySecretPath
	}
	if err := ValidateProviderBaseURL(c.BaseURL); err != nil {
		return MemoryConfig{}, err
	}
	if c.Model == "" {
		return MemoryConfig{}, errors.New("memory model is required")
	}
	if !filepath.IsAbs(c.SecretPath) {
		return MemoryConfig{}, errors.New("memory secret path must be absolute")
	}
	if strings.ContainsAny(c.Model, "\r\n") {
		return MemoryConfig{}, errors.New("memory model contains a newline")
	}
	return c, nil
}

func (c ProviderConfig) Normalized() (ProviderConfig, error) {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.Model = strings.TrimSpace(c.Model)
	c.SecretPath = strings.TrimSpace(c.SecretPath)
	if c.SecretPath == "" {
		c.SecretPath = DefaultProviderSecretPath
	}
	if err := ValidateProviderBaseURL(c.BaseURL); err != nil {
		return ProviderConfig{}, err
	}
	if c.Model == "" {
		return ProviderConfig{}, errors.New("provider model is required")
	}
	if !filepath.IsAbs(c.SecretPath) {
		return ProviderConfig{}, errors.New("provider secret path must be absolute")
	}
	if strings.ContainsAny(c.Model, "\r\n") {
		return ProviderConfig{}, errors.New("provider model contains a newline")
	}
	return c, nil
}

func ValidateProviderBaseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("provider URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return errors.New("provider URL must use HTTPS or local HTTP")
	}
	return nil
}

// WriteSecretAtomic writes a private credential without exposing it in logs or
// command arguments. Existing credentials are replaced only after the complete
// new file has been synced.
func WriteSecretAtomic(path string, secret []byte) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("secret path must be absolute")
	}
	if len(secret) == 0 {
		return errors.New("provider token is empty")
	}
	return writeFileAtomic(path, secret, 0o600)
}

func writeMemorySecretAtomic(path string, secret []byte) error {
	return writeFileAtomicOwned(path, secret, 0o600, memoryWorkerUID, memoryWorkerGID)
}

// WriteRunnerEnvAtomic renders runner provider/runtime variables. The token
// itself stays in SecretPath and is never written to this file.
func WriteRunnerEnvAtomic(path string, c ProviderConfig) error {
	c, err := c.Normalized()
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	updates := map[string]string{"ZOE_OPENCODE_PROVIDER_BASE_URL": c.BaseURL, "ZOE_OPENCODE_PROVIDER_KEY_FILE": c.SecretPath, "ZOE_OPENCODE_MODEL": c.Model, "ZOE_OPENCODE_WORKSPACE_ROOT": c.WorkspaceRoot}
	lines := strings.Split(string(old), "\n")
	kept := make([]string, 0, len(lines)+3)
	for _, line := range lines {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			if _, replace := updates[key]; replace {
				continue
			}
		}
		if line != "" {
			kept = append(kept, line)
		}
	}
	for _, key := range []string{"ZOE_OPENCODE_PROVIDER_BASE_URL", "ZOE_OPENCODE_PROVIDER_KEY_FILE", "ZOE_OPENCODE_MODEL", "ZOE_OPENCODE_WORKSPACE_ROOT"} {
		if updates[key] != "" {
			kept = append(kept, key+"="+strconv.Quote(updates[key]))
		}
	}
	return writeFileAtomic(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}

// WriteFileAtomic restores or writes a private deployment file with the same
// fsync and rename guarantees used for provider configuration.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	return writeFileAtomic(path, data, mode)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return writeFileAtomicOwned(path, data, mode, -1, -1)
}
func writeFileAtomicOwned(path string, data []byte, mode os.FileMode, uid, gid int) error {
	return writeFileAtomicOwnedWithChown(path, data, mode, uid, gid, chownFile, effectiveUID)
}
func writeFileAtomicOwnedWithChown(path string, data []byte, mode os.FileMode, uid, gid int, chown func(string, int, int) error, euid func() int) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("configuration path must be absolute")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".zoe-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if info, statErr := os.Stat(path); statErr == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			current, statErr := tmp.Stat()
			currentOwner, currentOK := current.Sys().(*syscall.Stat_t)
			if statErr != nil {
				tmp.Close()
				return statErr
			}
			if !currentOK || currentOwner.Uid != st.Uid || currentOwner.Gid != st.Gid {
				if err = chown(tmpName, int(st.Uid), int(st.Gid)); err != nil {
					tmp.Close()
					return err
				}
			}
		}
	}
	if uid >= 0 {
		current, statErr := tmp.Stat()
		if statErr != nil {
			tmp.Close()
			return statErr
		}
		currentOwner, currentOK := current.Sys().(*syscall.Stat_t)
		needsChown := !currentOK || int(currentOwner.Uid) != uid || int(currentOwner.Gid) != gid
		if needsChown && euid() == 0 {
			if err = chown(tmpName, uid, gid); err != nil {
				tmp.Close()
				return err
			}
		}
	}
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// ProbeResult deliberately contains no response body or credential.
type ProbeResult struct {
	HTTPStatus     int
	Model          string
	ModelAvailable bool
	ErrorCategory  string
	ErrorReason    string
	Models         []ProviderModelMetadata
}

type ModelLimit struct {
	Context int64 `json:"context,omitempty"`
	Input   int64 `json:"input,omitempty"`
	Output  int64 `json:"output,omitempty"`
}
type ProviderModelMetadata struct {
	ID    string     `json:"id"`
	Name  string     `json:"name,omitempty"`
	Limit ModelLimit `json:"limit,omitempty"`
}

var modelIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

func positiveLimit(v any) int64 {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > float64(^uint64(0)>>1) {
		return 0
	}
	return int64(f)
}

func modelLimit(raw map[string]any) ModelLimit {
	get := func(names ...string) int64 {
		for _, n := range names {
			if v, ok := raw[n]; ok {
				if x := positiveLimit(v); x > 0 {
					return x
				}
			}
		}
		return 0
	}
	return ModelLimit{Context: get("context_length", "context_window", "max_context_length", "context"), Input: get("input_length", "max_input_length", "max_prompt_tokens", "input_token_limit", "input"), Output: get("max_output_length", "max_tokens", "output_token_limit", "output")}
}

// Probe checks the provider's models endpoint and reports coarse outcomes.
func Probe(ctx context.Context, c ProviderConfig, client *http.Client) ProbeResult {
	normalized, err := c.Normalized()
	if err != nil {
		return ProbeResult{Model: c.Model, ErrorCategory: "invalid_configuration", ErrorReason: err.Error()}
	}
	secret := []byte(normalized.Secret)
	if len(secret) == 0 {
		secret, err = os.ReadFile(normalized.SecretPath)
	}
	secret = []byte(strings.TrimSpace(string(secret)))
	if err != nil || !validToken(string(secret)) {
		return ProbeResult{Model: normalized.Model, ErrorCategory: "credential_unavailable"}
	}
	bounded := http.Client{Timeout: 8 * time.Second}
	if client != nil {
		bounded = *client
		if bounded.Timeout <= 0 || bounded.Timeout > 8*time.Second {
			bounded.Timeout = 8 * time.Second
		}
	}
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &bounded
	endpoint := normalized.BaseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ProbeResult{Model: normalized.Model, ErrorCategory: "request_error"}
	}
	req.Header.Set("Authorization", "Bearer "+string(secret))
	res, err := client.Do(req)
	if err != nil {
		category := "network_error"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			category = "timeout"
		}
		return ProbeResult{Model: normalized.Model, ErrorCategory: category}
	}
	defer res.Body.Close()
	result := ProbeResult{HTTPStatus: res.StatusCode, Model: normalized.Model}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		result.ErrorCategory = "authentication_failed"
		return result
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		result.ErrorCategory = "provider_http_error"
		return result
	}
	var raw struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&raw); err != nil {
		result.ErrorCategory = "invalid_response"
		return result
	}
	for _, model := range raw.Data {
		id, _ := model["id"].(string)
		if modelIDRE.MatchString(id) && !strings.HasPrefix(id, "gpt-image") && !strings.HasPrefix(id, "codex-auto-review") {
			result.Models = append(result.Models, ProviderModelMetadata{ID: id, Name: id, Limit: modelLimit(model)})
		}
		if modelIDMatches(id, normalized.Model) {
			result.ModelAvailable = true
		}
	}
	for _, model := range raw.Models {
		id, _ := model["id"].(string)
		if modelIDRE.MatchString(id) {
			result.Models = append(result.Models, ProviderModelMetadata{ID: id, Name: id, Limit: modelLimit(model)})
		}
	}
	if len(result.Models) > 500 {
		result.Models = result.Models[:500]
	}
	if !result.ModelAvailable {
		for _, model := range raw.Models {
			id, _ := model["id"].(string)
			if modelIDMatches(id, normalized.Model) {
				result.ModelAvailable = true
				break
			}
		}
	}
	if !result.ModelAvailable {
		result.ErrorCategory = "model_unavailable"
	}
	return result
}

func modelIDMatches(id, configured string) bool {
	if subtle.ConstantTimeCompare([]byte(id), []byte(configured)) == 1 {
		return true
	}
	if slash := strings.IndexByte(configured, '/'); slash >= 0 {
		return subtle.ConstantTimeCompare([]byte(id), []byte(configured[slash+1:])) == 1
	}
	return false
}

func (r ProbeResult) OK() bool {
	return r.HTTPStatus >= 200 && r.HTTPStatus < 300 && r.ModelAvailable && r.ErrorCategory == ""
}

// ReadSecret prompts through the supplied reader. Callers should provide a
// terminal implementation that disables echo; this helper never prints the
// entered bytes or stores them globally.
func ReadSecret(prompt io.Writer, read func() ([]byte, error)) ([]byte, error) {
	if prompt != nil {
		_, _ = io.WriteString(prompt, "API token: ")
	}
	secret, err := read()
	if err != nil {
		return nil, err
	}
	secret = []byte(strings.TrimSpace(string(secret)))
	if !validToken(string(secret)) {
		return nil, fmt.Errorf("provider token is empty or contains whitespace/control characters")
	}
	return secret, nil
}

func validToken(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		if r <= 32 || r == 127 {
			return false
		}
	}
	return true
}
