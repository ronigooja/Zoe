// Package ops contains explicit operator actions that mutate a Zoe instance.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

// KeyOptions describes an explicit key operation. DatabasePath must identify
// the control database and is opened with its exclusive operator lock.
// OutputPath, when set, is created exclusively with mode 0600. No output is
// written automatically when it is empty; callers decide how to display the
// one-time result.
type KeyOptions struct {
	DatabasePath string
	OutputPath   string
	ServerURL    string
}

// KeyResult is returned once after a successful operation. Treat Key and
// InstallationURL as secrets: this package never logs or persists them except
// in the explicitly requested OutputPath.
type KeyResult struct {
	Key             string `json:"key"`
	InstallationURL string `json:"installation_url,omitempty"`
}

// InitKey creates the first instance credential.
func InitKey(ctx context.Context, options KeyOptions) (KeyResult, error) {
	return run(ctx, options, false)
}

// RotateKey atomically replaces the instance credential. The old credential
// is invalid as soon as this operation commits.
func RotateKey(ctx context.Context, options KeyOptions) (KeyResult, error) {
	return run(ctx, options, true)
}

// InitializeInstanceKey is an explicit alias suitable for command adapters.
func InitializeInstanceKey(ctx context.Context, options KeyOptions) (KeyResult, error) {
	return InitKey(ctx, options)
}

// RotateInstanceKey is an explicit alias suitable for command adapters.
func RotateInstanceKey(ctx context.Context, options KeyOptions) (KeyResult, error) {
	return RotateKey(ctx, options)
}

func run(ctx context.Context, options KeyOptions, rotate bool) (result KeyResult, err error) {
	if options.DatabasePath == "" {
		return result, errors.New("database path is required")
	}
	if err := ValidateServerURL(options.ServerURL); err != nil {
		return result, err
	}
	store, err := storage.Open(ctx, storage.Config{Path: options.DatabasePath})
	if err != nil {
		return result, err // storage.Open's flock is the lock check
	}
	defer store.Close()
	if err := registerMigrations(store); err != nil {
		return result, err
	}
	if err := store.Migrate(ctx); err != nil {
		return result, err
	}

	key, err := control.GenerateInstanceKey()
	if err != nil {
		return result, err
	}
	result.Key = key
	if options.ServerURL != "" {
		result.InstallationURL = InstallationURL(options.ServerURL, key)
	}

	if !rotate {
		var count int
		if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM instance_auth`).Scan(&count); err != nil {
			return KeyResult{}, err
		}
		if count != 0 {
			return KeyResult{}, errors.New("instance credential already exists; use zoe key rotate")
		}
	}

	// Export before committing the hash. A failed write therefore cannot leave
	// a credential initialized without a retrievable private key.
	if options.OutputPath != "" {
		if err := writePrivateExport(options.OutputPath, result); err != nil {
			return KeyResult{}, err
		}
		defer func() {
			if err != nil {
				_ = os.Remove(options.OutputPath)
			}
		}()
	}
	if rotate {
		err = control.RotateInstanceKey(ctx, store, key)
	} else {
		_, _, err = control.InitializeInstanceAuth(ctx, store, key)
	}
	if err != nil {
		return KeyResult{}, err
	}
	return result, nil
}

func writePrivateExport(path string, result KeyResult) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()
	if err := json.NewEncoder(file).Encode(result); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	committed = true
	return nil
}

func registerMigrations(store *storage.Store) error {
	if err := storage.RegisterCoreMigrations(store); err != nil {
		return err
	}
	if err := control.RegisterInstanceMaintenanceMigration(store); err != nil {
		return err
	}
	if err := memoryapi.RegisterMigrations(store); err != nil {
		return err
	}
	if err := voice.RegisterMigrations(store); err != nil {
		return err
	}
	return capabilities.RegisterMigrations(store)
}

// InstallationURL constructs the canonical import link. The key is kept in
// the fragment so it is not sent as an HTTP query parameter.
func InstallationURL(server, key string) string {
	return "zoe://connect?" + url.Values{"server": {server}}.Encode() + "#" + url.Values{"key": {key}}.Encode()
}

// ValidateServerURL rejects addresses that cannot be safely imported by the
// native clients.
func ValidateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || strings.ContainsAny(raw, "?#\\") || strings.HasSuffix(u.Host, ":") {
		return errors.New("server must be a valid http(s) URL without credentials, query, or fragment")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return errors.New("server port must be between 1 and 65535")
		}
	}
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		return errors.New("server path cannot contain encoded separators")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return errors.New("server path cannot contain dot segments")
		}
	}
	return nil
}

// Display writes a one-time human-readable result. It intentionally does not
// include labels or diagnostics that could cause callers to persist secrets in
// logs; callers should provide an interactive terminal writer.
func (r KeyResult) Display(w io.Writer) error {
	if w == nil {
		return fmt.Errorf("display writer is required")
	}
	if _, err := fmt.Fprintln(w, r.Key); err != nil {
		return err
	}
	if r.InstallationURL != "" {
		_, err := fmt.Fprintln(w, r.InstallationURL)
		return err
	}
	return nil
}
