package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/ops"
)

func TestDeploymentModeHybridModelConfiguration(t *testing.T) {
	containerKeyFixture(t)
	fakeServices(t, false)
	t.Setenv("ZOE_DEPLOYMENT_MODE", "")
	t.Setenv("ZOE_COMPOSE_FILE", "")
	root := t.TempDir()
	compose := filepath.Join(root, "compose.yml")
	for path, value := range map[string]string{
		compose:                              "services: {}\n",
		filepath.Join(root, "zoe-stack.env"): "ZOE_COMPOSE_FILE=" + compose + "\n",
		filepath.Join(root, "provider-deployment.json"): `{"Root":"` + root + `","Mode":"systemd","ComposeFile":"` + compose + `","StackEnv":"` + filepath.Join(root, "zoe-stack.env") + `"}`,
	} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"Register", "Compact"} {
		t.Run(kind, func(t *testing.T) {
			err := menuModel(ConfigureMenuIO{Root: root, In: strings.NewReader("https://gateway.example/v1\n" + kind + "-model\nnone\ndisabled\n\nn\n"), Out: io.Discard, ErrOut: io.Discard, PromptSecret: func(io.Writer) ([]byte, error) { return []byte("private-key"), nil }}, kind)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	d, err := ops.DiscoverDeployment(context.Background(), opsManager(), root)
	if err != nil || d.Mode != ops.ModeCompose {
		t.Fatalf("model configuration discovery: mode=%s err=%v", d.Mode, err)
	}
	b, err := os.ReadFile(d.StackEnv)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"REGISTER", "COMPACT"} {
		if !strings.Contains(string(b), "ZOE_"+name+"_MODEL=") {
			t.Errorf("missing %s configuration", name)
		}
	}
}

func TestDeploymentModeHybridPauseUsesSystemd(t *testing.T) {
	calls := fakeServices(t, false)
	t.Setenv("ZOE_DEPLOYMENT_MODE", "")
	t.Setenv("ZOE_COMPOSE_FILE", "")
	root := t.TempDir()
	compose := filepath.Join(root, "compose.yml")
	if err := os.WriteFile(compose, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(ops.Deployment{Root: root, Mode: ops.ModeCompose, ComposeFile: compose})
	if err := os.WriteFile(filepath.Join(root, "provider-deployment.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	recover, err := pauseControl(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := recover(); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(*calls, "\n")
	for _, want := range []string{"systemctl is-active zoe-compose.service", "systemctl stop zoe-compose.service", "systemctl start zoe-compose.service"} {
		if !strings.Contains(commands, want) {
			t.Errorf("missing %s; commands=%s", want, commands)
		}
	}
	if strings.Contains(commands, "docker") {
		t.Errorf("hybrid control used Docker instead of supervisor: %s", commands)
	}
}
