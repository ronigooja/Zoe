package ops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ronigooja/Zoe/internal/triage"
)

const DefaultTriageSecretPath = "/etc/zoe/secrets/triage-api-key"

const DefaultTriageReasoningEffort = "low"

var validReasoningEfforts = map[string]bool{"low": true, "medium": true, "high": true}

func ValidateReasoningEffort(value string) error {
	value = strings.TrimSpace(value)
	if value != "" && !validReasoningEfforts[value] {
		return errors.New("reasoning effort must be one of low, medium, high, or empty")
	}
	return nil
}

func WriteTriageSecretAtomic(path string, secret []byte) error {
	return WriteModelKeyAtomic(path, []byte(strings.TrimSpace(string(secret))+"\n"))
}

// WriteModelKeyAtomic applies to all server/memory model credentials. Ownership
// must succeed before the synced temporary file replaces the existing key.
func WriteModelKeyAtomic(path string, secret []byte) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("key file path must be absolute")
	}
	if !validToken(strings.TrimSpace(string(secret))) {
		return errors.New("key is empty or contains whitespace/control characters")
	}
	return writeFileAtomicOwned(path, secret, 0600, ContainerUID, ContainerGID)
}

// ValidateModelKeyReadable applies to bind-mounted model keys, independently of
// the operator's privileges. It checks POSIX file ownership/mode, not ACLs.
func ValidateModelKeyReadable(path string) error {
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			read := info.Mode().Perm() & 0004
			if int(st.Uid) == ContainerUID {
				read = info.Mode().Perm() & 0400
			} else if int(st.Gid) == ContainerGID {
				read = info.Mode().Perm() & 0040
			}
			if read != 0 {
				return nil
			}
		}
	}
	return fmt.Errorf("model key file %q is not readable by server user %d:%d; fix: sudo chown %d:%d -- %q && sudo chmod 0600 -- %q (create the file first if missing)", path, ContainerUID, ContainerGID, ContainerUID, ContainerGID, path, path)
}

// ModelKeyWarnings applies to configured model endpoints, never to key contents.
func ModelKeyWarnings(configs ...TriageConfig) []string {
	var warnings []string
	for _, c := range configs {
		if c.BaseURL != "" {
			if err := ValidateModelKeyReadable(c.SecretPath); err != nil {
				warnings = append(warnings, "WARNING: "+err.Error())
			}
		}
	}
	return warnings
}

// TriageConfig is the stateless classifier endpoint configuration.
type TriageConfig struct {
	APIStyle        string
	BaseURL         string
	Model           string
	ReasoningEffort string
	Thinking        string
	SecretPath      string
	Secret          string
}

func (c TriageConfig) Normalized() (TriageConfig, error) {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.APIStyle = strings.TrimSpace(c.APIStyle)
	if c.APIStyle != "" && c.APIStyle != "responses" && c.APIStyle != "chat" {
		return TriageConfig{}, errors.New("triage API style must be responses or chat")
	}
	c.Thinking = strings.TrimSpace(c.Thinking)
	if c.Thinking != "" && c.Thinking != "enabled" && c.Thinking != "disabled" {
		return TriageConfig{}, errors.New("triage thinking must be enabled, disabled, or empty")
	}
	c.Model = strings.TrimSpace(c.Model)
	c.ReasoningEffort = strings.TrimSpace(c.ReasoningEffort)
	if err := ValidateReasoningEffort(c.ReasoningEffort); err != nil {
		return TriageConfig{}, err
	}
	c.SecretPath = strings.TrimSpace(c.SecretPath)
	if c.SecretPath == "" {
		return TriageConfig{}, errors.New("triage key file is required")
	}
	if err := ValidateTriageBaseURL(c.BaseURL, ""); err != nil {
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

// ValidateModelBaseURL applies the /v1 endpoint contract used by memory and responses triage
// operator configuration. It intentionally does not log or return the URL.
func ValidateModelBaseURL(raw string) error {
	return ValidateTriageBaseURL(raw, "responses")
}

func ValidateTriageBaseURL(raw, style string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base URL must be an absolute URL without credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return errors.New("base URL must use HTTPS or local HTTP")
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
		"ZOE_TRIAGE_API_STYLE":        c.APIStyle,
		"ZOE_TRIAGE_BASE_URL":         c.BaseURL,
		"ZOE_TRIAGE_MODEL":            c.Model,
		"ZOE_TRIAGE_REASONING_EFFORT": c.ReasoningEffort,
		"ZOE_TRIAGE_THINKING":         c.Thinking,
		"ZOE_TRIAGE_API_KEY_FILE":     c.SecretPath,
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
	for _, key := range []string{"ZOE_TRIAGE_API_STYLE", "ZOE_TRIAGE_BASE_URL", "ZOE_TRIAGE_MODEL", "ZOE_TRIAGE_REASONING_EFFORT", "ZOE_TRIAGE_THINKING", "ZOE_TRIAGE_API_KEY_FILE"} {
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
	Style      string
	Latency    time.Duration
}

// TestTriage performs exactly one minimal classifier request.
func TestTriage(ctx context.Context, c TriageConfig, client *http.Client, text ...string) (TriageProbeResult, error) {
	c, err := c.Normalized()
	if err != nil {
		return TriageProbeResult{}, err
	}
	if err := ValidateModelKeyReadable(c.SecretPath); err != nil {
		return TriageProbeResult{}, err
	}
	start := time.Now()
	clientImpl, err := triage.FromEnv(func(name string) string {
		switch name {
		case "ZOE_TRIAGE_BASE_URL":
			return c.BaseURL
		case "ZOE_TRIAGE_MODEL":
			return c.Model
		case "ZOE_TRIAGE_API_KEY_FILE":
			return c.SecretPath
		case "ZOE_TRIAGE_REASONING_EFFORT":
			return c.ReasoningEffort
		case "ZOE_TRIAGE_API_STYLE":
			return c.APIStyle
		case "ZOE_TRIAGE_THINKING":
			return c.Thinking
		}
		return ""
	}, os.ReadFile)
	if err != nil {
		return TriageProbeResult{}, err
	}
	if clientImpl == nil {
		return TriageProbeResult{}, errors.New("triage is disabled")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	clientImpl.SetHTTPClient(client)
	input := "好了吗"
	if len(text) > 0 {
		input = text[0]
	}
	category, err := clientImpl.Classify(ctx, input)
	latency := time.Since(start)
	if err != nil {
		result := TriageProbeResult{Latency: latency}
		var httpErr *triage.HTTPError
		if errors.As(err, &httpErr) {
			result.HTTPStatus = httpErr.StatusCode
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return result, errors.New("triage request timed out")
		}
		if httpErr != nil && httpErr.StatusCode == http.StatusUnauthorized {
			return result, errors.New("key rejected by endpoint")
		}
		return result, err
	}
	return TriageProbeResult{HTTPStatus: http.StatusOK, Category: string(category), Style: clientImpl.Style(), Latency: latency}, nil
}
