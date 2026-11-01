package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/runner"
)

func TestWriteEnvPreservesExistingValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zoe.env")
	if err := os.WriteFile(path, []byte("TOKEN=operator-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeEnv(path, map[string]string{"TOKEN": "generated-value"}, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "TOKEN=operator-value\n" {
		t.Fatalf("existing env was rewritten: %q", data)
	}
}

func TestWriteEnvMergesReleaseValuesAndPreservesOperatorLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zoe.env")
	original := "# operator comment\nZOE_SERVER_IMAGE=old\nZOE_VOICE_LANGUAGE=en\nUNKNOWN=value\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeEnv(path, map[string]string{"ZOE_SERVER_IMAGE": "new", "ZOE_COMPOSE_FILE": "/new/compose.yml"}, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# operator comment\nZOE_SERVER_IMAGE=new\nZOE_VOICE_LANGUAGE=en\nUNKNOWN=value\nZOE_COMPOSE_FILE=/new/compose.yml\n"
	if string(data) != want {
		t.Fatalf("merged env = %q, want %q", data, want)
	}
	if mode := dataMode(t, path); mode != 0600 {
		t.Fatalf("mode = %04o, want 0600", mode)
	}
}

func TestDefaultProfileValuesRoundTripAndShareKeys(t *testing.T) {
	profilesJSON, routesJSON, err := defaultProfileValues(defaultOpenCodeModel)
	if err != nil {
		t.Fatal(err)
	}
	profiles := map[string]control.RuntimeProfile{}
	routes := map[string]runner.Route{}
	if err := json.Unmarshal([]byte(profilesJSON), &profiles); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(routesJSON), &routes); err != nil {
		t.Fatal(err)
	}
	if err := validateDefaultValues(profiles, routes); err != nil {
		t.Fatal(err)
	}
	if profiles["worker"].ID != "worker" || routes["worker"].ProfileID != "worker" {
		t.Fatalf("defaults do not select worker: profiles=%v routes=%v", profiles, routes)
	}
	if len(profiles) != len(routes) {
		t.Fatalf("profile and route key counts differ: %d/%d", len(profiles), len(routes))
	}
	for id := range profiles {
		if _, ok := routes[id]; !ok {
			t.Fatalf("route key set does not contain profile %q", id)
		}
	}
}

func TestWriteEnvSelfHealsProfileValuesButPreservesValidCustomValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zoe.env")
	profiles, routes, err := defaultProfileValues("provider/model")
	if err != nil {
		t.Fatal(err)
	}
	customProfiles := `{"custom":{"id":"custom","harness":"opencode","runtime_route_id":"custom-runtime","model_route_id":"custom-model","role":"worker","isolation_policy":"opencode-workspace-write-v1","capability_ids":[],"scopes":[]}}`
	customRoutes := `{"custom":{"profile_id":"custom","runtime_route_id":"custom-runtime","model_route_id":"custom-model","isolation_policy":"opencode-workspace-write-v1","harness":"opencode","model":"custom/model","approval_mode":"accept-edits"}}`
	values := map[string]string{"ZOE_RUNTIME_PROFILES": profiles, "ZOE_DEFAULT_PROFILE_ID": "worker", "ZOE_RUNNER_ROUTES": routes}
	if err := os.WriteFile(path, []byte("# keep\nZOE_RUNTIME_PROFILES="+customProfiles+"\nZOE_DEFAULT_PROFILE_ID=custom\nZOE_RUNNER_ROUTES="+customRoutes+"\nUNKNOWN=kept\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeEnv(path, values, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ZOE_RUNTIME_PROFILES="+customProfiles) || !strings.Contains(string(data), "ZOE_RUNNER_ROUTES="+customRoutes) || !strings.Contains(string(data), "ZOE_DEFAULT_PROFILE_ID=custom") || !strings.Contains(string(data), "UNKNOWN=kept") {
		t.Fatalf("valid operator values were not preserved: %q", data)
	}

	if err := os.WriteFile(path, []byte("ZOE_RUNTIME_PROFILES={}\nZOE_DEFAULT_PROFILE_ID=\nZOE_RUNNER_ROUTES={}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeEnv(path, values, 0600); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ZOE_RUNTIME_PROFILES="+profiles) || !strings.Contains(string(data), "ZOE_RUNNER_ROUTES="+routes) || !strings.Contains(string(data), "ZOE_DEFAULT_PROFILE_ID=worker") {
		t.Fatalf("empty operator values did not self-heal: %q", data)
	}
	if mode := dataMode(t, path); mode != 0600 {
		t.Fatalf("mode = %04o, want 0600", mode)
	}
}

func TestWriteEnvFreshFileGetsWorkingProfileDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zoe.env")
	profiles, routes, err := defaultProfileValues(defaultOpenCodeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEnv(path, map[string]string{
		"ZOE_RUNTIME_PROFILES":   profiles,
		"ZOE_DEFAULT_PROFILE_ID": "worker",
		"ZOE_RUNNER_ROUTES":      routes,
	}, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ZOE_RUNTIME_PROFILES="+profiles) || !strings.Contains(string(data), "ZOE_DEFAULT_PROFILE_ID=worker") || !strings.Contains(string(data), "ZOE_RUNNER_ROUTES="+routes) {
		t.Fatalf("fresh env did not receive defaults: %q", data)
	}
}

func dataMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestEnsureNetworkCreatesOnlyWhenMissing(t *testing.T) {
	created := false
	m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "docker" && args[0] == "network" && args[1] == "inspect" {
			return nil, errors.New("missing")
		}
		if name == "docker" && args[0] == "network" && args[1] == "create" {
			created = true
		}
		return nil, nil
	})
	if err := m.ensureNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("network was not created")
	}
}

func TestUninstallMidwayFailureLeavesReleaseRootForRetry(t *testing.T) {
	root, opt := t.TempDir(), filepath.Join(t.TempDir(), "opt")
	if err := os.MkdirAll(filepath.Join(opt, "releases", "current"), 0755); err != nil {
		t.Fatal(err)
	}
	var failNetworkRemoval = true
	m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case name == "docker" && len(args) >= 2 && args[0] == "ps":
			return nil, nil
		case name == "docker" && len(args) >= 3 && args[0] == "network" && args[1] == "rm" && failNetworkRemoval:
			failNetworkRemoval = false
			return []byte("temporary failure"), errors.New("failed")
		default:
			return nil, nil
		}
	})
	o := LifecycleOptions{Root: root, OptRoot: opt, Out: io.Discard}
	if err := m.Uninstall(context.Background(), o); err == nil {
		t.Fatal("expected first uninstall to fail")
	}
	if _, err := os.Stat(opt); err != nil {
		t.Fatalf("release root removed after failure: %v", err)
	}
	if err := m.Uninstall(context.Background(), o); err != nil {
		t.Fatalf("retry uninstall: %v", err)
	}
	if _, err := os.Stat(opt); !os.IsNotExist(err) {
		t.Fatalf("release root still exists: %v", err)
	}
}

func TestStageReleaseKeepsDeploymentFilesUnderDeployLinux(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "release")
	if err := os.MkdirAll(filepath.Join(src, "deploy-linux"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "deploy-linux", "docker-compose.yml"), []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := copyDeployFiles(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "deploy-linux", "docker-compose.yml")); err != nil {
		t.Fatalf("deployment file was not staged at the unit's relative path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "docker-compose.yml")); !os.IsNotExist(err) {
		t.Fatalf("deployment file was unexpectedly flattened into release root")
	}
}

func TestPruneReleasesKeepsInstalledCLI(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"current", "previous", "current-rev", "old-release", "zoe-cli-aaaaaaaaaaaa"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}

	if err := pruneReleases(dir, filepath.Join(dir, "previous"), "current-rev"); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"current", "previous", "current-rev", "zoe-cli-aaaaaaaaaaaa"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "old-release")); !os.IsNotExist(err) {
		t.Errorf("old release still exists or returned an unexpected error: %v", err)
	}
}

func TestUpdateRollbackReportsFailingStepAndCommandOutput(t *testing.T) {
	root := t.TempDir()
	opt := filepath.Join(t.TempDir(), "opt")
	releases := filepath.Join(opt, "releases")
	if err := os.MkdirAll(releases, 0755); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(releases, "previous")
	if err := os.MkdirAll(previous, 0755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(releases, "current")
	if err := os.Symlink(previous, current); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"zoe-stack.env", "zoe-runner.env"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("VALUE=old\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	composeRestarts := 0
	m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) >= 2 && args[0] == "restart" && args[1] == "zoe-compose.service" {
			composeRestarts++
			if composeRestarts == 1 {
				return []byte("compose file missing\n"), errors.New("exit status 1")
			}
		}
		return nil, nil
	})
	// Exercise the same rollback path used by Update without needing a valid
	// release manifest or Docker pulls.
	stack, err := snapshot(filepath.Join(root, "zoe-stack.env"))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := snapshot(filepath.Join(root, "zoe-runner.env"))
	if err != nil {
		t.Fatal(err)
	}
	_, cause := m.Run(context.Background(), "systemctl", "restart", "zoe-compose.service")
	if cause == nil {
		t.Fatal("expected compose restart to fail")
	}
	err = restoreUpdate(context.Background(), m, LifecycleOptions{Out: &out}, current, "previous", stack, runner, fmt.Errorf("start zoe-compose.service: %w", cause))
	if err == nil || !strings.Contains(err.Error(), "update rolled back") || !strings.Contains(err.Error(), "start zoe-compose.service") {
		t.Fatalf("rollback error = %v", err)
	}
	if !strings.Contains(out.String(), "compose file missing") {
		t.Fatalf("operator output = %q", out.String())
	}
	for _, name := range []string{"zoe-stack.env", "zoe-runner.env"} {
		data, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil || string(data) != "VALUE=old\n" {
			t.Fatalf("rollback did not restore %s byte-for-byte: %q, %v", name, data, readErr)
		}
	}
}

type updateFixture struct {
	root, opt, data, release, binary string
	previous, revision               string
}

func newUpdateFixture(t *testing.T) updateFixture {
	t.Helper()
	t.Cleanup(func() {
		hostPrepEUID = os.Geteuid
		hostPrepChown = func(path string, uid, gid int) error { return os.Chown(path, uid, gid) }
		hostPrepLookupZoeRunnerIDs = func() (int, int, error) { return 1000, 1000, nil }
	})
	hostPrepEUID = func() int { return 0 }
	hostPrepChown = func(string, int, int) error { return nil }
	hostPrepLookupZoeRunnerIDs = func() (int, int, error) { return 1000, 1000, nil }
	f := updateFixture{
		root: t.TempDir(), opt: filepath.Join(t.TempDir(), "opt"), data: filepath.Join(t.TempDir(), "data"),
		release: filepath.Join(t.TempDir(), "release"), binary: filepath.Join(t.TempDir(), "opencode"),
		revision: strings.Repeat("b", 40),
	}
	f.previous = filepath.Join(f.opt, "releases", "previous")
	if err := os.MkdirAll(f.previous, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("previous", filepath.Join(f.opt, "releases", "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "zoe-stack.env"), []byte("STACK=old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "zoe-runner.env"), []byte("RUNNER=old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.binary, []byte("fake binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.release, "deploy-linux"), 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"bin/zoe-secret-entrypoint":         []byte("entrypoint\n"),
		"bin/zoe-server":                    []byte("server\n"),
		"bin/zoe-runner":                    []byte("runner\n"),
		"bin/zoe":                           []byte("cli\n"),
		"deploy-linux/docker-compose.yml":   []byte("services: {}\n"),
		"deploy-linux/zoe-compose.service":  []byte("[Service]\n"),
		"deploy-linux/zoe-runner.service":   []byte("[Service]\n"),
		"deploy-linux/zoe-opencode.service": []byte("[Service]\nExecStart=/root/.opencode/bin/opencode serve\n"),
	}
	artifacts := make([]ReleaseArtifact, 0, len(files))
	for name, data := range files {
		path := filepath.Join(f.release, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		artifacts = append(artifacts, ReleaseArtifact{Name: filepath.Base(name), Path: name, SHA256: hex.EncodeToString(sum[:])})
	}
	manifest := ReleaseManifest{
		SourceRevision: f.revision, Architecture: runtime.GOARCH, Artifacts: artifacts,
		Images: map[string]ReleaseImage{
			"zoe-server":        {Ref: "ghcr.io/test/zoe-server:test", Digest: "sha256:" + strings.Repeat("1", 64)},
			"zoe-memory-worker": {Ref: "ghcr.io/test/zoe-memory:test", Digest: "sha256:" + strings.Repeat("2", 64)},
			"zoe-runner":        {Ref: "ghcr.io/test/zoe-runner:test", Digest: "sha256:" + strings.Repeat("3", 64)},
		},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.release, "release-manifest.json"), manifestData, 0644); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPrepareHostCreatesFreshLayoutAndIsIdempotent(t *testing.T) {
	root, opt, data := t.TempDir(), filepath.Join(t.TempDir(), "opt"), filepath.Join(t.TempDir(), "data")
	oldEUID, oldChown := hostPrepEUID, hostPrepChown
	t.Cleanup(func() { hostPrepEUID, hostPrepChown = oldEUID, oldChown })
	hostPrepEUID = func() int { return 0 }
	hostPrepChown = func(string, int, int) error { return nil }
	var commands []string
	groupExists, userExists, networkExists := false, false, false
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, strings.Join(append([]string{name}, args...), " "))
		if name == "getent" && args[1] == "docker" {
			return nil, nil
		}
		if name == "getent" && !groupExists {
			groupExists = true
			return nil, errors.New("missing")
		}
		if name == "id" && !userExists {
			userExists = true
			return nil, errors.New("missing")
		}
		if name == "docker" && args[1] == "inspect" && !networkExists {
			networkExists = true
			return nil, errors.New("missing")
		}
		if name == "groupadd" {
			groupExists = true
		}
		if name == "useradd" {
			userExists = true
		}
		if name == "docker" && args[1] == "create" {
			networkExists = true
		}
		return nil, nil
	}
	m := NewManager(run)
	o := LifecycleOptions{Root: root, OptRoot: opt, DataRoot: data}
	if err := m.prepareHost(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(opt, "releases"), filepath.Join(data, "server/data"), filepath.Join(data, "memory"), filepath.Join(data, "runner"), filepath.Join(root, "secrets"), filepath.Join(root, "runner-secrets"), filepath.Join(data, "runner-authority")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing %s: %v", path, err)
		}
	}
	if mode := dataMode(t, filepath.Join(data, "runner-authority/runner-deployment-authority.db")); mode != 0600 {
		t.Errorf("authority mode=%04o", mode)
	}
	first := len(commands)
	if err := m.prepareHost(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for _, command := range commands[first:] {
		if strings.Contains(command, "groupadd") || strings.Contains(command, "useradd") || strings.Contains(command, "network create") {
			t.Fatalf("second preparation recreated host state: %v", commands[first:])
		}
	}
	for _, want := range []string{"groupadd --system zoe-runner", "useradd --system --gid", "usermod --append --groups docker zoe-runner", "docker network create zoe"} {
		if !containsCommand(commands, want) {
			t.Errorf("missing command %q in %v", want, commands)
		}
	}
}

func TestPrepareHostRequiresRoot(t *testing.T) {
	old := hostPrepEUID
	t.Cleanup(func() { hostPrepEUID = old })
	hostPrepEUID = func() int { return 501 }
	if err := (Manager{}).prepareHost(context.Background(), LifecycleOptions{}); err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("error=%v", err)
	}
}

func TestPrepareLifecycleFilesFreshTokensSetOwnersAndModes(t *testing.T) {
	root := t.TempDir()
	oldChown, oldLookup := hostPrepChown, hostPrepLookupZoeRunnerIDs
	t.Cleanup(func() { hostPrepChown, hostPrepLookupZoeRunnerIDs = oldChown, oldLookup })
	var owners []struct {
		path     string
		uid, gid int
	}
	hostPrepChown = func(path string, uid, gid int) error {
		owners = append(owners, struct {
			path     string
			uid, gid int
		}{path, uid, gid})
		return nil
	}
	hostPrepLookupZoeRunnerIDs = func() (int, int, error) { return 4242, 4343, nil }

	if err := prepareLifecycleFiles(LifecycleOptions{Root: root, OptRoot: filepath.Join(root, "opt"), DataRoot: filepath.Join(root, "data")}); err != nil {
		t.Fatal(err)
	}
	wantOwners := map[string][2]int{
		filepath.Join(root, "secrets", "runner-token"):        {65532, 65532},
		filepath.Join(root, "secrets", "memory-runner-token"): {65532, 65532},
		filepath.Join(root, "runner-secrets", "runner-token"): {4242, 4343},
	}
	if len(owners) != len(wantOwners) {
		t.Fatalf("chown calls = %d, want %d: %v", len(owners), len(wantOwners), owners)
	}
	for _, owner := range owners {
		if want, ok := wantOwners[owner.path]; !ok || [2]int{owner.uid, owner.gid} != want {
			t.Fatalf("unexpected owner call: %+v", owner)
		}
	}
	for path := range wantOwners {
		if mode := dataMode(t, path); mode != 0400 {
			t.Errorf("%s mode = %04o, want 0400", path, mode)
		}
	}
	stackToken, err := os.ReadFile(filepath.Join(root, "secrets", "runner-token"))
	if err != nil {
		t.Fatal(err)
	}
	runnerToken, err := os.ReadFile(filepath.Join(root, "runner-secrets", "runner-token"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stackToken, runnerToken) {
		t.Fatal("runner token copy differs from stack token")
	}
}

func TestPrepareLifecycleFilesConvergesExistingRootOwnedTokens(t *testing.T) {
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	runnerSecrets := filepath.Join(root, "runner-secrets")
	if err := os.MkdirAll(secrets, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runnerSecrets, 0750); err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{
		filepath.Join(secrets, "runner-token"):        []byte("existing-runner-token"),
		filepath.Join(secrets, "memory-runner-token"): []byte("existing-memory-token"),
		filepath.Join(runnerSecrets, "runner-token"):  []byte("existing-runner-token"),
	}
	for path, data := range want {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldChown, oldLookup := hostPrepChown, hostPrepLookupZoeRunnerIDs
	t.Cleanup(func() { hostPrepChown, hostPrepLookupZoeRunnerIDs = oldChown, oldLookup })
	var owners []struct {
		path     string
		uid, gid int
	}
	hostPrepChown = func(path string, uid, gid int) error {
		owners = append(owners, struct {
			path     string
			uid, gid int
		}{path, uid, gid})
		return nil
	}
	hostPrepLookupZoeRunnerIDs = func() (int, int, error) { return 4242, 4343, nil }

	if err := prepareLifecycleFiles(LifecycleOptions{Root: root, OptRoot: filepath.Join(root, "opt"), DataRoot: filepath.Join(root, "data")}); err != nil {
		t.Fatal(err)
	}
	for path, original := range want {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, original) {
			t.Errorf("%s content changed", path)
		}
		if mode := dataMode(t, path); mode != 0400 {
			t.Errorf("%s mode = %04o, want 0400", path, mode)
		}
	}
	wantOwners := map[string][2]int{
		filepath.Join(secrets, "runner-token"):        {65532, 65532},
		filepath.Join(secrets, "memory-runner-token"): {65532, 65532},
		filepath.Join(runnerSecrets, "runner-token"):  {4242, 4343},
	}
	if len(owners) != len(wantOwners) {
		t.Fatalf("chown calls = %d, want %d: %v", len(owners), len(wantOwners), owners)
	}
	for _, owner := range owners {
		if want, ok := wantOwners[owner.path]; !ok || [2]int{owner.uid, owner.gid} != want {
			t.Fatalf("unexpected owner call: %+v", owner)
		}
	}
}

func TestWaitHealthUsesConfiguredServerPort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_SERVER_PORT="+port+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" {
			return []byte("active\n"), nil
		}
		return nil, nil
	})
	if err := waitHealth(context.Background(), m, LifecycleOptions{Root: root}, "", time.Second); err != nil {
		t.Fatal(err)
	}
}

type updateRunner struct {
	commands     []string
	active       bool
	runnerChecks int
	runnerFlipAt int
}

func (r *updateRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	command := strings.Join(append([]string{name}, args...), " ")
	r.commands = append(r.commands, command)
	if len(args) == 1 && args[0] == "--version" {
		return []byte("opencode v2.2.5-ronigooja.3\n"), nil
	}
	if name == "docker" {
		return nil, nil
	}
	if name != "systemctl" {
		return nil, nil
	}
	if len(args) >= 3 && args[0] == "enable" && args[2] == "zoe-opencode.service" {
		r.active = true
		return nil, nil
	}
	if len(args) >= 2 && (args[0] == "restart" || args[0] == "start") && args[1] == "zoe-opencode.service" {
		r.active = true
		return nil, nil
	}
	if len(args) >= 2 && args[0] == "is-active" {
		switch args[1] {
		case "zoe-opencode.service":
			if !r.active {
				return []byte("failed\n"), errors.New("inactive")
			}
			return []byte("active\n"), nil
		case "zoe-compose.service":
			return []byte("active\n"), nil
		case "zoe-runner.service":
			r.runnerChecks++
			if r.runnerFlipAt > 0 && r.runnerChecks >= r.runnerFlipAt {
				return []byte("failed\n"), errors.New("runner failed")
			}
			return []byte("active\n"), nil
		}
	}
	return nil, nil
}

func (f updateFixture) options(out io.Writer) LifecycleOptions {
	return LifecycleOptions{Root: f.root, OptRoot: f.opt, DataRoot: f.data, Release: f.release, OpenCodeBinary: f.binary, HealthTimeout: 2 * time.Second, Out: out}
}

func fixedUpdateServers(t *testing.T, passwordPath string, sessions bool, infoOK bool) (close func()) {
	t.Helper()
	var err error
	info := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		password, _ := os.ReadFile(passwordPath)
		if !ok || user != "opencode" || pass != strings.TrimSpace(string(password)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !infoOK {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	info.Listener, err = net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Fatal(err)
	}
	info.Start()
	active := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sessions {
			_, _ = w.Write([]byte(`{"data":{"session-1":{}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	active.Listener, err = net.Listen("tcp", "127.0.0.1:4097")
	if err != nil {
		info.Close()
		t.Fatal(err)
	}
	active.Start()
	control := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	control.Listener, err = net.Listen("tcp", "127.0.0.1:8787")
	if err != nil {
		info.Close()
		active.Close()
		t.Fatal(err)
	}
	control.Start()
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/api/session/active") {
			r.URL.Host = "127.0.0.1:4097"
		}
		return oldTransport.RoundTrip(r)
	})
	return func() { http.DefaultTransport = oldTransport; info.Close(); active.Close(); control.Close() }
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdatePreparesOpenCodeFilesIdempotently(t *testing.T) {
	f := newUpdateFixture(t)
	runner := &updateRunner{}
	m := NewManager(runner.run)
	closeServers := fixedUpdateServers(t, filepath.Join(f.data, "opencode", "password"), false, true)
	defer closeServers()
	if err := m.Update(context.Background(), f.options(nil)); err != nil {
		t.Fatal(err)
	}
	passwordPath := filepath.Join(f.data, "opencode", "password")
	first, err := os.ReadFile(passwordPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := dataMode(t, passwordPath); mode != 0600 {
		t.Fatalf("password mode = %04o", mode)
	}
	for _, path := range []string{"xdg-data", "xdg-state", "xdg-cache"} {
		if _, err := os.Stat(filepath.Join(f.data, "opencode", path)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.data, "runner", "workspaces")); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(context.Background(), f.options(nil)); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(passwordPath)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("password changed: %v", err)
	}
}

func TestUpdateStartsInactiveOpenCodeAndWaitsForAuthenticatedInfo(t *testing.T) {
	f := newUpdateFixture(t)
	runner := &updateRunner{}
	closeServers := fixedUpdateServers(t, filepath.Join(f.data, "opencode", "password"), false, true)
	defer closeServers()
	if err := NewManager(runner.run).Update(context.Background(), f.options(nil)); err != nil {
		t.Fatal(err)
	}
	if !containsCommand(runner.commands, "systemctl enable --now zoe-opencode.service") {
		t.Fatalf("OpenCode was not started: %v", runner.commands)
	}
}

func TestUpdateSkipsRestartWithActiveOpenCodeSessions(t *testing.T) {
	f := newUpdateFixture(t)
	runner := &updateRunner{active: true}
	closeServers := fixedUpdateServers(t, filepath.Join(f.data, "opencode", "password"), true, true)
	defer closeServers()
	if err := NewManager(runner.run).Update(context.Background(), f.options(nil)); err != nil {
		t.Fatal(err)
	}
	if containsCommand(runner.commands, "systemctl restart zoe-opencode.service") {
		t.Fatalf("OpenCode was restarted: %v", runner.commands)
	}
}

func TestUpdateRollsBackWhenOpenCodeNeverBecomesHealthy(t *testing.T) {
	f := newUpdateFixture(t)
	oldStack, _ := os.ReadFile(filepath.Join(f.root, "zoe-stack.env"))
	oldRunner, _ := os.ReadFile(filepath.Join(f.root, "zoe-runner.env"))
	runner := &updateRunner{}
	closeServers := fixedUpdateServers(t, filepath.Join(f.data, "opencode", "password"), false, false)
	defer closeServers()
	err := NewManager(runner.run).Update(context.Background(), f.options(nil))
	if err == nil || !strings.Contains(err.Error(), "zoe-opencode.service") {
		t.Fatalf("Update error = %v, want named OpenCode unit", err)
	}
	if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "runner-token") {
		t.Fatalf("Update error contains secret material: %v", err)
	}
	current, readErr := os.Readlink(filepath.Join(f.opt, "releases", "current"))
	if readErr != nil || current != "previous" {
		t.Fatalf("current selector = %q, %v", current, readErr)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "zoe-stack.env")); !bytes.Equal(got, oldStack) {
		t.Fatalf("stack not restored: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "zoe-runner.env")); !bytes.Equal(got, oldRunner) {
		t.Fatalf("runner env not restored: %q", got)
	}
}

func TestUpdateRollsBackWhenRunnerFlipsFailedDuringStabilityWindow(t *testing.T) {
	f := newUpdateFixture(t)
	oldStack, _ := os.ReadFile(filepath.Join(f.root, "zoe-stack.env"))
	oldRunner, _ := os.ReadFile(filepath.Join(f.root, "zoe-runner.env"))
	runner := &updateRunner{runnerFlipAt: 2}
	closeServers := fixedUpdateServers(t, filepath.Join(f.data, "opencode", "password"), false, true)
	defer closeServers()
	err := NewManager(runner.run).Update(context.Background(), f.options(nil))
	if err == nil || !strings.Contains(err.Error(), "zoe-runner.service") {
		t.Fatalf("Update error = %v", err)
	}
	current, readErr := os.Readlink(filepath.Join(f.opt, "releases", "current"))
	if readErr != nil || current != "previous" {
		t.Fatalf("current selector = %q, %v", current, readErr)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "zoe-stack.env")); !bytes.Equal(got, oldStack) {
		t.Fatalf("stack not restored: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "zoe-runner.env")); !bytes.Equal(got, oldRunner) {
		t.Fatalf("runner env not restored: %q", got)
	}
}

func TestUpdateMigratesRunnerOpenCodeEnvironment(t *testing.T) {
	f := newUpdateFixture(t)
	legacy := "ZOE_OPENCODE_IMAGE=old\nZOE_OPENCODE_PROVIDER_SOCKET=old-socket\nUNKNOWN_OPERATOR=keep\n"
	if err := os.WriteFile(filepath.Join(f.root, "zoe-runner.env"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &updateRunner{}
	closeServers := fixedUpdateServers(t, filepath.Join(f.data, "opencode", "password"), false, true)
	defer closeServers()
	if err := NewManager(runner.run).Update(context.Background(), f.options(nil)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.root, "zoe-runner.env"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, old := range []string{"ZOE_OPENCODE_IMAGE", "ZOE_OPENCODE_PROVIDER_SOCKET"} {
		if strings.Contains(text, old) {
			t.Fatalf("legacy key remains: %q", text)
		}
	}
	for _, key := range []string{"ZOE_OPENCODE_BASE_URL", "ZOE_OPENCODE_PASSWORD_FILE", "ZOE_OPENCODE_BINARY"} {
		if !strings.Contains(text, key+"=") {
			t.Fatalf("missing %s: %q", key, text)
		}
	}
	if !strings.Contains(text, "UNKNOWN_OPERATOR=keep") {
		t.Fatalf("unknown line was not preserved: %q", text)
	}
}

func containsCommand(commands []string, want string) bool {
	for _, command := range commands {
		if command == want || strings.HasPrefix(command, want+" ") {
			return true
		}
	}
	return false
}
