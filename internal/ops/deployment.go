// Package ops provides local deployment inspection and service lifecycle helpers
// for the zoe operator CLI. It never reads or prints secret contents.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

type Mode string

const (
	ModeSystemd Mode = "systemd"
	ModeCompose Mode = "compose"
	ModeUnknown Mode = "unknown"
)

type CommandRunner func(context.Context, string, ...string) ([]byte, error)

func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type Manager struct {
	Run CommandRunner
}

func NewManager(run CommandRunner) Manager {
	if run == nil {
		run = defaultRunner
	}
	return Manager{Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := run(ctx, name, args...)
		if err == nil {
			return out, nil
		}
		cmd := strings.TrimSpace(strings.Join(append([]string{name}, args...), " "))
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		if len(detail) > 300 {
			detail = detail[len(detail)-300:]
		}
		return out, fmt.Errorf("%s failed: %w: %s", cmd, err, detail)
	}}
}

type ServiceStatus struct{ Name, State, Source string }
type SecretStatus struct {
	Path   string
	Exists bool
	Mode   string
	Owner  string
}
type ProviderStatus struct{ BaseURL, Model, ReasoningEffort, ConfigSource, LastCheck string }
type MemoryStatus struct {
	BaseURL, Model, ReasoningEffort, SecretPath, LastCheck string
	Secret                                                 SecretStatus
}
type Status struct {
	Mode     Mode
	Services []ServiceStatus
	Provider ProviderStatus
	Secret   SecretStatus
	Memory   MemoryStatus
}

type InspectConfig struct {
	Mode                    Mode
	Services                []string
	ComposeProject          string
	ComposeFile             string
	ComposeEnv              string
	ComposeOverride         string
	SecretPath              string
	ProviderBaseURL         string
	ProviderModel           string
	ProviderReasoningEffort string
	ProviderConfigSource    string
	ProviderLastCheck       string
	MemorySecretPath        string
	MemoryBaseURL           string
	MemoryModel             string
	MemoryReasoningEffort   string
	MemoryLastCheck         string
}

func explicitDeploymentMode() Mode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ZOE_DEPLOYMENT_MODE"))) {
	case "systemd":
		return ModeSystemd
	case "compose":
		return ModeCompose
	}
	return ModeUnknown
}

func composeFileExists(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func DetectMode() Mode {
	if mode := explicitDeploymentMode(); mode != ModeUnknown {
		return mode
	}
	if composeFileExists(os.Getenv("ZOE_COMPOSE_FILE")) {
		return ModeCompose
	}
	stack, _ := readEnv("/etc/zoe/zoe-stack.env")
	if composeFileExists(stack["ZOE_COMPOSE_FILE"]) {
		return ModeCompose
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return ModeSystemd
	}
	return ModeUnknown
}

func (m Manager) Inspect(ctx context.Context, cfg InspectConfig) Status {
	mode := cfg.Mode
	if mode == ModeUnknown || mode == "" {
		mode = DetectMode()
	}
	services := cfg.Services
	if len(services) == 0 {
		services = []string{"zoe-compose.service", "zoe-runner.service"}
	}
	result := Status{Mode: mode, Provider: ProviderStatus{BaseURL: redactURL(cfg.ProviderBaseURL), Model: safeModel(cfg.ProviderModel), ReasoningEffort: cfg.ProviderReasoningEffort, ConfigSource: cfg.ProviderConfigSource, LastCheck: cfg.ProviderLastCheck}, Memory: MemoryStatus{BaseURL: redactURL(cfg.MemoryBaseURL), Model: safeModel(cfg.MemoryModel), ReasoningEffort: cfg.MemoryReasoningEffort, SecretPath: cfg.MemorySecretPath, LastCheck: cfg.MemoryLastCheck}}
	for _, name := range services {
		result.Services = append(result.Services, m.service(ctx, mode, name, cfg))
	}
	result.Secret = inspectSecret(cfg.SecretPath)
	result.Memory.Secret = inspectSecret(cfg.MemorySecretPath)
	return result
}

func (m Manager) service(ctx context.Context, mode Mode, name string, cfg InspectConfig) ServiceStatus {
	// Unit names always address systemd, including on Compose stack hosts.
	if strings.HasSuffix(name, ".service") {
		mode = ModeSystemd
	}
	s := ServiceStatus{Name: name, Source: string(mode), State: "unknown"}
	var out []byte
	var err error
	if mode == ModeCompose {
		args := composeArgs(cfg.ComposeProject, cfg.ComposeFile)
		if cfg.ComposeEnv != "" {
			if _, e := os.Stat(cfg.ComposeEnv); e == nil {
				args = append(args, "--env-file", cfg.ComposeEnv)
			}
		}
		if cfg.ComposeOverride != "" {
			if _, e := os.Stat(cfg.ComposeOverride); e == nil {
				args = append(args, "--file", cfg.ComposeOverride)
			}
		}
		args = append(args, "ps", "--all", "--format", "json", name)
		out, err = m.Run(ctx, "docker", args...)
		if err != nil {
			return s
		}
		var rows []struct {
			Service string
			State   string
		}
		if json.Unmarshal(out, &rows) != nil {
			var row struct {
				Service string
				State   string
			}
			if json.Unmarshal(out, &row) == nil {
				rows = append(rows, row)
			}
		}
		for _, row := range rows {
			if row.Service == name {
				s.State = safeState(row.State)
				return s
			}
		}
		s.State = "missing"
		return s
	} else {
		out, err = m.Run(ctx, "systemctl", "is-active", name)
	}
	if strings.TrimSpace(string(out)) != "" {
		s.State = safeState(strings.TrimSpace(string(out)))
	}
	_ = err
	return s
}

func inspectSecret(path string) SecretStatus {
	s := SecretStatus{Path: path}
	if path == "" {
		return s
	}
	info, err := os.Stat(path)
	if err != nil {
		return s
	}
	s.Exists = info.Mode().IsRegular()
	s.Mode = fmt.Sprintf("%04o", info.Mode().Perm())
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		s.Owner = fmt.Sprintf("%d:%d", st.Uid, st.Gid)
	}
	return s
}

func (m Manager) Reload(ctx context.Context, mode Mode, services []string, composeProject, composeFile string) error {
	if mode == ModeUnknown || mode == "" {
		mode = DetectMode()
	}
	if len(services) == 0 {
		services = []string{"zoe-runner.service"}
	}
	for _, service := range services {
		if !validService(service) {
			return errors.New("invalid service name")
		}
	}
	switch mode {
	case ModeSystemd:
		if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return commandError("systemctl daemon-reload", err)
		}
		args := append([]string{"reload-or-restart"}, services...)
		if _, err := m.Run(ctx, "systemctl", args...); err != nil {
			return commandError("systemctl reload-or-restart", err)
		}
		return nil
	case ModeCompose:
		args := append(composeArgs(composeProject, composeFile), "up", "-d", "--no-build", "--no-deps", "--force-recreate")
		args = append(args, services...)
		if _, err := m.Run(ctx, "docker", args...); err != nil {
			return commandError("docker compose up", err)
		}
		return nil
	default:
		return errors.New("unknown deployment mode")
	}
}

// ReloadCompose reloads selected Compose services with the deployment env file.
func (m Manager) ReloadCompose(ctx context.Context, services []string, composeProject, composeFile, composeEnv string) error {
	args := append(composeArgs(composeProject, composeFile), "--env-file", composeEnv, "up", "-d", "--no-build", "--no-deps", "--force-recreate")
	args = append(args, services...)
	if _, err := m.Run(ctx, "docker", args...); err != nil {
		return commandError("docker compose up", err)
	}
	return nil
}

func commandError(action string, err error) error { return fmt.Errorf("%s failed: %w", action, err) }

func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "invalid"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
func safeModel(raw string) string {
	if len(raw) > 128 {
		return "invalid"
	}
	for _, r := range raw {
		if r < 32 || r == 127 {
			return "invalid"
		}
	}
	return raw
}

func DefaultSecretPath(root string) string {
	if strings.TrimSpace(root) == "" {
		return "/etc/zoe/runner-secrets/cliproxyapi-api-key"
	}
	return filepath.Join(root, "runner-secrets", "cliproxyapi-api-key")
}

func composeArgs(project, file string) []string {
	args := []string{"compose"}
	if project != "" {
		args = append(args, "--project-name", project)
	}
	if file != "" {
		args = append(args, "--file", file)
	}
	return args
}
func safeState(raw string) string {
	switch raw {
	case "active", "inactive", "failed", "activating", "deactivating", "reloading", "running", "exited", "paused", "restarting", "dead", "created", "removing", "missing":
		return raw
	}
	return "unknown"
}
