package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const DefaultTriageSecretPath = "/etc/zoe/secrets/triage-api-key"

func WriteTriageSecretAtomic(path string, secret []byte) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("key file path must be absolute")
	}
	if !validToken(strings.TrimSpace(string(secret))) {
		return errors.New("key is empty or contains whitespace/control characters")
	}
	return writeFileAtomicOwned(path, []byte(strings.TrimSpace(string(secret))+"\n"), 0600, 0, 0)
}

// TriageConfig is the stateless classifier endpoint configuration.
type TriageConfig struct {
	BaseURL    string
	Model      string
	SecretPath string
	Secret     string
}

func (c TriageConfig) Normalized() (TriageConfig, error) {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.Model = strings.TrimSpace(c.Model)
	c.SecretPath = strings.TrimSpace(c.SecretPath)
	if c.SecretPath == "" {
		return TriageConfig{}, errors.New("triage key file is required")
	}
	if err := ValidateModelBaseURL(c.BaseURL); err != nil {
		return TriageConfig{}, err
	}
	if c.Model == "" {
		return TriageConfig{}, errors.New("triage model is required")
	}
	if strings.ContainsAny(c.Model, "\r\n") {
		return TriageConfig{}, errors.New("triage model contains a newline")
	}
	if !filepath.IsAbs(c.SecretPath) {
		return TriageConfig{}, errors.New("triage key file must be absolute")
	}
	return c, nil
}

// ValidateModelBaseURL applies the endpoint contract used by memory and triage
// operator configuration. It intentionally does not log or return the URL.
func ValidateModelBaseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base URL must be an absolute URL without credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return errors.New("base URL must use HTTPS or local HTTP")
	}
	if strings.TrimRight(u.Path, "/") != "/v1" {
		return errors.New("base URL must end with /v1")
	}
	return nil
}

// ValidateKeyFile rejects paths that could expose a configured credential.
func ValidateKeyFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return errors.New("key file does not exist")
	}
	if !info.Mode().IsRegular() {
		return errors.New("key file is not a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("key file must not be group- or world-readable")
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return errors.New("key file is empty")
	}
	return nil
}

// WriteTriageEnvAtomic updates only triage variables and preserves all other
// lines, comments, file ownership, and existing mode.
func WriteTriageEnvAtomic(path string, c TriageConfig, disabled bool) error {
	if disabled {
		c.BaseURL = ""
	} else {
		var normalized error
		c, normalized = c.Normalized()
		if normalized != nil {
			return normalized
		}
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	updates := map[string]string{
		"ZOE_TRIAGE_BASE_URL":     c.BaseURL,
		"ZOE_TRIAGE_MODEL":        c.Model,
		"ZOE_TRIAGE_API_KEY_FILE": c.SecretPath,
	}
	lines := strings.Split(string(old), "\n")
	kept := make([]string, 0, len(lines)+3)
	for _, line := range lines {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			if _, replace := updates[key]; replace {
				continue
			}
		}
		kept = append(kept, line)
	}
	for len(kept) > 0 && kept[len(kept)-1] == "" {
		kept = kept[:len(kept)-1]
	}
	for _, key := range []string{"ZOE_TRIAGE_BASE_URL", "ZOE_TRIAGE_MODEL", "ZOE_TRIAGE_API_KEY_FILE"} {
		if updates[key] != "" {
			kept = append(kept, key+"="+quoteEnv(updates[key]))
		}
	}
	mode := os.FileMode(0600)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	return WriteFileAtomic(path, []byte(strings.Join(kept, "\n")+"\n"), mode)
}

func quoteEnv(value string) string { return strconv.Quote(value) }

// TriageProbeResult contains only coarse response data and the classifier word.
type TriageProbeResult struct {
	HTTPStatus int
	Category   string
	Latency    time.Duration
}

// TestTriage performs exactly one minimal classifier request.
func TestTriage(ctx context.Context, c TriageConfig, client *http.Client) (TriageProbeResult, error) {
	c, err := c.Normalized()
	if err != nil {
		return TriageProbeResult{}, err
	}
	secret, err := os.ReadFile(c.SecretPath)
	if err != nil || !validToken(strings.TrimSpace(string(secret))) {
		return TriageProbeResult{}, errors.New("key file is unavailable or invalid")
	}
	payload := map[string]any{"model": c.Model, "input": "Classify this sentence as exactly one category word: billing, technical, or other. Sentence: The app crashes when I open settings."}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/responses", strings.NewReader(string(body)))
	if err != nil {
		return TriageProbeResult{}, errors.New("cannot create triage request")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	req.Header.Set("Content-Type", "application/json")
	bounded := &http.Client{Timeout: 10 * time.Second}
	if client != nil {
		*bounded = *client
		if bounded.Timeout <= 0 || bounded.Timeout > 10*time.Second {
			bounded.Timeout = 10 * time.Second
		}
	}
	start := time.Now()
	resp, err := bounded.Do(req)
	latency := time.Since(start)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return TriageProbeResult{Latency: latency}, errors.New("triage request timed out")
		}
		return TriageProbeResult{Latency: latency}, errors.New("triage request failed")
	}
	defer resp.Body.Close()
	result := TriageProbeResult{HTTPStatus: resp.StatusCode, Latency: latency}
	if resp.StatusCode == http.StatusUnauthorized {
		return result, errors.New("key rejected by endpoint")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, fmt.Errorf("triage endpoint returned HTTP %d", resp.StatusCode)
	}
	var raw struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Category string `json:"category"`
		Text     string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return result, errors.New("triage endpoint returned invalid JSON")
	}
	if words := strings.Fields(raw.Category); len(words) > 0 {
		result.Category = words[0]
	}
	if result.Category == "" {
		if words := strings.Fields(raw.Text); len(words) > 0 {
			result.Category = words[0]
		}
	}
	if result.Category == "" && len(raw.Output) > 0 && len(raw.Output[0].Content) > 0 {
		words := strings.Fields(raw.Output[0].Content[0].Text)
		if len(words) > 0 {
			result.Category = strings.Trim(words[0], ".,:;!?\"'")
		}
	}
	if result.Category == "" {
		return result, errors.New("triage endpoint returned no category")
	}
	return result, nil
}
