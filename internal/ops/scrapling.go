package ops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const ScraplingVersion = "0.5"

type ScraplingSettings struct {
	Mode       string
	URL        string
	TokenFile  string
	MaxBrowser int
}

func ParseScraplingSettings(values map[string]string) (ScraplingSettings, error) {
	s := ScraplingSettings{Mode: strings.ToLower(strings.TrimSpace(values["ZOE_SCRAPLING_MODE"])), URL: strings.TrimSpace(values["ZOE_SCRAPLING_URL"]), TokenFile: values["ZOE_SCRAPLING_TOKEN_FILE"], MaxBrowser: 1}
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
	if raw := strings.TrimSpace(values["ZOE_SCRAPLING_MAX_BROWSERS"]); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 8 {
			return s, errors.New("ZOE_SCRAPLING_MAX_BROWSERS must be between 1 and 8")
		}
		s.MaxBrowser = n
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
		return map[string]any{"type": "local", "command": []string{path}, "environment": map[string]string{"SCRAPLING_MAX_BROWSERS": strconv.Itoa(s.MaxBrowser)}}, true, nil
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
