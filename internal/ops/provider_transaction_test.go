package ops

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testDeployment(t *testing.T) Deployment {
	t.Helper()
	root := t.TempDir()
	return Deployment{Root: root, Mode: ModeSystemd, RunnerService: "zoe-runner.service", RunnerEnv: filepath.Join(root, "zoe-runner.env"), StackEnv: filepath.Join(root, "stack.env"), ComposeProject: "zoe", DropInPath: filepath.Join(root, "zoe-runner.service.d", "90-zoe-provider.conf")}
}
func modelServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer next" {
			t.Error("staged credential was not sent")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	t.Cleanup(s.Close)
	return s
}
func TestProviderTransactionPreservesConfigAndTargetsOnlyRunner(t *testing.T) {
	d := testDeployment(t)
	server := modelServer(t)
	secret := filepath.Join(d.Root, "token")
	_ = os.WriteFile(secret, []byte("old"), 0600)
	_ = os.WriteFile(d.RunnerEnv, []byte("KEEP=original\nZOE_OPENCODE_MODEL=old\n"), 0600)
	var commands [][]string
	m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, append([]string{name}, args...))
		return nil, nil
	})
	result, err := m.ConfigureProvider(context.Background(), d, ProviderConfig{BaseURL: server.URL, Model: "test-model", SecretPath: secret, Secret: "next"}, server.Client())
	if err != nil || !result.OK() {
		t.Fatalf("apply: %v %+v", err, result)
	}
	contents, _ := os.ReadFile(d.RunnerEnv)
	if !strings.Contains(string(contents), "KEEP=original") || strings.Contains(string(contents), "=old") {
		t.Fatal("unrelated environment lost or stale provider survived")
	}
	got, _ := os.ReadFile(secret)
	if string(got) != "next" {
		t.Fatal("credential not replaced")
	}
	expected := []string{"systemctl", "reload-or-restart", "zoe-runner.service"}
	if !reflect.DeepEqual(commands[len(commands)-1], expected) {
		t.Fatalf("targeted reload args: %v", commands)
	}
	restored, err := DiscoverDeployment(context.Background(), m, d.Root)
	if err != nil || restored.RunnerService != "zoe-runner.service" {
		t.Fatal("managed deployment not rediscovered")
	}
	if LastProviderCheck(d) == "not checked" {
		t.Fatal("missing safe check metadata")
	}
}
func TestProviderTransactionRestoresAfterPartialReload(t *testing.T) {
	d := testDeployment(t)
	server := modelServer(t)
	secret := filepath.Join(d.Root, "token")
	_ = os.WriteFile(secret, []byte("old"), 0600)
	_ = os.WriteFile(d.RunnerEnv, []byte("KEEP=old\n"), 0640)
	calls := 0
	m := NewManager(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "reload-or-restart") {
			calls++
			if calls == 1 {
				return nil, errors.New("partial failure")
			}
		}
		return nil, nil
	})
	_, err := m.ConfigureProvider(context.Background(), d, ProviderConfig{BaseURL: server.URL, Model: "test-model", SecretPath: secret, Secret: "next"}, server.Client())
	if err == nil || !strings.Contains(err.Error(), "partial failure") {
		t.Fatalf("error=%v", err)
	}
	data, _ := os.ReadFile(secret)
	if string(data) != "old" {
		t.Fatal("secret rollback failed")
	}
	data, _ = os.ReadFile(d.RunnerEnv)
	info, _ := os.Stat(d.RunnerEnv)
	if string(data) != "KEEP=old\n" || info.Mode().Perm() != 0640 {
		t.Fatal("environment rollback failed")
	}
	if calls != 2 {
		t.Fatal("old service configuration was not reloaded")
	}
}
func TestRejectedStagedCredentialDoesNotOverwrite(t *testing.T) {
	d := testDeployment(t)
	secret := filepath.Join(d.Root, "token")
	_ = os.WriteFile(secret, []byte("old"), 0600)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }))
	defer s.Close()
	m := NewManager(func(context.Context, string, ...string) ([]byte, error) {
		t.Error("reload before validation")
		return nil, nil
	})
	_, err := m.ConfigureProvider(context.Background(), d, ProviderConfig{BaseURL: s.URL, Model: "m", SecretPath: secret, Secret: "bad"}, s.Client())
	if err == nil {
		t.Fatal("accepted invalid credential")
	}
	b, _ := os.ReadFile(secret)
	if string(b) != "old" {
		t.Fatal("overwrote credential before probe")
	}
}

func TestMemoryTransactionChangesOnlyMemorySettingsAndReloadsWorker(t *testing.T) {
	oldChown, oldUID := chownFile, effectiveUID
	var owners [][3]int
	chownFile = func(path string, uid, gid int) error {
		owners = append(owners, [3]int{uid, gid, 1})
		return nil
	}
	effectiveUID = func() int { return 0 }
	t.Cleanup(func() { chownFile, effectiveUID = oldChown, oldUID })
	root := t.TempDir()
	d := Deployment{Root: root, Mode: ModeCompose, StackEnv: filepath.Join(root, "zoe-stack.env"), ComposeProject: "zoe", ComposeFile: filepath.Join(root, "compose.yml")}
	secret := filepath.Join(root, "memory-token")
	providerSecret := filepath.Join(root, "task-token")
	if err := os.WriteFile(providerSecret, []byte("task"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.StackEnv, []byte("KEEP=original\nZOE_MEM0_LLM_MODEL=old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer next" {
			t.Error("memory credential was not sent")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"memory-model"}]}`))
	}))
	defer server.Close()
	var commands []string
	m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	})
	result, err := m.ConfigureMemory(context.Background(), d, MemoryConfig{BaseURL: server.URL, Model: "memory-model", SecretPath: secret, Secret: "next"}, providerSecret, server.Client())
	if err != nil || !result.OK() {
		t.Fatalf("configure memory: %v %+v", err, result)
	}
	data, err := os.ReadFile(d.StackEnv)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "KEEP=original") || !strings.Contains(text, "ZOE_MEM0_LLM_BASE_URL=\""+server.URL+"\"") || strings.Contains(text, "next") {
		t.Fatalf("unexpected stack environment: %s", text)
	}
	secretData, err := os.ReadFile(secret)
	if err != nil || string(secretData) != "next" {
		t.Fatalf("memory secret = %q (%v)", secretData, err)
	}
	info, err := os.Stat(secret)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("memory secret mode = %v (%v)", info, err)
	}
	if len(owners) != 1 || owners[0][0] != memoryWorkerUID || owners[0][1] != memoryWorkerGID {
		t.Fatalf("memory secret owners = %v", owners)
	}
	if len(commands) != 1 || !strings.Contains(commands[0], "zoe-memory-worker") || strings.Contains(commands[0], "zoe-server") {
		t.Fatalf("reload targeted unexpected services: %v", commands)
	}
}

func TestMemoryTransactionReconfigureKeepsWorkerOwnership(t *testing.T) {
	oldChown, oldUID := chownFile, effectiveUID
	var owners [][2]int
	chownFile = func(path string, uid, gid int) error {
		owners = append(owners, [2]int{uid, gid})
		return nil
	}
	effectiveUID = func() int { return 0 }
	t.Cleanup(func() { chownFile, effectiveUID = oldChown, oldUID })
	root := t.TempDir()
	d := Deployment{Root: root, Mode: ModeCompose, StackEnv: filepath.Join(root, "zoe-stack.env"), ComposeProject: "zoe", ComposeFile: filepath.Join(root, "compose.yml")}
	secret := filepath.Join(root, "memory-token")
	server := modelServer(t)
	m := NewManager(func(context.Context, string, ...string) ([]byte, error) { return nil, nil })
	for _, token := range []string{"next", "next"} {
		result, err := m.ConfigureMemory(context.Background(), d, MemoryConfig{BaseURL: server.URL, Model: "test-model", SecretPath: secret, Secret: token}, filepath.Join(root, "task-token"), server.Client())
		if err != nil || !result.OK() {
			t.Fatalf("configure memory: %v %+v", err, result)
		}
	}
	if len(owners) < 2 {
		t.Fatalf("chown calls = %v", owners)
	}
	for _, owner := range owners {
		if owner != [2]int{memoryWorkerUID, memoryWorkerGID} {
			t.Fatalf("owner = %v", owner)
		}
	}
	if info, err := os.Stat(secret); err != nil || info.Mode().Perm() > 0600 {
		t.Fatalf("memory secret mode = %v (%v)", info, err)
	}
}

func TestMemoryTransactionRollbackLeavesNewFilesAbsent(t *testing.T) {
	root := t.TempDir()
	d := Deployment{Root: root, Mode: ModeCompose, StackEnv: filepath.Join(root, "zoe-stack.env"), ComposeProject: "zoe", ComposeFile: filepath.Join(root, "compose.yml")}
	secret := filepath.Join(root, "memory-token")
	server := modelServer(t)
	m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "docker" && strings.Contains(strings.Join(args, " "), "up -d") {
			return nil, errors.New("reload failed")
		}
		return nil, nil
	})
	_, err := m.ConfigureMemory(context.Background(), d, MemoryConfig{BaseURL: server.URL, Model: "test-model", SecretPath: secret, Secret: "next"}, filepath.Join(root, "task-token"), server.Client())
	if err == nil {
		t.Fatal("configuration unexpectedly succeeded")
	}
	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Fatalf("rolled-back secret exists: %v", err)
	}
	if _, err := os.Stat(d.StackEnv); !os.IsNotExist(err) {
		t.Fatalf("rolled-back environment exists: %v", err)
	}
}

func TestMemoryTransactionRestoresAfterReloadFailure(t *testing.T) {
	injectModelKeyChown(t, func(string, int, int) error { return nil }, true)
	root := t.TempDir()
	d := Deployment{Root: root, Mode: ModeCompose, StackEnv: filepath.Join(root, "zoe-stack.env"), ComposeProject: "zoe", ComposeFile: filepath.Join(root, "compose.yml")}
	secret := filepath.Join(root, "memory-token")
	if err := os.WriteFile(secret, []byte("old-memory"), 0600); err != nil {
		t.Fatal(err)
	}
	originalEnv := "KEEP=old\nZOE_MEM0_LLM_MODEL=old-model\n"
	if err := os.WriteFile(d.StackEnv, []byte(originalEnv), 0640); err != nil {
		t.Fatal(err)
	}
	server := modelServer(t)
	calls := 0
	m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "docker" && strings.Contains(strings.Join(args, " "), "up -d") {
			calls++
			if calls == 1 {
				return nil, errors.New("reload failed")
			}
		}
		return nil, nil
	})
	_, err := m.ConfigureMemory(context.Background(), d, MemoryConfig{BaseURL: server.URL, Model: "test-model", SecretPath: secret, Secret: "next"}, filepath.Join(root, "task-token"), server.Client())
	if err == nil || !strings.Contains(err.Error(), "reload failed") {
		t.Fatalf("error = %v", err)
	}
	got, _ := os.ReadFile(secret)
	if string(got) != "old-memory" {
		t.Fatal("memory secret rollback failed")
	}
	got, _ = os.ReadFile(d.StackEnv)
	info, _ := os.Stat(d.StackEnv)
	if string(got) != originalEnv || info.Mode().Perm() != 0640 {
		t.Fatal("stack environment rollback failed")
	}
	if calls != 2 {
		t.Fatalf("restoration reload count = %d", calls)
	}
}

func TestMemoryConfigurationRefusesTaskSecretPath(t *testing.T) {
	d := testDeployment(t)
	server := modelServer(t)
	secret := filepath.Join(d.Root, "same-token")
	_, err := NewManager(nil).ConfigureMemory(context.Background(), d, MemoryConfig{BaseURL: server.URL, Model: "test-model", SecretPath: secret, Secret: "next"}, secret, server.Client())
	if err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("same secret path accepted: %v", err)
	}
}
func TestProbeNeverFollowsCredentialRedirect(t *testing.T) {
	followed := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer origin.Close()
	r := Probe(context.Background(), ProviderConfig{BaseURL: origin.URL, Model: "m", Secret: "next"}, origin.Client())
	if followed || r.ErrorCategory != "provider_http_error" {
		t.Fatalf("redirect followed: %+v", r)
	}
}
func TestProviderLockHonorsCancellation(t *testing.T) {
	d := testDeployment(t)
	unlock, err := lockDeployment(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = lockDeployment(ctx, d)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error=%v", err)
	}
}
func TestRunnerStatusIsScopedAndAllowlisted(t *testing.T) {
	m := NewManager(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[len(args)-1] != "zoe-runner.service" {
			t.Fatal("status missing service scope")
		}
		return []byte("active\n"), nil
	})
	s := m.Inspect(context.Background(), InspectConfig{Mode: ModeSystemd, Services: []string{"zoe-runner.service"}})
	if s.Services[0].State != "active" {
		t.Fatal(s.Services)
	}
}
func TestProviderValidationRejectsUnsafeInputs(t *testing.T) {
	for _, raw := range []string{"http://remote.example/v1", "https://user:pass@example/v1", "https://example/v1?secret=x"} {
		if ValidateProviderBaseURL(raw) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if validToken("has space") || validToken("line\nfeed") {
		t.Fatal("accepted whitespace token")
	}
}
func TestLastCheckRejectsUntrustedMetadata(t *testing.T) {
	d := testDeployment(t)
	_ = os.WriteFile(filepath.Join(d.Root, "provider-check.json"), []byte(`{"checked_at":"2026-10-25T00:00:00Z","outcome":"secret-token"}`), 0600)
	if strings.Contains(LastProviderCheck(d), "secret-token") {
		t.Fatal("untrusted metadata echoed")
	}
}
