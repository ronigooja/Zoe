package ops

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ModelKeyChown and ModelKeyStat apply to model/private credential files. Tests
// may replace these OS boundaries to simulate container ownership without root.
var ModelKeyChown = func(path string, uid, gid int) error { return chownFile(path, uid, gid) }
var ModelKeyStat = os.Stat

var validReasoningEfforts = map[string]bool{"none": true, "low": true, "medium": true, "high": true, "max": true}

func ValidateReasoningEffort(value string) error {
	value = strings.TrimSpace(value)
	if value != "" && !validReasoningEfforts[value] {
		return errors.New("reasoning effort must be one of none, low, medium, high, max, or empty")
	}
	return nil
}

func WriteModelSecretAtomic(path string, secret []byte) error {
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
	return writeFileAtomicOwnedWithChown(path, secret, 0600, ContainerUID, ContainerGID, ModelKeyChown, effectiveUID)
}

// ValidateModelKeyReadable applies to bind-mounted model keys, independently of
// the operator's privileges. It checks POSIX file ownership/mode, not ACLs.
func ValidateModelKeyReadable(path string) error {
	info, err := ModelKeyStat(path)
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
func ModelKeyWarnings(configs ...ModelConfig) []string {
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

// ModelConfig applies to independently configured menu models.
type ModelConfig struct {
	APIStyle        string
	BaseURL         string
	Model           string
	ReasoningEffort string
	Thinking        string
	SecretPath      string
	Secret          string
	TimeoutMS       string
}

func (c ModelConfig) Normalized() (ModelConfig, error) {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.APIStyle = strings.TrimSpace(c.APIStyle)
	if c.APIStyle != "" && c.APIStyle != "responses" && c.APIStyle != "chat" {
		return ModelConfig{}, errors.New("model API style must be responses or chat")
	}
	c.Thinking = strings.TrimSpace(c.Thinking)
	if c.Thinking != "" && c.Thinking != "enabled" && c.Thinking != "disabled" {
		return ModelConfig{}, errors.New("model thinking must be enabled, disabled, or empty")
	}
	c.Model = strings.TrimSpace(c.Model)
	c.ReasoningEffort = strings.TrimSpace(c.ReasoningEffort)
	if err := ValidateReasoningEffort(c.ReasoningEffort); err != nil {
		return ModelConfig{}, err
	}
	c.SecretPath = strings.TrimSpace(c.SecretPath)
	if c.SecretPath == "" {
		return ModelConfig{}, errors.New("model key file is required")
	}
	if err := ValidateModelBaseURL(c.BaseURL); err != nil {
		return ModelConfig{}, err
	}
	if c.Model == "" {
		return ModelConfig{}, errors.New("model is required")
	}
	if strings.ContainsAny(c.Model, "\r\n") {
		return ModelConfig{}, errors.New("model contains a newline")
	}
	if !filepath.IsAbs(c.SecretPath) {
		return ModelConfig{}, errors.New("model key file must be absolute")
	}
	return c, nil
}

// ValidateModelBaseURL applies to configured model endpoints and never returns the URL.
func ValidateModelBaseURL(raw string) error {
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

// WriteMenuModelEnvAtomic applies only to one menu model, preserving unrelated lines.
func WriteMenuModelEnvAtomic(path string, c ModelConfig, kind string) error {
	if _, err := menuModelPrefix(kind); err != nil {
		return err
	}
	if _, err := menuModelTimeout(kind, c); err != nil {
		return err
	}
	var normalized error
	c, normalized = c.Normalized()
	if normalized != nil {
		return normalized
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	prefix, _ := menuModelPrefix(kind)
	updates := map[string]string{prefix + "_BASE_URL": c.BaseURL, prefix + "_MODEL": c.Model, prefix + "_REASONING_EFFORT": c.ReasoningEffort, prefix + "_THINKING": c.Thinking, prefix + "_API_KEY_FILE": c.SecretPath, prefix + "_TIMEOUT_MS": c.TimeoutMS}
	if kind == "Steward" {
		updates[prefix+"_API_STYLE"] = c.APIStyle
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
	for _, suffix := range []string{"API_STYLE", "BASE_URL", "MODEL", "REASONING_EFFORT", "THINKING", "API_KEY_FILE", "TIMEOUT_MS"} {
		key := prefix + "_" + suffix
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

// ModelProbeResult contains only coarse response data.
type ModelProbeResult struct {
	HTTPStatus int
	Category   string
	Style      string
	Latency    time.Duration
}
