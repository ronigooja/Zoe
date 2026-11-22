package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const oldProviderRoutes = `{"worker":{"profile_id":"worker","runtime_route_id":"opencode","model_route_id":"default","isolation_policy":"opencode-workspace-write-v1","harness":"opencode","model":"cliproxyapi/gpt-6.1-sol","effort":"high","approval_mode":"accept-edits","future":{"limit":9007199254740993}},"custom-route":{"profile_id":"custom-route","runtime_route_id":"custom-runtime","model_route_id":"custom-model","isolation_policy":"custom-policy","harness":"opencode","model":"other/old","approval_mode":"custom","unknown":[true,"kept"]}}`

func TestConfigureProviderModelChangeRewritesAllRoutesPreservingFields(t *testing.T) {
	for _, quote := range []string{"raw", "double", "single"} {
		t.Run(quote, func(t *testing.T) {
			d := testDeployment(t)
			server := modelServer(t)
			routes := oldProviderRoutes
			switch quote {
			case "double":
				routes = strconv.Quote(routes)
			case "single":
				routes = "'" + routes + "'"
			}
			unknown := "# custom settings\nUNKNOWN = 'leave exactly as is'\nnot-an-assignment\n"
			if err := os.WriteFile(d.RunnerEnv, []byte(unknown+"ZOE_OPENCODE_MODEL=cliproxyapi/gpt-6.1-sol\nZOE_RUNNER_ROUTES="+routes+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var commands []string
			m := NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
				commands = append(commands, name+" "+strings.Join(args, " "))
				return nil, nil
			})
			model := "cliproxyapi/test-model"
			setProviderTestLimits(t)
			_, err := m.ConfigureProvider(context.Background(), d, ProviderConfig{BaseURL: server.URL, Model: model, SecretPath: filepath.Join(d.Root, "token"), Secret: "next"}, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			values, err := readEnv(d.RunnerEnv)
			if err != nil {
				t.Fatal(err)
			}
			var before, after map[string]map[string]json.RawMessage
			if err := json.Unmarshal([]byte(oldProviderRoutes), &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(values["ZOE_RUNNER_ROUTES"]), &after); err != nil {
				t.Fatal(err)
			}
			for _, route := range before {
				route["model"] = json.RawMessage(strconv.Quote(model))
			}
			if !reflect.DeepEqual(after, before) || values["ZOE_OPENCODE_MODEL"] != model {
				t.Fatalf("routes or provider model not updated: %v", values)
			}
			contents, err := os.ReadFile(d.RunnerEnv)
			if err != nil || !strings.HasPrefix(string(contents), unknown) {
				t.Fatalf("unknown lines changed: %q (%v)", contents, err)
			}
			if !reflect.DeepEqual(commands, []string{"systemctl daemon-reload", "systemctl reload-or-restart zoe-runner.service"}) {
				t.Fatalf("runner refresh commands = %v", commands)
			}
		})
	}
}

func TestConfigureProviderReloadFailureRestoresOldRoutes(t *testing.T) {
	d := testDeployment(t)
	server := modelServer(t)
	old := "KEEP=old\nZOE_OPENCODE_MODEL=cliproxyapi/gpt-6.1-sol\nZOE_RUNNER_ROUTES=" + strconv.Quote(oldProviderRoutes) + "\n"
	if err := os.WriteFile(d.RunnerEnv, []byte(old), 0640); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m := NewManager(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "reload-or-restart" {
			calls++
			if calls == 1 {
				if got := readEnvValue(d.RunnerEnv, "ZOE_RUNNER_ROUTES"); strings.Contains(got, "gpt-6.1-sol") || !strings.Contains(got, "cliproxyapi/test-model") {
					t.Error("routes were not updated before reload")
				}
				return nil, errors.New("route reload failure")
			}
			data, err := os.ReadFile(d.RunnerEnv)
			if err != nil || string(data) != old {
				t.Error("old routes were not restored before rollback reload")
			}
		}
		return nil, nil
	})
	setProviderTestLimits(t)
	_, err := m.ConfigureProvider(context.Background(), d, ProviderConfig{BaseURL: server.URL, Model: "cliproxyapi/test-model", SecretPath: filepath.Join(d.Root, "token"), Secret: "next"}, server.Client())
	if err == nil || !strings.Contains(err.Error(), "route reload failure") {
		t.Fatalf("configure error = %v", err)
	}
	data, err := os.ReadFile(d.RunnerEnv)
	if err != nil || string(data) != old || calls != 2 {
		t.Fatalf("old environment not restored, reloads=%d: %v", calls, err)
	}
	info, err := os.Stat(d.RunnerEnv)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("old environment mode not restored: %v", err)
	}
}

func TestConfigureProviderInvalidOrAbsentRoutesUntouched(t *testing.T) {
	for _, routes := range []string{"", "ZOE_RUNNER_ROUTES=not-json\n", " ZOE_RUNNER_ROUTES=\"bad\\q\"\n", "ZOE_RUNNER_ROUTES=[]\n", "ZOE_RUNNER_ROUTES=null\n", "ZOE_RUNNER_ROUTES={}\n", "ZOE_RUNNER_ROUTES={\"worker\":null}\n", "ZOE_RUNNER_ROUTES={\"worker\":{}}\n", "ZOE_RUNNER_ROUTES={\"worker\":{},\"broken\":42}\n"} {
		t.Run(routes, func(t *testing.T) {
			d := testDeployment(t)
			server := modelServer(t)
			old := "KEEP=original\n" + routes
			if err := os.WriteFile(d.RunnerEnv, []byte(old), 0600); err != nil {
				t.Fatal(err)
			}
			m := NewManager(func(context.Context, string, ...string) ([]byte, error) { return nil, nil })
			setProviderTestLimits(t)
			_, err := m.ConfigureProvider(context.Background(), d, ProviderConfig{BaseURL: server.URL, Model: "cliproxyapi/test-model", SecretPath: filepath.Join(d.Root, "token"), Secret: "next"}, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(d.RunnerEnv)
			if err != nil || !strings.HasPrefix(string(data), old) {
				t.Fatalf("invalid routes changed: %q (%v)", data, err)
			}
			if routes == "" && strings.Contains(string(data), "ZOE_RUNNER_ROUTES=") {
				t.Fatal("absent routes were added")
			}
		})
	}
}
