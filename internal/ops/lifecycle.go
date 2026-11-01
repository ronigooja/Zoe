package ops

// This file contains the deliberately boring, host-side release lifecycle. It
// does not create application credentials: those belong to key/provider/memory
// commands and are kept outside the release selector.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/runner"
)

type ReleaseManifest struct {
	SourceRevision string                  `json:"source_revision"`
	Architecture   string                  `json:"architecture"`
	Artifacts      []ReleaseArtifact       `json:"artifacts"`
	Images         map[string]ReleaseImage `json:"images"`
}
type ReleaseArtifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
	SHA256 string `json:"sha256"`
}
type ReleaseImage struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}
type LifecycleOptions struct {
	Root, OptRoot, DataRoot, Release, OpenCodeImage         string
	NonInteractive, Yes, PurgeData, PurgeCredentials, Start bool
	HealthURL                                               string
	HealthTimeout                                           time.Duration
	Out                                                     io.Writer
}

type UninstallPlan struct {
	Units, ComposeContainers, RuntimeContainers, ImageIDs, KeepFiles, RemoveFiles []string
	ComposeFile, Network, OptRoot, DataRoot                                       string
}

var revisionRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
var imageRefRE = regexp.MustCompile(`^ghcr\.io/[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*:[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ValidateRelease(dir string) (ReleaseManifest, error) {
	if !filepath.IsAbs(dir) {
		return ReleaseManifest{}, errors.New("release directory must be absolute")
	}
	manifestPath := filepath.Join(filepath.Clean(dir), "release-manifest.json")
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return ReleaseManifest{}, fmt.Errorf("read release manifest: %w", err)
	}
	var m ReleaseManifest
	if json.Unmarshal(b, &m) != nil {
		return m, errors.New("invalid release manifest JSON")
	}
	if !revisionRE.MatchString(m.SourceRevision) {
		return m, errors.New("source_revision must be a 40-character lowercase git revision")
	}
	if m.Architecture != "amd64" && m.Architecture != "arm64" {
		return m, errors.New("architecture must be amd64 or arm64")
	}
	if len(m.Artifacts) == 0 {
		return m, errors.New("manifest artifacts must be a non-empty list")
	}
	seen := map[string]bool{}
	for _, a := range m.Artifacts {
		if a.Name == "" || seen[a.Name] || filepath.IsAbs(a.Path) || a.Path == "" || strings.Contains(filepath.ToSlash(a.Path), "../") {
			return m, fmt.Errorf("invalid artifact %q", a.Name)
		}
		seen[a.Name] = true
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(a.SHA256) {
			return m, fmt.Errorf("artifact %s has invalid sha256", a.Name)
		}
		p := filepath.Join(dir, filepath.FromSlash(a.Path))
		rel, e := filepath.Rel(dir, p)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return m, fmt.Errorf("artifact %s escapes release", a.Name)
		}
		data, e := os.ReadFile(p)
		if e != nil {
			return m, fmt.Errorf("artifact %s missing: %w", a.Name, e)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != a.SHA256 {
			return m, fmt.Errorf("artifact %s sha256 mismatch", a.Name)
		}
		if a.Digest != "" && !digestRE.MatchString(a.Digest) {
			return m, fmt.Errorf("artifact %s has invalid digest", a.Name)
		}
	}
	for name, image := range m.Images {
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`).MatchString(name) || !imageRefRE.MatchString(image.Ref) || !digestRE.MatchString(image.Digest) {
			return m, fmt.Errorf("image %s is invalid", name)
		}
	}
	return m, nil
}

func (m Manager) Install(ctx context.Context, o LifecycleOptions) (ReleaseManifest, error) {
	if o.Root == "" {
		o.Root = "/etc/zoe"
	}
	if o.OptRoot == "" {
		o.OptRoot = "/opt/zoe"
	}
	if o.DataRoot == "" {
		o.DataRoot = "/var/lib/zoe"
	}
	if o.HealthTimeout == 0 {
		o.HealthTimeout = 90 * time.Second
	}
	manifest, err := ValidateRelease(o.Release)
	if err != nil {
		return manifest, err
	}
	if err = validateArch(manifest); err != nil {
		return manifest, err
	}
	if err = validateOpenCode(o.OpenCodeImage); err != nil {
		return manifest, err
	}
	if installed, err := installAlreadyComplete(o, manifest); err == nil && installed {
		if o.Out != nil {
			fmt.Fprintln(o.Out, "already installed")
		}
		return manifest, nil
	}
	if err = formatPreflightProblems(m.preflight(ctx, o, manifest)); err != nil {
		return manifest, err
	}
	if err = ensureDirs(o); err != nil {
		return manifest, err
	}
	if err = m.stageRelease(o, manifest); err != nil {
		return manifest, err
	}
	if err = m.switchCurrent(o, manifest.SourceRevision); err != nil {
		return manifest, err
	}
	if err = m.writeDeploymentFiles(o, manifest); err != nil {
		return manifest, err
	}
	if err = m.ensureNetwork(ctx); err != nil {
		return manifest, err
	}
	if err = m.pull(ctx, manifest); err != nil {
		return manifest, err
	}
	if err = m.installUnits(o); err != nil {
		return manifest, err
	}
	if !o.Start {
		printNextSteps(o.Out)
		return manifest, nil
	}
	if err := m.start(ctx); err != nil {
		return manifest, err
	}
	if err := waitHealth(ctx, m, o.HealthURL, o.HealthTimeout); err != nil {
		return manifest, err
	}
	printNextSteps(o.Out)
	return manifest, nil
}

func installAlreadyComplete(o LifecycleOptions, manifest ReleaseManifest) (bool, error) {
	current := filepath.Join(o.OptRoot, "releases", "current")
	selected, err := os.Readlink(current)
	if err != nil || selected != manifest.SourceRevision {
		return false, nil
	}
	for _, path := range []string{filepath.Join(o.Root, "zoe-stack.env"), filepath.Join(o.Root, "zoe-runner.env")} {
		if _, err := os.Stat(path); err != nil {
			return false, nil
		}
	}
	for _, path := range []string{filepath.Join(o.Root, "systemd", "zoe-compose.service"), filepath.Join(o.Root, "systemd", "zoe-runner.service"), "/etc/systemd/system/zoe-compose.service", "/etc/systemd/system/zoe-runner.service"} {
		if _, err := os.Stat(path); err == nil {
			continue
		}
		return false, nil
	}
	return true, nil
}

func (m Manager) Update(ctx context.Context, o LifecycleOptions) error {
	if o.Root == "" {
		o.Root = "/etc/zoe"
	}
	if o.OptRoot == "" {
		o.OptRoot = "/opt/zoe"
	}
	if o.HealthTimeout == 0 {
		o.HealthTimeout = 90 * time.Second
	}
	manifest, err := ValidateRelease(o.Release)
	if err != nil {
		return err
	}
	if err = validateArch(manifest); err != nil {
		return err
	}
	if err = validateOpenCode(o.OpenCodeImage); err != nil {
		return err
	}
	current := filepath.Join(o.OptRoot, "releases", "current")
	previous, _ := os.Readlink(current)
	if err = m.stageRelease(o, manifest); err != nil {
		return err
	}
	if err = m.pull(ctx, manifest); err != nil {
		return err
	}
	if err = m.ensureNetwork(ctx); err != nil {
		return err
	}
	stackSnapshot, stackErr := snapshot(filepath.Join(o.Root, "zoe-stack.env"))
	if stackErr != nil {
		return stackErr
	}
	runnerSnapshot, runnerErr := snapshot(filepath.Join(o.Root, "zoe-runner.env"))
	if runnerErr != nil {
		return runnerErr
	}
	if err = m.writeDeploymentFiles(o, manifest); err != nil {
		return restoreUpdate(ctx, m, o, current, previous, stackSnapshot, runnerSnapshot, err)
	}
	rollback := func(cause error) error {
		return restoreUpdate(ctx, m, o, current, previous, stackSnapshot, runnerSnapshot, cause)
	}
	if err = m.switchCurrent(o, manifest.SourceRevision); err != nil {
		return rollback(err)
	}
	if err = m.start(ctx); err != nil {
		return rollback(err)
	}
	if err = waitHealth(ctx, m, o.HealthURL, o.HealthTimeout); err != nil {
		return rollback(err)
	}
	if err = pruneReleases(filepath.Join(o.OptRoot, "releases"), previous, manifest.SourceRevision); err != nil {
		return rollback(err)
	}
	return nil
}

func restoreUpdate(ctx context.Context, m Manager, o LifecycleOptions, selector, previous string, stack, runner fileSnapshot, cause error) error {
	var rollbackErrs []error
	if previous != "" {
		if err := os.Remove(selector); err != nil && !os.IsNotExist(err) {
			rollbackErrs = append(rollbackErrs, err)
		}
		if err := os.Symlink(previous, selector); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
	}
	if err := stack.restore(); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	if err := runner.restore(); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	if err := m.start(ctx); err != nil {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("restart previous release: %w", err))
	}
	if len(rollbackErrs) > 0 {
		result := errors.Join(fmt.Errorf("update failed: %w", cause), fmt.Errorf("rollback failed: %w", errors.Join(rollbackErrs...)))
		printUpdateError(o.Out, result)
		return result
	}
	result := fmt.Errorf("update rolled back: %w", cause)
	printUpdateError(o.Out, result)
	return result
}

func printUpdateError(out io.Writer, err error) {
	if out != nil {
		fmt.Fprintln(out, "zoe:", err)
	}
}

func validateArch(m ReleaseManifest) error {
	want := runtime.GOARCH
	if want == "amd64" || want == "arm64" {
		if m.Architecture != want {
			return fmt.Errorf("release architecture %s does not match host %s", m.Architecture, want)
		}
	}
	return nil
}
func validateOpenCode(v string) error {
	if !imageRefRE.MatchString(strings.Split(v, "@")[0]) || !strings.Contains(v, "@") || !digestRE.MatchString(strings.SplitN(v, "@", 2)[1]) {
		return errors.New("--opencode-image must be a pinned GHCR image reference")
	}
	return nil
}
func ensureDirs(o LifecycleOptions) error {
	for _, p := range []string{o.Root, filepath.Join(o.Root, "secrets"), filepath.Join(o.Root, "runner-secrets"), filepath.Join(o.OptRoot, "releases"), o.DataRoot} {
		if !filepath.IsAbs(p) {
			return errors.New("lifecycle paths must be absolute")
		}
		if err := os.MkdirAll(p, 0750); err != nil {
			return err
		}
	}
	for _, name := range []string{"runner-token", "memory-runner-token"} {
		p := filepath.Join(o.Root, "secrets", name)
		if _, e := os.Stat(p); os.IsNotExist(e) {
			b := make([]byte, 32)
			if _, e = rand.Read(b); e != nil {
				return e
			}
			if e = os.WriteFile(p, []byte(base64.RawURLEncoding.EncodeToString(b)), 0400); e != nil {
				return e
			}
		}
	}
	return nil
}
func (m Manager) stageRelease(o LifecycleOptions, manifest ReleaseManifest) error {
	target := filepath.Join(o.OptRoot, "releases", manifest.SourceRevision)
	if _, e := os.Stat(target); os.IsNotExist(e) {
		if e = os.MkdirAll(filepath.Join(target, "bin"), 0755); e != nil {
			return e
		}
		for _, a := range manifest.Artifacts {
			src := filepath.Join(o.Release, filepath.FromSlash(a.Path))
			name := filepath.Base(a.Path)
			if name == "zoe-server" || name == "zoe-runner" || name == "zoe" || name == "zoe-secret-entrypoint" {
				data, e := os.ReadFile(src)
				if e != nil {
					return e
				}
				if e = os.WriteFile(filepath.Join(target, "bin", name), data, 0755); e != nil {
					return e
				}
			} else if strings.HasPrefix(filepath.ToSlash(a.Path), "deploy-linux/") {
				data, e := os.ReadFile(src)
				if e != nil {
					return e
				}
				dst := filepath.Join(target, filepath.FromSlash(a.Path))
				if e = os.MkdirAll(filepath.Dir(dst), 0755); e != nil {
					return e
				}
				if e = os.WriteFile(dst, data, 0644); e != nil {
					return e
				}
			}
		}
		if b, e := os.ReadFile(filepath.Join(o.Release, "SOURCE_REVISION")); e == nil {
			_ = os.WriteFile(filepath.Join(target, "SOURCE_REVISION"), b, 0644)
		}
	}
	for _, path := range []string{filepath.Join(target, "bin", "zoe-secret-entrypoint"), filepath.Join(target, "deploy-linux", "docker-compose.yml")} {
		if _, e := os.Stat(path); e != nil {
			return fmt.Errorf("release staging incomplete: required artifact %s is missing; release must contain bin/zoe-secret-entrypoint and deploy-linux/docker-compose.yml", path)
		}
	}
	return nil
}
func copyDeployFiles(src, dst string) error {
	if err := os.MkdirAll(filepath.Join(dst, "deploy-linux"), 0755); err != nil {
		return err
	}
	for _, name := range []string{"docker-compose.yml", "zoe-compose.service", "zoe-runner.service", "zoe-runner-tmpfiles.conf"} {
		p := filepath.Join(src, "deploy-linux", name)
		b, e := os.ReadFile(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(dst, "deploy-linux", name), b, 0644); e != nil {
			return e
		}
	}
	return nil
}
func (m Manager) switchCurrent(o LifecycleOptions, rev string) error {
	releases := filepath.Join(o.OptRoot, "releases")
	tmp := filepath.Join(releases, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(rev, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(releases, "current"))
}
func (m Manager) writeDeploymentFiles(o LifecycleOptions, mfest ReleaseManifest) error {
	if err := os.MkdirAll(o.Root, 0750); err != nil {
		return err
	}
	model := readEnvValue(filepath.Join(o.Root, "zoe-runner.env"), "ZOE_OPENCODE_MODEL")
	if strings.TrimSpace(model) == "" {
		model = "provider/model"
	}
	profiles, routes, err := defaultProfileValues(model)
	if err != nil {
		return err
	}
	stack := map[string]string{"ZOE_SERVER_IMAGE": image(mfest, "zoe-server"), "ZOE_MEMORY_WORKER_IMAGE": image(mfest, "zoe-memory-worker"), "ZOE_SERVER_DATA_ROOT": filepath.Join(o.DataRoot, "server"), "ZOE_MEMORY_DATA_ROOT": filepath.Join(o.DataRoot, "memory"), "ZOE_RUNNER_TOKEN_FILE": filepath.Join(o.Root, "secrets", "runner-token"), "ZOE_MEMORY_RUNNER_TOKEN_FILE": filepath.Join(o.Root, "secrets", "memory-runner-token"), "ZOE_MEM0_LLM_API_KEY_FILE": filepath.Join(o.Root, "secrets", "mem0-llm-api-key"), "ZOE_RUNTIME_PROFILES": profiles, "ZOE_DEFAULT_PROFILE_ID": "worker", "ZOE_COMPOSE_FILE": filepath.Join(o.OptRoot, "releases", "current", "deploy-linux", "docker-compose.yml")}
	runnerEnv := map[string]string{"ZOE_OPENCODE_BASE_URL": "http://127.0.0.1:4096", "ZOE_OPENCODE_PASSWORD_FILE": filepath.Join(o.DataRoot, "opencode", "password"), "ZOE_OPENCODE_WORKSPACE_ROOT": filepath.Join(o.DataRoot, "runner", "workspaces"), "ZOE_CONTROL_URL": "http://127.0.0.1:8787", "ZOE_RUNNER_JOURNAL_PATH": filepath.Join(o.DataRoot, "runner", "journal.sqlite"), "ZOE_RUNNER_ROUTES": routes, "ZOE_OPENCODE_MODEL": model}
	if err := writeEnv(filepath.Join(o.Root, "zoe-stack.env"), stack, 0600); err != nil {
		return err
	}
	return writeEnv(filepath.Join(o.Root, "zoe-runner.env"), runnerEnv, 0600)
}

const defaultOpenCodeModel = "provider/model"

func readEnvValue(path, wanted string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "export ")), "=")
		if ok && strings.TrimSpace(key) == wanted {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func defaultProfileValues(model string) (string, string, error) {
	profiles := map[string]control.RuntimeProfile{
		"worker": {ID: "worker", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "worker", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{}, Scopes: []string{}},
	}
	routes := map[string]runner.Route{
		"worker": {ProfileID: "worker", RuntimeRouteID: "opencode", ModelRouteID: "default", IsolationPolicy: "opencode-workspace-write-v1", Harness: "opencode", Model: model, ApprovalMode: "accept-edits"},
	}
	if err := validateDefaultValues(profiles, routes); err != nil {
		return "", "", err
	}
	profileJSON, err := json.Marshal(profiles)
	if err != nil {
		return "", "", err
	}
	routeJSON, err := json.Marshal(routes)
	if err != nil {
		return "", "", err
	}
	return string(profileJSON), string(routeJSON), nil
}

func validateDefaultValues(profiles map[string]control.RuntimeProfile, routes map[string]runner.Route) error {
	if len(profiles) == 0 || len(routes) == 0 || len(profiles) != len(routes) {
		return errors.New("default profile and route maps must be non-empty and have the same keys")
	}
	for id, profile := range profiles {
		if id == "" || profile.ID != id || profile.Harness != "opencode" || profile.IsolationPolicy != "opencode-workspace-write-v1" || profile.RuntimeRouteID == "" || profile.ModelRouteID == "" || profile.Role == "" {
			return fmt.Errorf("default profile %q is incomplete or invalid", id)
		}
		route, ok := routes[id]
		if !ok || route.ProfileID != id || route.RuntimeRouteID == "" || route.ModelRouteID == "" || route.IsolationPolicy != "opencode-workspace-write-v1" || route.Harness == "" || route.Model == "" {
			return fmt.Errorf("default route %q is incomplete or invalid", id)
		}
	}
	return nil
}
func image(m ReleaseManifest, name string) string { i := m.Images[name]; return i.Ref + "@" + i.Digest }
func writeEnv(path string, values map[string]string, mode os.FileMode) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) {
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, "%s=%s\n", k, values[k])
		}
		return writeFileAtomic(path, []byte(b.String()), mode)
	}
	lines := strings.SplitAfter(string(data), "\n")
	seen := make(map[string]bool, len(values))
	for i, line := range lines {
		content, ending := strings.TrimSuffix(line, "\n"), ""
		if strings.HasSuffix(line, "\n") {
			ending = "\n"
		}
		key, _, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(content, "export ")), "=")
		key = strings.TrimSpace(key)
		if ok && strings.Contains(key, " ") {
			ok = false
		}
		if ok {
			if value, generated := values[key]; generated {
				_, existingValue, _ := strings.Cut(content, "=")
				if isReleaseOwnedEnvKey(key) || (isOperatorOwnedEnvKey(key) && !validOperatorEnvValue(key, strings.TrimSpace(existingValue))) {
					lines[i] = key + "=" + value + ending
				}
				seen[key] = true
			}
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		if !seen[k] && (isReleaseOwnedEnvKey(k) || isOperatorOwnedEnvKey(k)) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	result := strings.Join(lines, "")
	if len(keys) > 0 {
		if result != "" && !strings.HasSuffix(result, "\n") {
			result += "\n"
		}
		for _, k := range keys {
			result += k + "=" + values[k] + "\n"
		}
	}
	return writeFileAtomic(path, []byte(result), mode)
}

func isReleaseOwnedEnvKey(key string) bool {
	switch key {
	case "ZOE_SERVER_IMAGE", "ZOE_MEMORY_WORKER_IMAGE",
		"ZOE_SERVER_DATA_ROOT", "ZOE_MEMORY_DATA_ROOT", "ZOE_RUNNER_TOKEN_FILE",
		"ZOE_MEMORY_RUNNER_TOKEN_FILE", "ZOE_MEM0_LLM_API_KEY_FILE", "ZOE_COMPOSE_FILE",
		"ZOE_CONTROL_URL", "ZOE_RUNNER_JOURNAL_PATH":
		return true
	default:
		return false
	}
}

func isOperatorOwnedEnvKey(key string) bool {
	switch key {
	case "ZOE_RUNTIME_PROFILES", "ZOE_DEFAULT_PROFILE_ID", "ZOE_RUNNER_ROUTES":
		return true
	default:
		return false
	}
}

func validOperatorEnvValue(key, value string) bool {
	if value == "" || value == "{}" {
		return false
	}
	switch key {
	case "ZOE_DEFAULT_PROFILE_ID":
		return true
	case "ZOE_RUNTIME_PROFILES":
		profiles := map[string]control.RuntimeProfile{}
		if json.Unmarshal([]byte(value), &profiles) != nil || len(profiles) == 0 {
			return false
		}
		for id, profile := range profiles {
			if id == "" || profile.ID != id || (profile.Harness != "" && profile.Harness != "opencode") || profile.RuntimeRouteID == "" || profile.ModelRouteID == "" || profile.Role == "" || profile.IsolationPolicy != "opencode-workspace-write-v1" {
				return false
			}
		}
		return true
	case "ZOE_RUNNER_ROUTES":
		routes := map[string]runner.Route{}
		if json.Unmarshal([]byte(value), &routes) != nil || len(routes) == 0 {
			return false
		}
		for id, route := range routes {
			if id == "" || route.ProfileID != id || route.RuntimeRouteID == "" || route.ModelRouteID == "" || route.IsolationPolicy == "" || route.Harness == "" || route.Model == "" {
				return false
			}
		}
		return true
	default:
		return false
	}
}
func (m Manager) ensureNetwork(ctx context.Context) error {
	if _, err := m.Run(ctx, "docker", "network", "inspect", "zoe"); err == nil {
		return nil
	}
	if _, err := m.Run(ctx, "docker", "network", "create", "zoe"); err != nil {
		return fmt.Errorf("create Zoe Docker network: %w", err)
	}
	return nil
}
func (m Manager) pull(ctx context.Context, manifest ReleaseManifest) error {
	for _, name := range []string{"zoe-server", "zoe-memory-worker", "zoe-runner"} {
		i := manifest.Images[name]
		if i.Digest == "" {
			continue
		}
		if _, e := m.Run(ctx, "docker", "pull", i.Ref+"@"+i.Digest); e != nil {
			return e
		}
	}
	return nil
}
func (m Manager) installUnits(o LifecycleOptions) error {
	dir := filepath.Join(o.Root, "systemd")
	if o.Root == "/etc/zoe" {
		dir = "/etc/systemd/system"
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for _, n := range []string{"zoe-compose.service", "zoe-runner.service"} {
		b, e := os.ReadFile(filepath.Join(o.OptRoot, "releases", "current", "deploy-linux", n))
		if e != nil {
			continue
		}
		b = bytesReplacePaths(b, o)
		if e = os.WriteFile(filepath.Join(dir, n), b, 0644); e != nil {
			return e
		}
	}
	_, e := m.Run(context.Background(), "systemctl", "daemon-reload")
	return e
}
func bytesReplacePaths(b []byte, o LifecycleOptions) []byte {
	return []byte(strings.ReplaceAll(strings.ReplaceAll(string(b), "/etc/zoe", o.Root), "/opt/zoe", o.OptRoot))
}
func (m Manager) start(ctx context.Context) error {
	_, e := m.Run(ctx, "systemctl", "restart", "zoe-compose.service")
	if e != nil {
		return fmt.Errorf("start zoe-compose.service: %w", e)
	}
	_, e = m.Run(ctx, "systemctl", "restart", "zoe-runner.service")
	if e != nil {
		return fmt.Errorf("start zoe-runner.service: %w", e)
	}
	return nil
}
func waitHealth(ctx context.Context, m Manager, url string, timeout time.Duration) error {
	if url == "" {
		url = "http://127.0.0.1:8787/healthz"
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		active, activeErr := m.Run(ctx, "systemctl", "is-active", "zoe-runner.service")
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		r, e := http.DefaultClient.Do(req)
		if e == nil {
			r.Body.Close()
			if activeErr == nil && strings.TrimSpace(string(active)) == "active" && r.StatusCode >= 200 && r.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("health check timed out")
}
func pruneReleases(dir, previous, current string) error {
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, x := range entries {
		if x.IsDir() && !strings.HasPrefix(x.Name(), "zoe-cli-") && x.Name() != "current" && x.Name() != filepath.Base(previous) && x.Name() != current {
			_ = os.RemoveAll(filepath.Join(dir, x.Name()))
		}
	}
	return nil
}
func printNextSteps(w io.Writer) {
	if w == nil {
		return
	}
	fmt.Fprintln(w, "Services started. Next steps:")
	fmt.Fprintln(w, "  zoe key init")
	fmt.Fprintln(w, "  zoe status  # shows Instance key: pending until key init")
	fmt.Fprintln(w, "  zoe memory configure")
	fmt.Fprintln(w, "  zoe provider configure  # systemd/runner deployments only")
}

func (m Manager) UninstallPlan(ctx context.Context, o LifecycleOptions) (UninstallPlan, error) {
	if o.Root == "" {
		o.Root = "/etc/zoe"
	}
	if o.OptRoot == "" {
		o.OptRoot = "/opt/zoe"
	}
	if o.DataRoot == "" {
		o.DataRoot = "/var/lib/zoe"
	}
	plan := UninstallPlan{Units: []string{"zoe-compose.service", "zoe-runner.service", "zoe-compose.service.d/*", "zoe-runner.service.d/*", "zoe-runner.conf"}, Network: "zoe", OptRoot: o.OptRoot, DataRoot: o.DataRoot}
	composeFile := filepath.Join(o.OptRoot, "releases", "current", "deploy-linux", "docker-compose.yml")
	if env, err := readEnv(filepath.Join(o.Root, "zoe-stack.env")); err == nil && filepath.IsAbs(env["ZOE_COMPOSE_FILE"]) {
		composeFile = env["ZOE_COMPOSE_FILE"]
	}
	if _, err := os.Stat(composeFile); err == nil {
		plan.ComposeFile = composeFile
	}
	out, err := m.Run(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=zoe")
	if err != nil {
		return plan, fmt.Errorf("find Compose containers: %w", err)
	}
	plan.ComposeContainers = lines(out)
	out, err = m.Run(ctx, "docker", "ps", "-aq", "--filter", "label=io.zoe.managed=true")
	if err != nil {
		return plan, fmt.Errorf("find OpenCode containers: %w", err)
	}
	plan.RuntimeContainers = lines(out)
	for _, repo := range []string{"ghcr.io/ronigooja/zoe-server", "ghcr.io/ronigooja/zoe-memory-worker", "ghcr.io/ronigooja/zoe-runner", "ghcr.io/ronigooja/zoe-opencode"} {
		out, err = m.Run(ctx, "docker", "images", "-q", "--filter", "reference="+repo)
		if err != nil {
			return plan, fmt.Errorf("find images for %s: %w", repo, err)
		}
		plan.ImageIDs = append(plan.ImageIDs, lines(out)...)
	}
	plan.KeepFiles = []string{filepath.Join(o.Root, "secrets", "cliproxyapi-api-key"), filepath.Join(o.Root, "secrets", "voice-gateway-api-key"), filepath.Join(o.Root, "secrets", "mem0-llm-api-key")}
	if o.PurgeData {
		plan.RemoveFiles = []string{o.DataRoot, filepath.Join(o.Root, "zoe-stack.env"), filepath.Join(o.Root, "zoe-runner.env"), filepath.Join(o.Root, "secrets", "runner-token"), filepath.Join(o.Root, "secrets", "memory-runner-token")}
	}
	return plan, nil
}

func (m Manager) Uninstall(ctx context.Context, o LifecycleOptions) error {
	plan, err := m.UninstallPlan(ctx, o)
	if err != nil {
		return err
	}
	var errs []error
	done := func(description string) {
		if o.Out != nil {
			fmt.Fprintf(o.Out, "Completed: %s\n", description)
		}
	}
	run := func(description, name string, args ...string) {
		if _, runErr := m.Run(ctx, name, args...); runErr != nil && !isAlreadyGone(runErr) {
			errs = append(errs, fmt.Errorf("%s: %w", description, runErr))
			return
		}
		done(description)
	}
	run("stop units", "systemctl", "stop", "zoe-runner.service", "zoe-compose.service")
	run("disable units", "systemctl", "disable", "zoe-runner.service", "zoe-compose.service")
	if plan.ComposeFile != "" {
		args := []string{"compose", "--project-name", "zoe", "--env-file", filepath.Join(o.Root, "zoe-stack.env"), "--file", plan.ComposeFile, "down", "--remove-orphans"}
		run("remove Compose stack", "docker", args...)
	} else if len(plan.ComposeContainers) > 0 {
		run("remove Compose containers", "docker", append([]string{"rm", "-f"}, plan.ComposeContainers...)...)
	}
	if len(plan.RuntimeContainers) > 0 {
		run("remove OpenCode containers", "docker", append([]string{"rm", "-f"}, plan.RuntimeContainers...)...)
	}
	if len(plan.ImageIDs) > 0 {
		run("remove Zoe images", "docker", append([]string{"image", "rm"}, plan.ImageIDs...)...)
	}
	run("remove Zoe network", "docker", "network", "rm", "zoe")
	for _, p := range []string{filepath.Join(o.Root, "systemd", "zoe-compose.service"), filepath.Join(o.Root, "systemd", "zoe-runner.service"), filepath.Join(o.Root, "systemd", "zoe-runner-tmpfiles.conf"), "/etc/systemd/system/zoe-compose.service", "/etc/systemd/system/zoe-runner.service", "/etc/tmpfiles.d/zoe-runner.conf"} {
		if err = os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	run("reload systemd", "systemctl", "daemon-reload")
	if o.PurgeData {
		if err = purgeData(o, o.PurgeCredentials); err != nil {
			errs = append(errs, err)
		}
	}
	// Verify that exact Zoe-owned labels/images are gone; never use prune.
	if out, e := m.Run(ctx, "docker", "ps", "-aq", "--filter", "label=io.zoe.managed=true"); e == nil && len(lines(out)) > 0 {
		errs = append(errs, errors.New("OpenCode containers remain"))
	}
	if out, e := m.Run(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=zoe"); e == nil && len(lines(out)) > 0 {
		errs = append(errs, errors.New("Compose containers remain"))
	}
	for _, repo := range []string{"ghcr.io/ronigooja/zoe-server", "ghcr.io/ronigooja/zoe-memory-worker", "ghcr.io/ronigooja/zoe-runner", "ghcr.io/ronigooja/zoe-opencode"} {
		if out, e := m.Run(ctx, "docker", "images", "-q", "--filter", "reference="+repo); e == nil && len(lines(out)) > 0 {
			errs = append(errs, fmt.Errorf("Zoe images remain for %s", repo))
		}
	}
	if len(errs) > 0 {
		if o.Out != nil {
			fmt.Fprintln(o.Out, "Uninstall stopped before removing the release root; rerun zoe uninstall after fixing the reported step.")
		}
		return errors.Join(errs...)
	}
	// Linux keeps an unlinked executable usable until the running process exits.
	if err = os.RemoveAll(o.OptRoot); err != nil {
		return fmt.Errorf("remove release root (final step): %w", err)
	}
	done("remove release root, including the installed CLI and all releases")
	return nil
}

func isAlreadyGone(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no such file") || strings.Contains(s, "not found") || strings.Contains(s, "no such network") || strings.Contains(s, "does not exist")
}

func lines(b []byte) []string {
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			result = append(result, strings.TrimSpace(line))
		}
	}
	return result
}
func purgeData(o LifecycleOptions, purgeCredentials bool) error {
	if err := os.RemoveAll(o.DataRoot); err != nil {
		return fmt.Errorf("remove data root: %w", err)
	}
	entries, err := os.ReadDir(o.Root)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		p := filepath.Join(o.Root, entry.Name())
		if entry.Name() == "secrets" || entry.Name() == "runner-secrets" {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	for _, dir := range []string{"secrets", "runner-secrets"} {
		d := filepath.Join(o.Root, dir)
		entries, _ := os.ReadDir(d)
		for _, entry := range entries {
			keep := entry.Name() == "cliproxyapi-api-key" || entry.Name() == "voice-gateway-api-key" || entry.Name() == "mem0-llm-api-key"
			if keep && !purgeCredentials {
				continue
			}
			if err := os.RemoveAll(filepath.Join(d, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
