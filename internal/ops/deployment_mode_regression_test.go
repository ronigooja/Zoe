package ops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func modeFixture(t *testing.T, persistedMode Mode, composeExists bool) (string, Manager, *[]string) {
	t.Helper()
	t.Setenv("ZOE_DEPLOYMENT_MODE", "")
	t.Setenv("ZOE_COMPOSE_FILE", "")
	root := t.TempDir()
	d := Deployment{Root: root, Mode: persistedMode, RunnerEnv: filepath.Join(root, "zoe-runner.env"), StackEnv: filepath.Join(root, "zoe-stack.env"), RunnerService: "zoe-runner.service", ComposeProject: "zoe", ComposeFile: filepath.Join(root, "compose.yml"), DropInPath: filepath.Join(root, "systemd", "zoe-runner.service.d", "90-zoe-provider.conf")}
	if composeExists {
		if err := os.WriteFile(d.ComposeFile, []byte("services: {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "provider-deployment.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "systemctl" && len(args) > 0 && args[0] == "show" {
			return []byte("LoadState=loaded\n"), nil
		}
		return []byte("active\n"), nil
	})
	return root, m, &calls
}

func TestDeploymentModePersistedSystemdMemory(t *testing.T) {
	root, m, calls := modeFixture(t, ModeSystemd, true)
	oldChown, oldUID := chownFile, effectiveUID
	chownFile = func(string, int, int) error { return nil }
	effectiveUID = func() int { return 0 }
	t.Cleanup(func() { chownFile, effectiveUID = oldChown, oldUID })
	d, err := DiscoverDeployment(context.Background(), m, root)
	if err != nil {
		t.Fatal(err)
	}
	s := modelServer(t)
	r, err := m.ConfigureMemory(context.Background(), d, MemoryConfig{BaseURL: s.URL, Model: "test-model", Secret: "next", SecretPath: filepath.Join(root, "memory-key")}, filepath.Join(root, "provider-key"), s.Client())
	if err != nil || !r.OK() {
		t.Fatalf("ConfigureMemory on systemd-supervised Compose: %v (%+v)", err, r)
	}
	if d.Mode != ModeCompose || !strings.Contains(strings.Join(*calls, "\n"), "zoe-memory-worker") {
		t.Fatalf("mode=%s commands=%v", d.Mode, *calls)
	}
}

func TestDeploymentModePureSystemd(t *testing.T) {
	for _, persisted := range []Mode{ModeSystemd, ModeCompose} {
		t.Run(string(persisted), func(t *testing.T) {
			root, m, _ := modeFixture(t, persisted, false)
			d, err := DiscoverDeployment(context.Background(), m, root)
			if err != nil || d.Mode != ModeSystemd {
				t.Fatalf("pure systemd with stale %s descriptor: mode=%s err=%v", persisted, d.Mode, err)
			}
		})
	}
}

func TestDeploymentModeExplicitSystemd(t *testing.T) {
	for _, persisted := range []Mode{ModeSystemd, ModeCompose} {
		t.Run(string(persisted), func(t *testing.T) {
			root, m, _ := modeFixture(t, persisted, true)
			t.Setenv("ZOE_DEPLOYMENT_MODE", "systemd")
			d, err := DiscoverDeployment(context.Background(), m, root)
			if err != nil || d.Mode != ModeSystemd {
				t.Fatalf("explicit systemd with %s descriptor: mode=%s err=%v", persisted, d.Mode, err)
			}
		})
	}
}

func TestDeploymentModeSystemdUnitStatus(t *testing.T) {
	root, m, calls := modeFixture(t, ModeSystemd, true)
	d, err := DiscoverDeployment(context.Background(), m, root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := d.InspectConfig()
	// Exercise unit routing even when a caller explicitly selects Compose.
	cfg.Mode = ModeCompose
	cfg.Services = []string{"zoe-runner.service", "zoe-opencode.service", "zoe-compose.service"}
	status := m.Inspect(context.Background(), cfg)
	for _, s := range status.Services {
		if s.Source != "systemd" || s.State != "active" {
			t.Errorf("unit %s incorrectly routed: %+v; calls=%v", s.Name, s, *calls)
		}
	}
}

func TestDeploymentModeComposeSourcePrecedence(t *testing.T) {
	for _, source := range []string{"persisted", "stack", "environment"} {
		t.Run(source, func(t *testing.T) {
			root, m, _ := modeFixture(t, ModeSystemd, true)
			selected := filepath.Join(root, "compose.yml")
			if source != "persisted" {
				selected = filepath.Join(root, "current.yml")
				if err := os.WriteFile(selected, []byte("services: {}\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_COMPOSE_FILE="+selected+"\nCOMPOSE_PROJECT_NAME=current\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if source == "environment" {
					selected = filepath.Join(root, "override.yml")
					if err := os.WriteFile(selected, []byte("services: {}\n"), 0600); err != nil {
						t.Fatal(err)
					}
					t.Setenv("ZOE_COMPOSE_FILE", selected)
				}
			}
			d, err := DiscoverDeployment(context.Background(), m, root)
			if err != nil || d.Mode != ModeCompose || d.ComposeFile != selected {
				t.Fatalf("%s selection: mode=%s file=%s err=%v", source, d.Mode, d.ComposeFile, err)
			}
		})
	}
}

func TestDeploymentModeProviderPersistsCorrectedDescriptor(t *testing.T) {
	root, m, calls := modeFixture(t, ModeSystemd, true)
	d, err := DiscoverDeployment(context.Background(), m, root)
	if err != nil {
		t.Fatal(err)
	}
	s := modelServer(t)
	key := filepath.Join(root, "provider-key")
	r, err := m.ConfigureProvider(context.Background(), d, ProviderConfig{BaseURL: s.URL, Model: "test-model", Secret: "next", SecretPath: key}, s.Client())
	if err != nil || !r.OK() {
		t.Fatalf("ConfigureProvider on hybrid deployment: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "provider-deployment.json"))
	var saved Deployment
	if err != nil || json.Unmarshal(b, &saved) != nil || saved.Mode != ModeCompose {
		t.Fatalf("descriptor not healed: %s (%v)", b, err)
	}
	secret, err := os.ReadFile(key)
	if err != nil || string(secret) != "next" {
		t.Fatalf("provider secret not saved: %v", err)
	}
	dropIn, err := os.ReadFile(d.DropInPath)
	if err != nil || !strings.Contains(string(dropIn), d.RunnerEnv) {
		t.Fatalf("systemd drop-in not saved: %v", err)
	}
	if got := (*calls)[len(*calls)-1]; got != "systemctl reload-or-restart zoe-runner.service" {
		t.Fatalf("provider restart=%s", got)
	}
}

func TestDeploymentModeDetectCompose(t *testing.T) {
	root, _, _ := modeFixture(t, ModeSystemd, true)
	t.Setenv("ZOE_COMPOSE_FILE", filepath.Join(root, "compose.yml"))
	if got := DetectMode(); got != ModeCompose {
		t.Fatalf("DetectMode=%s, want compose", got)
	}
	t.Setenv("ZOE_DEPLOYMENT_MODE", "systemd")
	if got := DetectMode(); got != ModeSystemd {
		t.Fatalf("DetectMode override=%s, want systemd", got)
	}
}
