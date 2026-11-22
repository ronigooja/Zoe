package ops

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/ronigooja/Zoe/internal/limits"
)

// LimitsPath resolves deployment-owned configuration without sourcing env files.
func (d Deployment) LimitsPath() string {
	if path := os.Getenv("ZOE_LIMITS_FILE"); path != "" {
		return path
	}
	if path := readEnvValue(d.StackEnv, "ZOE_LIMITS_FILE"); path != "" {
		return path
	}
	if d.Root != "" {
		return filepath.Join(d.Root, "limits.json")
	}
	return limits.DefaultPath
}

func (d Deployment) loadLimits() (limits.Set, error) {
	return limits.Load(d.LimitsPath())
}

func lifecycleLimitsPath(root string) string {
	return (Deployment{Root: root, StackEnv: filepath.Join(root, "zoe-stack.env")}).LimitsPath()
}

func installLifecycleLimits(o LifecycleOptions, manifest ReleaseManifest) error {
	if err := limits.RejectLegacyEnv(os.LookupEnv); err != nil {
		return err
	}
	for _, name := range []string{"zoe-stack.env", "zoe-runner.env"} {
		env, err := readEnv(filepath.Join(o.Root, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := limits.RejectLegacyEnv(func(key string) (string, bool) { v, ok := env[key]; return v, ok }); err != nil {
			return err
		}
	}
	verified := false
	for _, artifact := range manifest.Artifacts {
		if artifact.Path == "limits.json" {
			verified = true
			break
		}
	}
	if !verified {
		return errors.New("missing release artifact limits.json")
	}
	source, dest := filepath.Join(o.Release, "limits.json"), lifecycleLimitsPath(o.Root)
	if !filepath.IsAbs(dest) || filepath.Clean(dest) != dest || dest == "/" {
		return errors.New("ZOE_LIMITS_FILE must be a canonical absolute file path")
	}
	if _, err := limits.Load(source); err != nil {
		return err
	}
	info, err := os.Lstat(dest)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0004 == 0 {
			return limits.Error("limits_unreadable")
		}
		_, err = limits.Load(dest)
		return err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	return writeFileAtomic(dest, data, 0644)
}
