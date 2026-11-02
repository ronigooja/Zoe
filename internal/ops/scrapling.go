package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const ScraplingVersion = "0.5"

type ScraplingSettings struct {
	Mode      string
	URL       string
	TokenFile string
}

func ParseScraplingSettings(values map[string]string) (ScraplingSettings, error) {
	s := ScraplingSettings{Mode: strings.ToLower(strings.TrimSpace(values["ZOE_SCRAPLING_MODE"])), URL: strings.TrimSpace(values["ZOE_SCRAPLING_URL"]), TokenFile: values["ZOE_SCRAPLING_TOKEN_FILE"]}
	if s.Mode == "" {
		if s.URL != "" {
			s.Mode = "remote"
		} else {
			s.Mode = "off"
		}
	}
	if s.Mode != "off" && s.Mode != "remote" && s.Mode != "local" {
		return s, fmt.Errorf("ZOE_SCRAPLING_MODE must be off, remote, or local")
	}
	if s.Mode == "remote" {
		if s.URL == "" {
			return s, errors.New("remote Scrapling requires ZOE_SCRAPLING_URL")
		}
		if err := validateScraplingURL(s.URL); err != nil {
			return s, err
		}
		if s.TokenFile == "" {
			s.TokenFile = "/etc/zoe/runner-secrets/scrapling-mcp-token"
		}
	}
	if s.Mode == "local" && s.URL != "" {
		return s, errors.New("ZOE_SCRAPLING_URL is only valid for remote mode")
	}
	if s.TokenFile != "" && (!filepath.IsAbs(s.TokenFile) || strings.ContainsAny(s.TokenFile, "\r\n\x00")) {
		return s, errors.New("ZOE_SCRAPLING_TOKEN_FILE must be an absolute path")
	}
	return s, nil
}

func validateScraplingURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return errors.New("ZOE_SCRAPLING_URL must be an http(s) URL with a host and no credentials")
	}
	return nil
}

func scraplingMCPEntry(s ScraplingSettings, optRoot string) (map[string]any, bool, error) {
	switch s.Mode {
	case "off":
		return nil, false, nil
	case "remote":
		return map[string]any{"type": "remote", "url": s.URL, "headers": map[string]string{"Authorization": "Bearer {file:" + s.TokenFile + "}"}}, true, nil
	case "local":
		path := filepath.Join(optRoot, "scrapling", ScraplingVersion, "venv", "bin", "scrapling-mcp")
		if _, err := os.Stat(path); err != nil {
			return nil, false, nil
		}
		return map[string]any{"type": "local", "command": []string{path}}, true, nil
	default:
		return nil, false, errors.New("invalid Scrapling mode")
	}
}

func scraplingTokenFingerprint(s ScraplingSettings) []byte {
	if s.Mode != "remote" {
		return nil
	}
	b, err := os.ReadFile(s.TokenFile)
	if err != nil {
		return []byte("missing")
	}
	h := sha256.Sum256(b)
	return []byte(hex.EncodeToString(h[:]))
}

var scraplingVersionRE = regexp.MustCompile(`^0\.[0-9]+$`)

func ValidateScraplingReleaseVersion(v string) error {
	if !scraplingVersionRE.MatchString(v) {
		return fmt.Errorf("invalid Scrapling release version %q", v)
	}
	return nil
}

func ValidateScraplingWheel(version, digest string) error {
	if version != ScraplingVersion || digest != "366f0ec9f1e7e998900e6c5e3a4063ea34062326394ced64e5c389b690572726" {
		return errors.New("Scrapling release pin or wheel checksum mismatch")
	}
	return nil
}

func (m Manager) ensureLocalScrapling(ctx context.Context, o LifecycleOptions, values map[string]string) error {
	s, err := ParseScraplingSettings(values)
	if err != nil || s.Mode != "local" {
		return err
	}
	bin := filepath.Join(o.OptRoot, "scrapling", ScraplingVersion, "venv", "bin", "scrapling-mcp")
	if _, err := os.Stat(bin); err == nil {
		return nil
	}
	venv := filepath.Dir(bin)
	if _, err := os.Stat(filepath.Join(venv, "python")); os.IsNotExist(err) {
		if _, e := m.Run(ctx, "dpkg-query", "-W", "-f=${Status}", "python3-venv"); e != nil {
			if _, e = m.Run(ctx, "apt-get", "update"); e != nil {
				return e
			}
			if _, e = m.Run(ctx, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "python3-venv"); e != nil {
				return e
			}
		}
		if _, e := m.Run(ctx, "python3", "-m", "venv", venv); e != nil {
			return e
		}
	}
	dir, err := os.MkdirTemp("", "zoe-scrapling-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err = m.Run(ctx, "python3", "-m", "pip", "download", "--disable-pip-version-check", "--no-deps", "--dest", dir, "scrapling=="+ScraplingVersion); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var wheel string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".whl" && strings.HasPrefix(e.Name(), "scrapling-") {
			wheel = filepath.Join(dir, e.Name())
			break
		}
	}
	if wheel == "" {
		return errors.New("Scrapling wheel download missing")
	}
	b, err := os.ReadFile(wheel)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	if err := ValidateScraplingWheel(ScraplingVersion, hex.EncodeToString(h[:])); err != nil {
		return err
	}
	if _, err = m.Run(ctx, filepath.Join(venv, "bin", "pip"), "install", "--disable-pip-version-check", "--no-cache-dir", wheel+"[ai]"); err != nil {
		return err
	}
	cache := filepath.Join(o.OptRoot, "scrapling", "cache")
	if _, err = m.Run(ctx, "env", "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(o.OptRoot, "scrapling", "browsers"), "XDG_CACHE_HOME="+cache, filepath.Join(venv, "bin", "scrapling"), "install"); err != nil {
		return err
	}
	return nil
}

func reportScraplingWarning(out io.Writer, err error) {
	if out != nil && err != nil {
		fmt.Fprintf(out, "Warning: local Scrapling unavailable; MCP omitted: %v\n", err)
	}
}
