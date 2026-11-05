package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Deployment records resolved host paths; discovery never changes services.
type Deployment struct {
	Mode                                                                                            Mode
	Root, RunnerEnv, StackEnv, DatabasePath, ComposeFile, ComposeProject, RunnerService, DropInPath string
}

func DiscoverDeployment(ctx context.Context, m Manager, root string) (Deployment, error) {
	if root == "" {
		root = "/etc/zoe"
	}
	if !filepath.IsAbs(root) {
		return Deployment{}, errors.New("deployment root must be absolute")
	}
	d := Deployment{Root: root, Mode: ModeUnknown, RunnerEnv: filepath.Join(root, "zoe-runner.env"), StackEnv: filepath.Join(root, "zoe-stack.env"), RunnerService: "zoe-runner.service", ComposeProject: "zoe"}
	if v := os.Getenv("ZOE_DEPLOYMENT_MODE"); v != "" {
		d.Mode = Mode(v)
	}
	if m.Run == nil {
		m = NewManager(nil)
	}
	// Only fixed metadata properties are requested; Environment is deliberately excluded.
	out, err := m.Run(ctx, "systemctl", "show", d.RunnerService, "--property=LoadState,EnvironmentFiles,FragmentPath", "--no-pager")
	if err == nil && strings.Contains(string(out), "LoadState=loaded") {
		if d.Mode == ModeUnknown {
			d.Mode = ModeSystemd
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "EnvironmentFiles=") {
				v := strings.TrimPrefix(line, "EnvironmentFiles=")
				if i := strings.Index(v, " ("); i >= 0 {
					v = v[:i]
				}
				if filepath.IsAbs(v) && !strings.ContainsAny(v, "\r\n\x00") {
					d.RunnerEnv = v
				}
			}
		}
	}
	if d.Mode == ModeUnknown {
		if _, err := os.Stat(d.RunnerEnv); err == nil {
			d.Mode = ModeSystemd
		}
	}
	stack, _ := readEnv(d.StackEnv)
	if p := stack["ZOE_COMPOSE_FILE"]; filepath.IsAbs(p) {
		d.ComposeFile = p
	}
	if p := os.Getenv("ZOE_COMPOSE_FILE"); filepath.IsAbs(p) {
		d.ComposeFile = p
	}
	if d.ComposeFile != "" && d.Mode == ModeUnknown {
		d.Mode = ModeCompose
	}
	if p := stack["COMPOSE_PROJECT_NAME"]; p != "" {
		d.ComposeProject = p
	}
	if p := stack["ZOE_DATABASE_PATH"]; filepath.IsAbs(p) {
		d.DatabasePath = p
	} else if p := stack["ZOE_SERVER_DATA_ROOT"]; filepath.IsAbs(p) {
		d.DatabasePath = filepath.Join(p, "data", "zoe.sqlite")
	}
	if d.DatabasePath == "" {
		d.DatabasePath = "/var/lib/zoe/server/data/zoe.sqlite"
	}
	d.DropInPath = "/etc/systemd/system/zoe-runner.service.d/90-zoe-provider.conf"
	if root != "/etc/zoe" {
		d.DropInPath = filepath.Join(root, "systemd", "zoe-runner.service.d", "90-zoe-provider.conf")
	}
	// Reload the non-secret managed deployment descriptor, if present.
	if b, e := os.ReadFile(filepath.Join(root, "provider-deployment.json")); e == nil && len(b) < 16384 {
		var persisted Deployment
		if json.Unmarshal(b, &persisted) == nil && persisted.Root == root {
			d = persisted
		}
	}
	// A Compose stack may be supervised by systemd (zoe-compose.service).
	// A stale persisted descriptor must not make Compose-only operations use
	// the systemd provider path.
	if os.Getenv("ZOE_DEPLOYMENT_MODE") == "" && d.ComposeFile != "" && d.Mode != ModeSystemd {
		d.Mode = ModeCompose
	}
	return d, nil
}

// readEnv parses literal values without shell evaluation and returns no raw output.
func readEnv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("environment file too large")
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			decoded, e := strconv.Unquote(value)
			if e != nil {
				continue
			}
			value = decoded
		} else if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
			value = strings.Trim(value, "'")
		}
		values[strings.TrimSpace(key)] = value
	}
	return values, nil
}
func (d Deployment) ProviderConfig() (ProviderConfig, error) {
	c := ProviderConfig{SecretPath: DefaultSecretPath(d.Root)}
	v, err := readEnv(d.RunnerEnv)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.BaseURL = v["ZOE_OPENCODE_PROVIDER_BASE_URL"]
	c.Model = v["ZOE_OPENCODE_MODEL"]
	c.WorkspaceRoot = v["ZOE_OPENCODE_WORKSPACE_ROOT"]
	if v["ZOE_OPENCODE_PROVIDER_KEY_FILE"] != "" {
		c.SecretPath = v["ZOE_OPENCODE_PROVIDER_KEY_FILE"]
	}
	return c, nil
}

func (d Deployment) MemoryConfig() (MemoryConfig, error) {
	c := MemoryConfig{SecretPath: DefaultMemorySecretPath}
	v, err := readEnv(d.StackEnv)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.BaseURL = v["ZOE_MEM0_LLM_BASE_URL"]
	c.Model = v["ZOE_MEM0_LLM_MODEL"]
	c.ReasoningEffort = v["ZOE_MEM0_LLM_REASONING_EFFORT"]
	if v["ZOE_MEM0_LLM_API_KEY_FILE"] != "" {
		c.SecretPath = v["ZOE_MEM0_LLM_API_KEY_FILE"]
	}
	return c, nil
}

func (d Deployment) TriageConfig() (TriageConfig, error) {
	c := TriageConfig{}
	v, err := readEnv(d.StackEnv)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.BaseURL = v["ZOE_TRIAGE_BASE_URL"]
	c.APIStyle = v["ZOE_TRIAGE_API_STYLE"]
	c.Model = v["ZOE_TRIAGE_MODEL"]
	c.ReasoningEffort = v["ZOE_TRIAGE_REASONING_EFFORT"]
	c.Thinking = v["ZOE_TRIAGE_THINKING"]
	c.SecretPath = v["ZOE_TRIAGE_API_KEY_FILE"]
	return c, nil
}

// ServerPort returns the configured host port for the Zoe server.
func (d Deployment) ServerPort() int {
	if value, err := readEnv(d.StackEnv); err == nil {
		if port, err := strconv.Atoi(value["ZOE_SERVER_PORT"]); err == nil && port > 0 && port <= 65535 {
			return port
		}
	}
	return 8787
}

func (d Deployment) InspectConfig() InspectConfig {
	c, _ := d.ProviderConfig()
	memory, _ := d.MemoryConfig()
	services := []string{}
	if d.Mode == ModeCompose {
		services = []string{"zoe-server", "zoe-memory-worker", "qdrant"}
	} else {
		services = []string{"zoe-compose.service", d.RunnerService}
	}
	return InspectConfig{Mode: d.Mode, Services: services, ComposeProject: d.ComposeProject, ComposeFile: d.ComposeFile, ComposeEnv: d.StackEnv, SecretPath: c.SecretPath, ProviderBaseURL: c.BaseURL, ProviderModel: c.Model, ProviderConfigSource: d.RunnerEnv, ProviderLastCheck: LastProviderCheck(d), MemorySecretPath: memory.SecretPath, MemoryBaseURL: memory.BaseURL, MemoryModel: memory.Model, MemoryLastCheck: LastMemoryCheck(d)}
}

func lockDeployment(ctx context.Context, d Deployment) (func(), error) {
	if err := os.MkdirAll(d.Root, 0750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(d.Root, ".operations.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

type fileSnapshot struct {
	path   string
	data   []byte
	info   os.FileInfo
	exists bool
}

func snapshot(path string) (fileSnapshot, error) {
	s := fileSnapshot{path: path}
	var err error
	s.info, err = os.Lstat(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if !s.info.Mode().IsRegular() {
		return s, errors.New("configuration must be a regular file")
	}
	s.data, err = os.ReadFile(path)
	s.exists = true
	return s, err
}
func (s fileSnapshot) restore() error {
	if !s.exists {
		err := os.Remove(s.path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return syncDir(filepath.Dir(s.path))
	}
	if err := writeFileAtomic(s.path, s.data, s.info.Mode().Perm()); err != nil {
		return err
	}
	if stat, ok := s.info.Sys().(*syscall.Stat_t); ok {
		return os.Chown(s.path, int(stat.Uid), int(stat.Gid))
	}
	return nil
}

// ConfigureProvider validates a staged credential before modifying persistent state.
// On any post-write failure it restores all files and restarts the prior configuration.
func (m Manager) ConfigureProvider(ctx context.Context, d Deployment, c ProviderConfig, client *http.Client) (ProbeResult, error) {
	if m.Run == nil {
		m = NewManager(nil)
	}
	c, err := c.Normalized()
	if err != nil {
		return ProbeResult{Model: c.Model, ErrorCategory: "invalid_configuration", ErrorReason: err.Error()}, fmt.Errorf("invalid_configuration: %w", err)
	}
	if !validToken(c.Secret) {
		return ProbeResult{}, errors.New("provider token is empty or contains whitespace/control characters")
	}
	unlock, err := lockDeployment(ctx, d)
	if err != nil {
		return ProbeResult{}, err
	}
	defer unlock()
	if d.RunnerService != "zoe-runner.service" {
		return ProbeResult{}, errors.New("zoe-runner.service is not the configured provider consumer")
	}
	if d.Mode != ModeSystemd {
		return ProbeResult{}, errors.New("provider configuration requires the systemd zoe-runner.service deployment")
	}
	result := Probe(ctx, c, client)
	if !result.OK() {
		_ = saveProviderCheck(d, result)
		return result, probeError("provider", result)
	}
	serviceOptions, managedOpenCode := providerOpenCodeOptions(d)
	if managedOpenCode {
		enrichProviderModels(ctx, &result)
	}
	paths := []string{d.RunnerEnv, c.SecretPath, filepath.Join(d.Root, "provider-deployment.json")}
	if managedOpenCode {
		serviceFiles, e := snapshotOpenCode(serviceOptions)
		if e != nil {
			return result, e
		}
		for _, s := range serviceFiles {
			paths = append(paths, s.path)
		}
	}
	if d.DropInPath == "" {
		return result, errors.New("runner systemd drop-in path is required")
	}
	paths = append(paths, d.DropInPath)
	seen := map[string]bool{}
	saved := []fileSnapshot{}
	for _, path := range paths {
		if seen[path] {
			return result, errors.New("provider configuration paths must be distinct")
		}
		seen[path] = true
		s, e := snapshot(path)
		if e != nil {
			return result, e
		}
		saved = append(saved, s)
	}
	parent := filepath.Dir(c.SecretPath)
	parentInfo, parentErr := os.Stat(parent)
	rollback := func(cause error) error {
		errs := []error{cause}
		for i := len(saved) - 1; i >= 0; i-- {
			if e := saved[i].restore(); e != nil {
				errs = append(errs, fmt.Errorf("restore configuration: %w", e))
			}
		}
		if parentErr == nil {
			if st, ok := parentInfo.Sys().(*syscall.Stat_t); ok {
				if e := os.Chown(parent, int(st.Uid), int(st.Gid)); e != nil {
					errs = append(errs, e)
				}
			}
			if e := os.Chmod(parent, parentInfo.Mode().Perm()); e != nil {
				errs = append(errs, e)
			}
		} else if os.IsNotExist(parentErr) {
			if e := os.Remove(parent); e != nil && !os.IsNotExist(e) {
				errs = append(errs, e)
			}
		}
		fresh, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if e := m.RefreshProvider(fresh, d); e != nil {
			errs = append(errs, fmt.Errorf("reload restored configuration: %w", e))
		}
		return errors.Join(errs...)
	}
	if managedOpenCode {
		providerID, _, _ := strings.Cut(c.Model, "/")
		if providerID == "" {
			providerID = "cliproxyapi"
		}
		if err := persistProviderModels(serviceOptions, providerID, result.Models); err != nil {
			return result, rollback(err)
		}
	}
	if d.Mode == ModeSystemd {
		canonicalSecretDir := filepath.Clean(c.SecretPath) == filepath.Join("/etc/zoe", "runner-secrets", filepath.Base(c.SecretPath)) && filepath.Dir(filepath.Clean(c.SecretPath)) == "/etc/zoe/runner-secrets"
		if d.Root == "/etc/zoe" && !canonicalSecretDir {
			return result, errors.New("systemd credential must be inside /etc/zoe/runner-secrets")
		}
		uid, gid := -1, -1
		if canonicalSecretDir {
			u, e := user.Lookup("zoe-runner")
			if e != nil {
				return result, errors.New("zoe-runner account unavailable")
			}
			uid, e = strconv.Atoi(u.Uid)
			if e != nil {
				return result, e
			}
			gid, e = strconv.Atoi(u.Gid)
			if e != nil {
				return result, e
			}
			if err = os.MkdirAll(filepath.Dir(c.SecretPath), 0710); err != nil {
				return result, err
			}
			if err = os.Chown(filepath.Dir(c.SecretPath), 0, gid); err != nil {
				return result, rollback(err)
			}
			if err = os.Chmod(filepath.Dir(c.SecretPath), 0710); err != nil {
				return result, rollback(err)
			}
		}
		if err = writeFileAtomicOwned(c.SecretPath, []byte(c.Secret), 0600, uid, gid); err != nil {
			return result, rollback(err)
		}
	}
	if err = WriteRunnerEnvAtomic(d.RunnerEnv, c); err != nil {
		return result, rollback(err)
	}
	if managedOpenCode {
		if err = renderOpenCodeConfig(serviceOptions); err != nil {
			return result, rollback(err)
		}
		unitPath := filepath.Join(openCodeUnitDir(serviceOptions), "zoe-opencode.service")
		b, e := os.ReadFile(unitPath)
		if e != nil {
			return result, rollback(e)
		}
		b, e = m.renderOpenCodeUnit(ctx, serviceOptions, b)
		if e != nil {
			return result, rollback(e)
		}
		if err = writeFileAtomic(unitPath, b, 0644); err != nil {
			return result, rollback(err)
		}
		active, e := openCodeActive(ctx, serviceOptions, m)
		if e != nil || active {
			return result, rollback(errors.New("provider change requires an idle, reachable Zoe OpenCode service"))
		}
		if _, err = m.Run(ctx, "systemctl", "restart", "zoe-opencode.service"); err != nil {
			return result, rollback(err)
		}
	}
	if strings.ContainsAny(d.RunnerEnv, "\r\n\x00%\"\\") {
		return result, rollback(errors.New("unsupported systemd environment path"))
	}
	if err = writeFileAtomic(d.DropInPath, []byte("[Service]\nEnvironmentFile=\""+d.RunnerEnv+"\"\n"), 0644); err != nil {
		return result, rollback(err)
	}
	data, _ := json.Marshal(d)
	if err = writeFileAtomic(filepath.Join(d.Root, "provider-deployment.json"), data, 0600); err != nil {
		return result, rollback(err)
	}
	fresh, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err = m.RefreshProvider(fresh, d); err != nil {
		return result, rollback(err)
	}
	if err = saveProviderCheck(d, result); err != nil {
		return result, rollback(fmt.Errorf("save provider check metadata: %w", err))
	}
	return result, nil
}
func (m Manager) RefreshProvider(ctx context.Context, d Deployment) error {
	if m.Run == nil {
		m = NewManager(nil)
	}
	if d.RunnerService != "zoe-runner.service" {
		return errors.New("zoe-runner.service is not discovered")
	}
	return m.Reload(ctx, ModeSystemd, []string{d.RunnerService}, d.ComposeProject, d.ComposeFile)
}
func (m Manager) TestProvider(ctx context.Context, d Deployment, c ProviderConfig, client *http.Client) (ProbeResult, error) {
	unlock, err := lockDeployment(ctx, d)
	if err != nil {
		return ProbeResult{}, err
	}
	defer unlock()
	r := Probe(ctx, c, client)
	if err := saveProviderCheck(d, r); err != nil {
		return r, err
	}
	if !r.OK() {
		return r, probeError("provider", r)
	}
	return r, nil
}

// WriteMemoryEnvAtomic changes only the three memory-worker credential
// variables. All unrelated deployment lines are retained verbatim.
func WriteMemoryEnvAtomic(path string, c MemoryConfig) error {
	c, err := c.Normalized()
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	updates := map[string]string{
		"ZOE_MEM0_LLM_BASE_URL":         c.BaseURL,
		"ZOE_MEM0_LLM_MODEL":            c.Model,
		"ZOE_MEM0_LLM_REASONING_EFFORT": c.ReasoningEffort,
		"ZOE_MEM0_LLM_API_KEY_FILE":     c.SecretPath,
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
	for _, key := range []string{"ZOE_MEM0_LLM_BASE_URL", "ZOE_MEM0_LLM_MODEL", "ZOE_MEM0_LLM_REASONING_EFFORT", "ZOE_MEM0_LLM_API_KEY_FILE"} {
		kept = append(kept, key+"="+strconv.Quote(updates[key]))
	}
	return writeFileAtomic(path, []byte(strings.Join(kept, "\n")+"\n"), 0600)
}

// ConfigureMemory validates a staged memory credential before changing the
// Compose environment. It restores both files and the prior worker state on
// every post-write failure.
func (m Manager) ConfigureMemory(ctx context.Context, d Deployment, c MemoryConfig, providerSecretPath string, client *http.Client) (ProbeResult, error) {
	if m.Run == nil {
		m = NewManager(nil)
	}
	c, err := c.Normalized()
	if err != nil {
		return ProbeResult{Model: c.Model, ErrorCategory: "invalid_configuration", ErrorReason: err.Error()}, fmt.Errorf("invalid_configuration: %w", err)
	}
	if filepath.Clean(c.SecretPath) == filepath.Clean(providerSecretPath) {
		return ProbeResult{}, errors.New("memory secret path must be distinct from the task-model secret path")
	}
	if filepath.Clean(c.SecretPath) == filepath.Clean(d.StackEnv) {
		return ProbeResult{}, errors.New("memory secret path must be distinct from the stack environment path")
	}
	if !validToken(c.Secret) {
		return ProbeResult{}, errors.New("memory token is empty or contains whitespace/control characters")
	}
	if d.Mode != ModeCompose {
		return ProbeResult{}, errors.New("memory configuration requires the managed Compose deployment")
	}
	result := Probe(ctx, ProviderConfig{BaseURL: c.BaseURL, Model: c.Model, Secret: c.Secret}, client)
	if !result.OK() {
		_ = saveMemoryCheck(d, result)
		return result, probeError("memory", result)
	}
	unlock, err := lockDeployment(ctx, d)
	if err != nil {
		return result, err
	}
	defer unlock()
	saved := make([]fileSnapshot, 0, 3)
	for _, path := range []string{d.StackEnv, c.SecretPath, filepath.Join(d.Root, "memory-check.json")} {
		s, e := snapshot(path)
		if e != nil {
			return result, e
		}
		saved = append(saved, s)
	}
	parent := filepath.Dir(c.SecretPath)
	parentInfo, parentErr := os.Stat(parent)
	rollback := func(cause error) error {
		errs := []error{cause}
		for i := len(saved) - 1; i >= 0; i-- {
			if e := saved[i].restore(); e != nil {
				errs = append(errs, fmt.Errorf("restore configuration: %w", e))
			}
		}
		if parentErr == nil {
			if st, ok := parentInfo.Sys().(*syscall.Stat_t); ok {
				if e := os.Chown(parent, int(st.Uid), int(st.Gid)); e != nil {
					errs = append(errs, e)
				}
			}
			if e := os.Chmod(parent, parentInfo.Mode().Perm()); e != nil {
				errs = append(errs, e)
			}
		} else if os.IsNotExist(parentErr) {
			if e := os.Remove(parent); e != nil && !os.IsNotExist(e) {
				errs = append(errs, e)
			}
		}
		fresh, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if e := m.RefreshMemory(fresh, d); e != nil {
			errs = append(errs, fmt.Errorf("reload restored configuration: %w", e))
		}
		return errors.Join(errs...)
	}
	if err = writeMemorySecretAtomic(c.SecretPath, []byte(c.Secret)); err != nil {
		return result, rollback(err)
	}
	if err = WriteMemoryEnvAtomic(d.StackEnv, c); err != nil {
		return result, rollback(err)
	}
	fresh, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err = m.RefreshMemory(fresh, d); err != nil {
		return result, rollback(err)
	}
	if err = saveMemoryCheck(d, result); err != nil {
		return result, rollback(fmt.Errorf("save memory check metadata: %w", err))
	}
	return result, nil
}

func (m Manager) RefreshMemory(ctx context.Context, d Deployment) error {
	return m.ReloadCompose(ctx, []string{"zoe-memory-worker"}, d.ComposeProject, d.ComposeFile, d.StackEnv)
}

func (m Manager) TestMemory(ctx context.Context, d Deployment, c MemoryConfig, client *http.Client) (ProbeResult, error) {
	unlock, err := lockDeployment(ctx, d)
	if err != nil {
		return ProbeResult{}, err
	}
	defer unlock()
	r := Probe(ctx, ProviderConfig{BaseURL: c.BaseURL, Model: c.Model, SecretPath: c.SecretPath}, client)
	if err := saveMemoryCheck(d, r); err != nil {
		return r, err
	}
	if !r.OK() {
		return r, probeError("memory", r)
	}
	return r, nil
}

type checkMetadata struct {
	CheckedAt string `json:"checked_at"`
	Outcome   string `json:"outcome"`
}

func probeError(kind string, result ProbeResult) error {
	if result.ErrorReason != "" {
		return fmt.Errorf("%s validation failed: %s: %s", kind, result.ErrorCategory, result.ErrorReason)
	}
	return fmt.Errorf("%s validation failed: %s", kind, result.ErrorCategory)
}

func saveProviderCheck(d Deployment, r ProbeResult) error {
	outcome := r.ErrorCategory
	if r.OK() {
		outcome = "ok"
	}
	data, _ := json.Marshal(checkMetadata{time.Now().UTC().Format(time.RFC3339), outcome})
	return writeFileAtomic(filepath.Join(d.Root, "provider-check.json"), data, 0600)
}
func LastProviderCheck(d Deployment) string {
	b, err := os.ReadFile(filepath.Join(d.Root, "provider-check.json"))
	if err != nil || len(b) > 1024 {
		return "not checked"
	}
	var c checkMetadata
	if json.Unmarshal(b, &c) != nil {
		return "unknown"
	}
	if _, err = time.Parse(time.RFC3339, c.CheckedAt); err != nil {
		return "unknown"
	}
	switch c.Outcome {
	case "ok", "invalid_configuration", "credential_unavailable", "request_error", "network_error", "timeout", "authentication_failed", "provider_http_error", "invalid_response", "model_unavailable":
		return c.CheckedAt + " " + c.Outcome
	}
	return "unknown"
}

func saveMemoryCheck(d Deployment, r ProbeResult) error {
	outcome := r.ErrorCategory
	if r.OK() {
		outcome = "ok"
	}
	data, _ := json.Marshal(checkMetadata{time.Now().UTC().Format(time.RFC3339), outcome})
	return writeFileAtomic(filepath.Join(d.Root, "memory-check.json"), data, 0600)
}

func LastMemoryCheck(d Deployment) string {
	b, err := os.ReadFile(filepath.Join(d.Root, "memory-check.json"))
	if err != nil || len(b) > 1024 {
		return "not checked"
	}
	var c checkMetadata
	if json.Unmarshal(b, &c) != nil {
		return "unknown"
	}
	if _, err = time.Parse(time.RFC3339, c.CheckedAt); err != nil {
		return "unknown"
	}
	switch c.Outcome {
	case "ok", "invalid_configuration", "credential_unavailable", "request_error", "network_error", "timeout", "authentication_failed", "provider_http_error", "invalid_response", "model_unavailable":
		return c.CheckedAt + " " + c.Outcome
	}
	return "unknown"
}

func validService(name string) bool {
	return regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@-]*$`).MatchString(name)
}

// LockDeployment serializes cooperating provider and instance-key operations.
func LockDeployment(ctx context.Context, d Deployment) (func(), error) { return lockDeployment(ctx, d) }
