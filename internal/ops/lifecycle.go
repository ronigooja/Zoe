package ops

// This file contains the deliberately boring, host-side release lifecycle. It
// does not create application credentials: those belong to key/provider/memory
// commands and are kept outside the release selector.
import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
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
type OpenCodeAsset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}
type OpenCodeRelease struct {
	Repository string                   `json:"repository"`
	Tag        string                   `json:"tag"`
	Assets     map[string]OpenCodeAsset `json:"assets"`
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
	Root, OptRoot, DataRoot, Release, OpenCodeImage, OpenCodeBinary string
	NonInteractive, Yes, PurgeData, PurgeCredentials, Start         bool
	HealthURL                                                       string
	HealthTimeout                                                   time.Duration
	Out                                                             io.Writer
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
	if o.OpenCodeBinary, err = m.openCodeBinary(ctx, o); err != nil {
		return manifest, err
	}
	if err = m.checkOpenCodeVersion(ctx, o); err != nil {
		return manifest, err
	}
	if err = m.prepareHost(ctx, o); err != nil {
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
	if err = prepareLifecycleFiles(o); err != nil {
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
	if o.Start {
		if err = m.startOpenCode(ctx, o); err != nil {
			return manifest, err
		}
	}
	if o.Start {
		if err = waitOpenCode(ctx, o, m, o.HealthTimeout); err != nil {
			return manifest, err
		}
	}
	if !o.Start {
		printNextSteps(o.Out)
		return manifest, nil
	}
	if err := m.start(ctx); err != nil {
		return manifest, err
	}
	if err := waitHealth(ctx, m, o, o.HealthURL, o.HealthTimeout); err != nil {
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
	for _, path := range []string{filepath.Join(openCodeUnitDir(o), "zoe-compose.service"), filepath.Join(openCodeUnitDir(o), "zoe-runner.service"), filepath.Join(openCodeUnitDir(o), "zoe-opencode.service"), openCodeConfigPath(o)} {
		if _, err := os.Stat(path); err == nil {
			continue
		}
		return false, nil
	}
	return true, nil
}

func (m Manager) Update(ctx context.Context, o LifecycleOptions) error {
	if o.DataRoot == "" {
		o.DataRoot = "/var/lib/zoe"
	}
	if o.Root == "" {
		o.Root = "/etc/zoe"
	}
	if o.OptRoot == "" {
		o.OptRoot = "/opt/zoe"
	}
	if o.HealthTimeout == 0 {
		o.HealthTimeout = 90 * time.Second
	}
	d := Deployment{Root: o.Root, StackEnv: filepath.Join(o.Root, "zoe-stack.env")}
	triage, triageErr := d.TriageConfig()
	if triageErr != nil {
		return triageErr
	}
	memory, memoryErr := d.MemoryConfig()
	if memoryErr != nil {
		return memoryErr
	}
	warnings := ModelKeyWarnings(triage, TriageConfig{BaseURL: memory.BaseURL, SecretPath: memory.SecretPath})
	for _, warning := range warnings {
		if o.Out != nil {
			fmt.Fprintln(o.Out, warning)
		}
	}
	if len(warnings) > 0 {
		return errors.New("update stopped before service changes: " + strings.Join(warnings, "; "))
	}
	manifest, err := ValidateRelease(o.Release)
	if err != nil {
		return err
	}
	if err = validateArch(manifest); err != nil {
		return err
	}
	if o.OpenCodeBinary, err = m.openCodeBinary(ctx, o); err != nil {
		return err
	}
	if err = m.checkOpenCodeVersion(ctx, o); err != nil {
		return err
	}
	if err = m.prepareHost(ctx, o); err != nil {
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
	prepPaths := []string{
		filepath.Join(o.Root, "secrets", "runner-token"),
		filepath.Join(o.Root, "secrets", "memory-runner-token"),
		filepath.Join(o.Root, "runner-secrets", "runner-token"),
	}
	var prepSnapshots []fileSnapshot
	for _, path := range prepPaths {
		s, snapshotErr := snapshot(path)
		if snapshotErr != nil {
			return snapshotErr
		}
		prepSnapshots = append(prepSnapshots, s)
	}
	if err = prepareLifecycleFiles(o); err != nil {
		errs := []error{err}
		for _, s := range prepSnapshots {
			if restoreErr := s.restore(); restoreErr != nil {
				errs = append(errs, restoreErr)
			}
		}
		return errors.Join(errs...)
	}
	stackSnapshot, stackErr := snapshot(filepath.Join(o.Root, "zoe-stack.env"))
	if stackErr != nil {
		return stackErr
	}
	runnerSnapshot, runnerErr := snapshot(filepath.Join(o.Root, "zoe-runner.env"))
	if runnerErr != nil {
		return runnerErr
	}
	saved, err := snapshotOpenCode(o)
	if err != nil {
		return err
	}
	restarted := false
	if err = m.writeDeploymentFiles(o, manifest); err != nil {
		args := make([]any, 0, len(saved)+len(prepSnapshots)+1)
		args = append(args, err)
		for _, s := range prepSnapshots {
			args = append(args, s)
		}
		for _, s := range saved {
			args = append(args, s)
		}
		return restoreUpdate(ctx, m, o, current, previous, stackSnapshot, runnerSnapshot, args...)
	}
	if err = removeEnvKeys(filepath.Join(o.Root, "zoe-runner.env"), "ZOE_OPENCODE_IMAGE", "ZOE_OPENCODE_PROVIDER_SOCKET"); err != nil {
		return rollbackUpdateBeforeClosure(ctx, m, o, current, previous, stackSnapshot, runnerSnapshot, prepSnapshots, saved, err)
	}
	rollback := func(cause error) error {
		args := make([]any, 0, len(saved)+len(prepSnapshots)+1)
		args = append(args, cause)
		for _, s := range prepSnapshots {
			args = append(args, s)
		}
		for _, s := range saved {
			args = append(args, s)
		}
		result := restoreUpdate(ctx, m, o, current, previous, stackSnapshot, runnerSnapshot, args...)
		if restarted {
			if _, e := m.Run(ctx, "systemctl", "restart", "zoe-opencode.service"); e != nil {
				return errors.Join(result, e)
			}
		}
		return result
	}
	if err = m.switchCurrent(o, manifest.SourceRevision); err != nil {
		return rollback(err)
	}
	if err = m.installUnits(o); err != nil {
		return rollback(err)
	}
	changed, err := openCodeChanged(saved)
	if err != nil {
		return rollback(err)
	}
	if changed {
		serviceActive, serviceErr := serviceIsActive(ctx, m, "zoe-opencode.service")
		if serviceErr != nil || !serviceActive {
			restarted = true
			if err = m.startOpenCode(ctx, o); err != nil {
				return rollback(err)
			}
			if err = waitOpenCode(ctx, o, m, o.HealthTimeout); err != nil {
				return rollback(err)
			}
		} else if active, e := openCodeActive(ctx, o, m); e != nil {
			if o.Out != nil {
				fmt.Fprintln(o.Out, "OpenCode restart skipped: active session state unavailable; health will be verified")
			}
			// Leave the last applied fingerprint in place so a later update retries.
			for _, s := range saved {
				if filepath.Base(s.path) == "applied-fingerprint" {
					if e := s.restore(); e != nil {
						return rollback(e)
					}
				}
			}
		} else if active {
			if o.Out != nil {
				fmt.Fprintln(o.Out, "OpenCode restart skipped: active sessions")
			}
			// Leave the last applied fingerprint in place so a later update retries.
			for _, s := range saved {
				if filepath.Base(s.path) == "applied-fingerprint" {
					if e := s.restore(); e != nil {
						return rollback(e)
					}
				}
			}
		} else {
			restarted = true
			if _, err = m.Run(ctx, "systemctl", "restart", "zoe-opencode.service"); err != nil {
				return rollback(err)
			}
			if err = waitOpenCode(ctx, o, m, o.HealthTimeout); err != nil {
				return rollback(err)
			}
		}
	}
	if err = m.start(ctx); err != nil {
		return rollback(err)
	}
	if err = waitAllHealth(ctx, o, m, o.HealthURL, o.HealthTimeout); err != nil {
		return rollback(err)
	}
	if err = pruneReleases(filepath.Join(o.OptRoot, "releases"), previous, manifest.SourceRevision); err != nil {
		return rollback(err)
	}
	return nil
}

func rollbackUpdateBeforeClosure(ctx context.Context, m Manager, o LifecycleOptions, selector, previous string, stack, runner fileSnapshot, prep, saved []fileSnapshot, cause error) error {
	args := make([]any, 0, len(prep)+len(saved)+1)
	args = append(args, cause)
	for _, s := range prep {
		args = append(args, s)
	}
	for _, s := range saved {
		args = append(args, s)
	}
	return restoreUpdate(ctx, m, o, selector, previous, stack, runner, args...)
}

func restoreUpdate(ctx context.Context, m Manager, o LifecycleOptions, selector, previous string, stack, runner fileSnapshot, args ...any) error {
	var cause error
	var serviceFiles []fileSnapshot
	for _, arg := range args {
		switch v := arg.(type) {
		case error:
			cause = v
		case fileSnapshot:
			serviceFiles = append(serviceFiles, v)
		}
	}
	if cause == nil {
		cause = errors.New("update failed")
	}
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
	for _, s := range serviceFiles {
		if err := s.restore(); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
	}
	if len(serviceFiles) > 0 {
		if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
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
func (m Manager) openCodeBinary(ctx context.Context, o LifecycleOptions) (string, error) {
	if o.OpenCodeBinary != "" {
		if st, err := os.Stat(o.OpenCodeBinary); err == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 {
			return o.OpenCodeBinary, nil
		}
		return "", fmt.Errorf("OpenCode binary %q is missing or not executable", o.OpenCodeBinary)
	}
	if o.OpenCodeBinary == "" {
		o.OpenCodeBinary = readEnvValue(filepath.Join(o.Root, "zoe-runner.env"), "ZOE_OPENCODE_BINARY")
	}
	if o.OpenCodeBinary == "" {
		o.OpenCodeBinary = os.Getenv("ZOE_OPENCODE_BINARY")
	}
	if o.OpenCodeBinary != "" && !strings.HasPrefix(filepath.Clean(o.OpenCodeBinary), filepath.Join(filepath.Clean(o.OptRoot), "opencode")+string(os.PathSeparator)) {
		if st, err := os.Stat(o.OpenCodeBinary); err == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 {
			return o.OpenCodeBinary, nil
		}
		return "", fmt.Errorf("OpenCode binary %q is missing or not executable", o.OpenCodeBinary)
	}
	release, err := readOpenCodeRelease(o.Release)
	if err != nil {
		return "", err
	}
	managedDir := filepath.Join(o.OptRoot, "opencode", release.Tag)
	managed := filepath.Join(managedDir, "opencode")
	if version, e := os.ReadFile(filepath.Join(managedDir, "version")); e == nil && strings.TrimSpace(string(version)) == release.Tag {
		if st, e := os.Stat(managed); e == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 {
			return managed, nil
		}
	}
	asset, ok := release.Assets[runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("no pinned OpenCode asset for host architecture %s", runtime.GOARCH)
	}
	if err := installOpenCode(ctx, o.OptRoot, release, asset); err != nil {
		return "", err
	}
	if err := pruneOpenCodeVersions(filepath.Join(o.OptRoot, "opencode"), release.Tag); err != nil {
		return "", err
	}
	return managed, nil
}

func pruneOpenCodeVersions(root, current string) error {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var versions []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != current {
			versions = append(versions, entry.Name())
		}
	}
	sort.Strings(versions)
	for len(versions) > 1 {
		if err := os.RemoveAll(filepath.Join(root, versions[0])); err != nil {
			return err
		}
		versions = versions[1:]
	}
	return nil
}

var openCodeDownloadBaseURL = "https://github.com"

func readOpenCodeRelease(dir string) (OpenCodeRelease, error) {
	b, err := os.ReadFile(filepath.Join(dir, "opencode-release.json"))
	if err != nil {
		return OpenCodeRelease{}, fmt.Errorf("read pinned OpenCode release: %w", err)
	}
	var r OpenCodeRelease
	if json.Unmarshal(b, &r) != nil || r.Repository == "" || r.Tag == "" {
		return r, errors.New("invalid pinned OpenCode release")
	}
	return r, nil
}

func installOpenCode(ctx context.Context, optRoot string, release OpenCodeRelease, asset OpenCodeAsset) error {
	dir := filepath.Join(optRoot, "opencode", release.Tag)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	base := fmt.Sprintf("%s/%s/releases/download/%s", openCodeDownloadBaseURL, release.Repository, release.Tag)
	client := &http.Client{}
	fetch := func(name string) ([]byte, error) {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+name, nil)
		if e != nil {
			return nil, e
		}
		resp, e := client.Do(req)
		if e != nil {
			return nil, e
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
	sums, err := fetch("SHA256SUMS")
	if err != nil {
		return fmt.Errorf("download OpenCode SHA256SUMS for %s: %w", release.Tag, err)
	}
	var expected string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset.Name {
			expected = fields[0]
		}
	}
	if expected == "" || expected != asset.SHA256 {
		return fmt.Errorf("OpenCode asset %s has an unexpected checksum for %s", asset.Name, release.Tag)
	}
	data, err := fetch(asset.Name)
	if err != nil {
		return fmt.Errorf("download OpenCode asset %s for %s: %w", asset.Name, release.Tag, err)
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != expected {
		return fmt.Errorf("OpenCode asset %s checksum verification failed for %s", asset.Name, release.Tag)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid OpenCode asset %s for %s", asset.Name, release.Tag)
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	var binary []byte
	entries := 0
	for {
		header, e := tarReader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return fmt.Errorf("invalid OpenCode asset %s for %s", asset.Name, release.Tag)
		}
		entries++
		if header.Name == "opencode" && header.Typeflag == tar.TypeReg {
			binary, e = io.ReadAll(tarReader)
			if e != nil {
				return e
			}
		}
	}
	if entries != 1 {
		return fmt.Errorf("OpenCode asset %s for %s must contain exactly one file", asset.Name, release.Tag)
	}
	if len(binary) == 0 {
		return fmt.Errorf("OpenCode asset %s for %s did not contain opencode", asset.Name, release.Tag)
	}
	tmp, err := os.CreateTemp(dir, ".opencode-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0755); err == nil {
		_, err = tmp.Write(binary)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("install OpenCode asset %s for %s: %w", asset.Name, release.Tag, err)
	}
	if err = os.Rename(tmpName, filepath.Join(dir, "opencode")); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "version"), []byte(release.Tag+"\n"), 0644)
}
func prepareLifecycleFiles(o LifecycleOptions) error {
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
		if err := prepareLifecycleSecret(p, ContainerUID, ContainerGID); err != nil {
			return err
		}
	}
	runnerSecret := filepath.Join(o.Root, "runner-secrets", "runner-token")
	if _, err := os.Stat(runnerSecret); os.IsNotExist(err) {
		data, err := os.ReadFile(filepath.Join(o.Root, "secrets", "runner-token"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(runnerSecret, data, 0400); err != nil {
			return err
		}
	}
	stackToken, err := os.ReadFile(filepath.Join(o.Root, "secrets", "runner-token"))
	if err != nil {
		return err
	}
	runnerToken, err := os.ReadFile(runnerSecret)
	if err != nil {
		return err
	}
	if !bytes.Equal(stackToken, runnerToken) {
		return errors.New("runner token copies must match")
	}
	zoeRunner, zoeGroup, err := hostPrepLookupZoeRunnerIDs()
	if err != nil {
		return err
	}
	if err := prepareLifecycleSecret(runnerSecret, zoeRunner, zoeGroup); err != nil {
		return err
	}
	runnerWorkspaces := filepath.Join(o.DataRoot, "runner", "workspaces")
	opencode := filepath.Join(o.DataRoot, "opencode")
	for _, p := range []string{opencode, filepath.Join(opencode, "xdg-config", "opencode"), filepath.Join(opencode, "xdg-data"), filepath.Join(opencode, "xdg-state"), filepath.Join(opencode, "xdg-cache"), runnerWorkspaces} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return err
		}
	}
	if err := os.Chmod(runnerWorkspaces, 0700); err != nil {
		return err
	}
	if err := hostPrepChown(runnerWorkspaces, zoeRunner, zoeGroup); err != nil {
		return err
	}
	password := filepath.Join(opencode, "password")
	if _, err := os.Stat(password); os.IsNotExist(err) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		if err := os.WriteFile(password, []byte(base64.RawURLEncoding.EncodeToString(b)), 0600); err != nil {
			return err
		}
	}
	return nil
}

func prepareLifecycleSecret(path string, uid, gid int) error {
	if err := os.Chmod(path, 0400); err != nil {
		return err
	}
	if err := hostPrepChown(path, uid, gid); err != nil {
		return err
	}
	return nil
}

var hostPrepLookupZoeRunnerIDs = func() (int, int, error) {
	u, err := user.Lookup("zoe-runner")
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}
	if g, err := user.LookupGroup("zoe-runner"); err == nil {
		gid, err = strconv.Atoi(g.Gid)
		if err != nil {
			return 0, 0, err
		}
	}
	return uid, gid, nil
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
	for _, path := range []string{filepath.Join(target, "bin", "zoe-secret-entrypoint"), filepath.Join(target, "deploy-linux", "docker-compose.yml"), filepath.Join(target, "deploy-linux", "zoe-opencode.service")} {
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
	for _, name := range []string{"docker-compose.yml", "zoe-compose.service", "zoe-runner.service", "zoe-opencode.service", "zoe-runner-tmpfiles.conf"} {
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
		model = "cliproxyapi/gpt-6.1-sol"
	}
	profiles, routes, err := defaultProfileValues(model)
	if err != nil {
		return err
	}
	stack := map[string]string{"ZOE_SERVER_IMAGE": image(mfest, "zoe-server"), "ZOE_MEMORY_WORKER_IMAGE": image(mfest, "zoe-memory-worker"), "ZOE_SERVER_DATA_ROOT": filepath.Join(o.DataRoot, "server"), "ZOE_MEMORY_DATA_ROOT": filepath.Join(o.DataRoot, "memory"), "ZOE_RUNNER_TOKEN_FILE": filepath.Join(o.Root, "secrets", "runner-token"), "ZOE_MEMORY_RUNNER_TOKEN_FILE": filepath.Join(o.Root, "secrets", "memory-runner-token"), "ZOE_MEM0_LLM_API_KEY_FILE": filepath.Join(o.Root, "secrets", "mem0-llm-api-key"), "ZOE_RUNTIME_PROFILES": profiles, "ZOE_DEFAULT_PROFILE_ID": "worker", "ZOE_MAX_RUNS": "12", "ZOE_COMPOSE_FILE": filepath.Join(o.OptRoot, "releases", "current", "deploy-linux", "docker-compose.yml")}
	oldStack, _ := readEnv(filepath.Join(o.Root, "zoe-stack.env"))
	for _, kind := range []string{"SUMMARY", "ANSWER"} {
		for _, suffix := range []string{"BASE_URL", "MODEL", "REASONING_EFFORT", "THINKING", "TIMEOUT_MS", "API_KEY_FILE"} {
			key := "ZOE_" + kind + "_" + suffix
			if value, ok := oldStack[key]; ok {
				stack[key] = value
			}
		}
	}
	for _, key := range []string{"ZOE_MEM0_LLM_PROVIDER", "ZOE_MEM0_LLM_MODEL", "ZOE_MEM0_LLM_BASE_URL", "ZOE_MEM0_LLM_REASONING_EFFORT", "ZOE_TRIAGE_API_STYLE", "ZOE_TRIAGE_BASE_URL", "ZOE_TRIAGE_MODEL", "ZOE_TRIAGE_REASONING_EFFORT", "ZOE_TRIAGE_THINKING", "ZOE_TRIAGE_TIMEOUT_MS", "ZOE_TRIAGE_API_KEY_FILE"} {
		if value := strings.TrimSpace(oldStack[key]); value != "" {
			stack[key] = value
		}
	}
	port := readEnvValue(filepath.Join(o.Root, "zoe-stack.env"), "ZOE_SERVER_PORT")
	if _, err := strconv.Atoi(port); err != nil || port == "" {
		port = "8787"
	}
	runnerEnv := map[string]string{"ZOE_OPENCODE_BASE_URL": "http://127.0.0.1:4096", "ZOE_OPENCODE_PASSWORD_FILE": "/run/credentials/zoe-runner.service/opencode-password", "ZOE_OPENCODE_WORKSPACE_ROOT": filepath.Join(o.DataRoot, "runner", "workspaces"), "ZOE_CONTROL_URL": "http://127.0.0.1:" + port, "ZOE_RUNNER_JOURNAL_PATH": filepath.Join(o.DataRoot, "runner", "journal.sqlite"), "ZOE_RUNNER_ROUTES": routes, "ZOE_OPENCODE_MODEL": model}
	if o.OpenCodeBinary != "" {
		runnerEnv["ZOE_OPENCODE_BINARY"] = o.OpenCodeBinary
	}
	if err := writeEnv(filepath.Join(o.Root, "zoe-stack.env"), stack, 0600); err != nil {
		return err
	}
	if err := writeEnv(filepath.Join(o.Root, "zoe-runner.env"), runnerEnv, 0600); err != nil {
		return err
	}
	values, _ := readEnv(filepath.Join(o.Root, "zoe-runner.env"))
	if err := m.ensureLocalScrapling(context.Background(), o, values); err != nil {
		reportScraplingWarning(o.Out, err)
	}
	return renderOpenCodeConfig(o)
}

const defaultOpenCodeModel = "provider/model"

func readEnvValue(path, wanted string) string {
	values, _ := readEnv(path)
	return values[wanted]
}

func defaultProfileValues(model string) (string, string, error) {
	profiles := map[string]control.RuntimeProfile{
		"worker":      {ID: "worker", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "worker", Agent: "zoe-worker", PermissionTier: "workspace_read_write", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{"workspace.read", "workspace.write"}, Scopes: []string{"workspace"}},
		"zoe-worker":  {ID: "zoe-worker", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "worker", Agent: "zoe-worker", PermissionTier: "workspace_read_write", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{"workspace.read", "workspace.write"}, Scopes: []string{"workspace"}},
		"zoe-steward": {ID: "zoe-steward", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "steward", Agent: "zoe-steward", PermissionTier: "read_only_research", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{"research.read"}, Scopes: []string{"workspace"}},
		"zoe-super":   {ID: "zoe-super", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "super", Agent: "zoe-super", PermissionTier: "external_paths", IsolationPolicy: "opencode-workspace-write-v1", CapabilityIDs: []string{"workspace.read", "workspace.write", "external.read"}, Scopes: []string{"workspace"}},
	}
	routes := map[string]runner.Route{
		"worker":      {ProfileID: "worker", RuntimeRouteID: "opencode", ModelRouteID: "default", IsolationPolicy: "opencode-workspace-write-v1", Harness: "opencode", Model: model, ApprovalMode: "accept-edits"},
		"zoe-worker":  {ProfileID: "zoe-worker", RuntimeRouteID: "opencode", ModelRouteID: "default", IsolationPolicy: "opencode-workspace-write-v1", Harness: "opencode", Model: model, ApprovalMode: "accept-edits"},
		"zoe-steward": {ProfileID: "zoe-steward", RuntimeRouteID: "opencode", ModelRouteID: "default", IsolationPolicy: "opencode-workspace-write-v1", Harness: "opencode", Model: model, ApprovalMode: "accept-edits"},
		"zoe-super":   {ProfileID: "zoe-super", RuntimeRouteID: "opencode", ModelRouteID: "default", IsolationPolicy: "opencode-workspace-write-v1", Harness: "opencode", Model: model, ApprovalMode: "accept-edits"},
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
	for _, kind := range []string{"SUMMARY", "ANSWER"} {
		for _, suffix := range []string{"BASE_URL", "MODEL", "REASONING_EFFORT", "THINKING", "TIMEOUT_MS", "API_KEY_FILE"} {
			if key == "ZOE_"+kind+"_"+suffix {
				return true
			}
		}
	}
	switch key {
	case "ZOE_RUNTIME_PROFILES", "ZOE_DEFAULT_PROFILE_ID", "ZOE_MAX_RUNS", "ZOE_RUNNER_ROUTES", "ZOE_AGENT_CONTEXT_WINDOW_RATIO", "ZOE_AGENT_CONTEXT_TOKEN_BUDGET", "ZOE_AGENT_MAX_ROTATIONS", "ZOE_AGENT_ROTATION_SUMMARY":
		return true
	case "ZOE_OPENCODE_BASE_URL", "ZOE_OPENCODE_PASSWORD_FILE", "ZOE_OPENCODE_WORKSPACE_ROOT", "ZOE_OPENCODE_MODEL", "ZOE_OPENCODE_BINARY", "ZOE_OPENCODE_USER":
		return true
	case "ZOE_SCRAPLING_MODE", "ZOE_SCRAPLING_URL", "ZOE_SCRAPLING_TOKEN_FILE", "ZOE_VOICE_UPSTREAM_ROUTE":
		return true
	case "ZOE_VOICE_INGEST_TOKEN_FILE", "ZOE_VOICE_INGEST_ID", "ZOE_VOICE_INGEST_WORKSPACES":
		return true
	case "ZOE_MEM0_LLM_PROVIDER", "ZOE_MEM0_LLM_MODEL", "ZOE_MEM0_LLM_BASE_URL", "ZOE_MEM0_LLM_REASONING_EFFORT", "ZOE_TRIAGE_API_STYLE", "ZOE_TRIAGE_BASE_URL", "ZOE_TRIAGE_MODEL", "ZOE_TRIAGE_REASONING_EFFORT", "ZOE_TRIAGE_THINKING", "ZOE_TRIAGE_TIMEOUT_MS", "ZOE_TRIAGE_API_KEY_FILE":
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
	case "ZOE_OPENCODE_BASE_URL", "ZOE_OPENCODE_PASSWORD_FILE", "ZOE_OPENCODE_WORKSPACE_ROOT", "ZOE_OPENCODE_MODEL", "ZOE_OPENCODE_BINARY", "ZOE_OPENCODE_USER", "ZOE_SCRAPLING_MODE", "ZOE_SCRAPLING_URL", "ZOE_SCRAPLING_TOKEN_FILE":
		return true
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
	case "ZOE_VOICE_UPSTREAM_ROUTE":
		return value == "wm" || value == "avas"
	case "ZOE_VOICE_INGEST_TOKEN_FILE", "ZOE_VOICE_INGEST_ID", "ZOE_VOICE_INGEST_WORKSPACES":
		return true
	case "ZOE_AGENT_CONTEXT_WINDOW_RATIO":
		n, err := strconv.ParseFloat(value, 64)
		return err == nil && n > 0 && n < 1
	case "ZOE_AGENT_CONTEXT_TOKEN_BUDGET", "ZOE_AGENT_MAX_ROTATIONS":
		n, err := strconv.ParseInt(value, 10, 64)
		return err == nil && n >= 0
	case "ZOE_AGENT_ROTATION_SUMMARY":
		return value == "on" || value == "off"
	default:
		return false
	}
}

func removeEnvKeys(path string, keys ...string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	remove := make(map[string]bool, len(keys))
	for _, key := range keys {
		remove[key] = true
	}
	lines := strings.SplitAfter(string(data), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		content := strings.TrimSuffix(line, "\n")
		key, _, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(content, "export ")), "=")
		if ok && remove[strings.TrimSpace(key)] {
			continue
		}
		result = append(result, line)
	}
	return writeFileAtomic(path, []byte(strings.Join(result, "")), 0600)
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

var hostPrepEUID = os.Geteuid
var hostPrepChown = func(path string, uid, gid int) error { return os.Chown(path, uid, gid) }

func (m Manager) prepareHost(ctx context.Context, o LifecycleOptions) error {
	if hostPrepEUID() != 0 {
		return errors.New("host preparation requires root (use sudo)")
	}
	step := func(name string, err error) error {
		if err != nil {
			return fmt.Errorf("prepare host %s: %w", name, err)
		}
		return nil
	}
	if _, err := m.Run(ctx, "getent", "group", "docker"); err != nil {
		return step("Docker group", err)
	}
	if _, err := m.Run(ctx, "getent", "group", "zoe-runner"); err != nil {
		if _, err = m.Run(ctx, "groupadd", "--system", "zoe-runner"); err != nil {
			return step("zoe-runner group", err)
		}
	}
	if _, err := m.Run(ctx, "id", "zoe-runner"); err != nil {
		if _, err = m.Run(ctx, "useradd", "--system", "--gid", "zoe-runner", "--home-dir", filepath.Join(o.DataRoot, "runner"), "--shell", "/usr/sbin/nologin", "zoe-runner"); err != nil {
			return step("zoe-runner user", err)
		}
	}
	if _, err := m.Run(ctx, "usermod", "--append", "--groups", "docker", "zoe-runner"); err != nil {
		return step("Docker group membership", err)
	}
	if _, err := m.Run(ctx, "docker", "network", "inspect", "zoe"); err != nil {
		if _, err = m.Run(ctx, "docker", "network", "create", "zoe"); err != nil {
			return step("Docker network", err)
		}
	}
	zoeRunner, zoeGroup, err := hostPrepLookupZoeRunnerIDs()
	if err != nil {
		return step("zoe-runner account", err)
	}
	dirs := []struct {
		path     string
		mode     os.FileMode
		uid, gid int
	}{
		{o.OptRoot + "/releases", 0755, 0, 0},
		{o.Root, 0750, 0, zoeGroup}, {o.DataRoot, 0750, 0, zoeGroup},
		{filepath.Join(o.DataRoot, "server"), 0700, ContainerUID, ContainerGID}, {filepath.Join(o.DataRoot, "server/data"), 0700, ContainerUID, ContainerGID},
		{filepath.Join(o.DataRoot, "memory"), 0700, ContainerUID, ContainerGID}, {filepath.Join(o.DataRoot, "runner"), 0700, zoeRunner, zoeGroup},
		{filepath.Join(o.DataRoot, "runner/workspaces"), 0700, zoeRunner, zoeGroup},
		{filepath.Join(o.DataRoot, "runner-authority"), 0700, zoeRunner, zoeGroup}, {filepath.Join(o.Root, "secrets"), 0700, 0, 0},
		{filepath.Join(o.Root, "runner-secrets"), 0750, 0, zoeGroup}, {filepath.Join(o.DataRoot, "opencode"), 0700, 0, 0},
	}
	if o.Root == "/etc/zoe" {
		dirs = append(dirs, struct {
			path     string
			mode     os.FileMode
			uid, gid int
		}{"/var/cache/zoe/builds", 0750, 0, 0})
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return step("directory "+d.path, err)
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return step("mode "+d.path, err)
		}
		if err := hostPrepChown(d.path, d.uid, d.gid); err != nil {
			return step("owner "+d.path, err)
		}
	}
	for _, sub := range []string{"xdg-config/opencode", "xdg-data", "xdg-state", "xdg-cache"} {
		p := filepath.Join(o.DataRoot, "opencode", sub)
		if err := os.MkdirAll(p, 0700); err != nil {
			return step("directory "+p, err)
		}
		if err := os.Chmod(p, 0700); err != nil {
			return step("mode "+p, err)
		}
		if err := hostPrepChown(p, 0, 0); err != nil {
			return step("owner "+p, err)
		}
	}
	authority := filepath.Join(o.DataRoot, "runner-authority/runner-deployment-authority.db")
	if info, err := os.Lstat(authority); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("prepare host runner authority: must not be a symlink")
	} else if os.IsNotExist(err) {
		if err := os.WriteFile(authority, nil, 0600); err != nil {
			return step("runner authority", err)
		}
	}
	if err := os.Chmod(authority, 0600); err != nil {
		return step("runner authority mode", err)
	}
	if err := hostPrepChown(authority, zoeRunner, zoeGroup); err != nil {
		return step("runner authority owner", err)
	}
	if o.Root == "/etc/zoe" {
		tmpfiles := "f /run/lock/zoe-runner-deployments.lock 0600 zoe-runner zoe-runner -\n"
		if err := os.WriteFile("/etc/tmpfiles.d/zoe-runner.conf", []byte(tmpfiles), 0644); err != nil {
			return step("tmpfiles configuration", err)
		}
		if _, err := m.Run(ctx, "systemd-tmpfiles", "--create", "/etc/tmpfiles.d/zoe-runner.conf"); err != nil {
			return step("tmpfiles", err)
		}
	}
	if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return step("systemd reload", err)
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
	for _, n := range []string{"zoe-compose.service", "zoe-runner.service", "zoe-opencode.service"} {
		b, e := os.ReadFile(filepath.Join(o.OptRoot, "releases", "current", "deploy-linux", n))
		if e != nil {
			return e
		}
		if n == "zoe-opencode.service" {
			b, e = m.renderOpenCodeUnit(context.Background(), o, b)
			if e != nil {
				return e
			}
		} else {
			b = bytesReplacePaths(b, o)
		}
		if e = os.WriteFile(filepath.Join(dir, n), b, 0644); e != nil {
			return e
		}
	}
	_, e := m.Run(context.Background(), "systemctl", "daemon-reload")
	return e
}

func (m Manager) startOpenCode(ctx context.Context, o LifecycleOptions) error {
	if _, err := m.Run(ctx, "systemctl", "enable", "--now", "zoe-opencode.service"); err != nil {
		return fmt.Errorf("start zoe-opencode.service: %w", err)
	}
	return nil
}

func waitOpenCode(ctx context.Context, o LifecycleOptions, m Manager, timeout time.Duration) error {
	password, err := os.ReadFile(filepath.Join(o.DataRoot, "opencode", "password"))
	if err != nil {
		return err
	}
	defer func() {
		for i := range password {
			password[i] = 0
		}
	}()
	url := "http://127.0.0.1:4096/api/info"
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.SetBasicAuth("opencode", strings.TrimSpace(string(password)))
		if r, e := http.DefaultClient.Do(req); e == nil {
			r.Body.Close()
			if r.StatusCode == http.StatusOK {
				return nil
			}
		}
		if _, e := m.Run(ctx, "systemctl", "is-active", "zoe-opencode.service"); e != nil { /* keep waiting for systemd startup */
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("zoe-opencode.service health check timed out")
}

func openCodeActive(ctx context.Context, o LifecycleOptions, _ Manager) (bool, error) {
	password, err := os.ReadFile(filepath.Join(o.DataRoot, "opencode", "password"))
	if err != nil {
		return false, err
	}
	defer func() {
		for i := range password {
			password[i] = 0
		}
	}()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:4096/api/session/active", nil)
	req.SetBasicAuth("opencode", strings.TrimSpace(string(password)))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return false, fmt.Errorf("OpenCode active sessions returned HTTP %d", r.StatusCode)
	}
	var payload struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return false, err
	}
	return len(payload.Data) > 0, nil
}
func bytesReplacePaths(b []byte, o LifecycleOptions) []byte {
	s := strings.ReplaceAll(strings.ReplaceAll(string(b), "/etc/zoe", o.Root), "/opt/zoe", o.OptRoot)
	if o.DataRoot != "" {
		s = strings.ReplaceAll(s, "/var/lib/zoe", o.DataRoot)
	}
	if o.OpenCodeBinary != "" {
		s = strings.ReplaceAll(s, "/root/.opencode/bin/opencode", o.OpenCodeBinary)
	}
	return []byte(s)
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

func serviceIsActive(ctx context.Context, m Manager, unit string) (bool, error) {
	out, err := m.Run(ctx, "systemctl", "is-active", unit)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "active", nil
}

func waitAllHealth(ctx context.Context, o LifecycleOptions, m Manager, url string, timeout time.Duration) error {
	if url == "" {
		port := readEnvValue(filepath.Join(o.Root, "zoe-stack.env"), "ZOE_SERVER_PORT")
		if _, err := strconv.Atoi(port); err != nil || port == "" {
			port = "8787"
		}
		url = "http://127.0.0.1:" + port + "/healthz"
	}
	deadline := time.Now().Add(timeout)
	lastFailure := "zoe-compose.service"
	for time.Now().Before(deadline) {
		compose, composeErr := serviceIsActive(ctx, m, "zoe-compose.service")
		runner, runnerErr := serviceIsActive(ctx, m, "zoe-runner.service")
		opencode, opencodeErr := serviceIsActive(ctx, m, "zoe-opencode.service")
		controlOK := false
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
			controlOK = r.StatusCode >= 200 && r.StatusCode < 300
		}
		if compose && runner && opencode && controlOK && opencodeInfoHealthy(ctx, o) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			runner, runnerErr = serviceIsActive(ctx, m, "zoe-runner.service")
			opencode, opencodeErr = serviceIsActive(ctx, m, "zoe-opencode.service")
			if runner && opencode && opencodeInfoHealthy(ctx, o) {
				return nil
			}
			if !runner {
				lastFailure = "zoe-runner.service"
			} else if !opencode {
				lastFailure = "zoe-opencode.service"
			} else {
				lastFailure = "zoe-opencode.service health endpoint"
			}
		} else if !compose {
			lastFailure = "zoe-compose.service"
		} else if !opencode || !opencodeInfoHealthy(ctx, o) {
			lastFailure = "zoe-opencode.service"
		} else if !runner {
			lastFailure = "zoe-runner.service"
		} else if !controlOK {
			lastFailure = "zoe-compose.service health endpoint"
		}
		_ = composeErr
		_ = runnerErr
		_ = opencodeErr
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("required service health check timed out: %s", lastFailure)
}

func opencodeInfoHealthy(ctx context.Context, o LifecycleOptions) bool {
	password, err := os.ReadFile(filepath.Join(o.DataRoot, "opencode", "password"))
	if err != nil {
		return false
	}
	defer func() {
		for i := range password {
			password[i] = 0
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:4096/api/info", nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth("opencode", strings.TrimSpace(string(password)))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer r.Body.Close()
	return r.StatusCode == http.StatusOK
}
func waitHealth(ctx context.Context, m Manager, o LifecycleOptions, url string, timeout time.Duration) error {
	if url == "" {
		port := readEnvValue(filepath.Join(o.Root, "zoe-stack.env"), "ZOE_SERVER_PORT")
		if _, err := strconv.Atoi(port); err != nil || port == "" {
			port = "8787"
		}
		url = "http://127.0.0.1:" + port + "/healthz"
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
	plan := UninstallPlan{Units: []string{"zoe-compose.service", "zoe-runner.service", "zoe-opencode.service", "zoe-compose.service.d/*", "zoe-runner.service.d/*", "zoe-runner.conf"}, Network: "zoe", OptRoot: o.OptRoot, DataRoot: o.DataRoot}
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
	plan.KeepFiles = []string{filepath.Join(o.Root, "secrets", "cliproxyapi-api-key"), filepath.Join(o.Root, "runner-secrets", "cliproxyapi-api-key"), filepath.Join(o.Root, "secrets", "voice-gateway-api-key"), filepath.Join(o.Root, "secrets", "mem0-llm-api-key")}
	plan.RemoveFiles = []string{filepath.Join(o.DataRoot, "opencode"), filepath.Join(o.Root, "zoe-opencode.env"), filepath.Join(openCodeUnitDir(o), "zoe-opencode.service")}
	if o.PurgeData {
		plan.RemoveFiles = append(plan.RemoveFiles, o.DataRoot, filepath.Join(o.Root, "zoe-stack.env"), filepath.Join(o.Root, "zoe-runner.env"), filepath.Join(o.Root, "secrets", "runner-token"), filepath.Join(o.Root, "secrets", "memory-runner-token"))
	}
	return plan, nil
}

func (m Manager) Uninstall(ctx context.Context, o LifecycleOptions) error {
	if o.Root == "" {
		o.Root = "/etc/zoe"
	}
	if o.OptRoot == "" {
		o.OptRoot = "/opt/zoe"
	}
	if o.DataRoot == "" {
		o.DataRoot = "/var/lib/zoe"
	}
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
	run("stop units", "systemctl", "stop", "zoe-opencode.service", "zoe-runner.service", "zoe-compose.service")
	run("disable units", "systemctl", "disable", "zoe-opencode.service", "zoe-runner.service", "zoe-compose.service")
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
	paths := []string{filepath.Join(openCodeUnitDir(o), "zoe-compose.service"), filepath.Join(openCodeUnitDir(o), "zoe-runner.service"), filepath.Join(openCodeUnitDir(o), "zoe-opencode.service"), filepath.Join(o.Root, "zoe-opencode.env")}
	if o.Root == "/etc/zoe" {
		paths = append(paths, "/etc/tmpfiles.d/zoe-runner.conf")
	}
	for _, p := range paths {
		if err = os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	if err = os.RemoveAll(filepath.Join(o.DataRoot, "opencode")); err != nil {
		errs = append(errs, err)
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
