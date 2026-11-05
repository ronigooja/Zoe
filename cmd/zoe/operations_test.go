package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/ops"
	"github.com/ronigooja/Zoe/internal/storage"
)

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
func fakeServices(t *testing.T, failReload bool) *[]string {
	t.Helper()
	old := opsManager
	calls := []string{}
	opsManager = func() ops.Manager {
		return ops.NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
			command := name + " " + strings.Join(args, " ")
			calls = append(calls, command)
			if strings.Contains(command, "systemctl show") {
				return []byte("LoadState=loaded\n"), nil
			}
			if strings.Contains(command, "is-active") {
				return []byte("active\n"), nil
			}
			if failReload && strings.Contains(command, "reload-or-restart") {
				return nil, errors.New("simulated service error")
			}
			return nil, nil
		})
	}
	t.Cleanup(func() { opsManager = old })
	return &calls
}

func TestTriageTestTextFlagClassifiesGivenSentence(t *testing.T) {
	fakeServices(t, false)
	var input string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		input, _ = body["input"].(string)
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"task"}]}]}`))
	}))
	defer s.Close()
	root := t.TempDir()
	key := filepath.Join(root, "key")
	if err := os.WriteFile(key, []byte("key"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_TRIAGE_BASE_URL=\""+s.URL+"/v1\"\nZOE_TRIAGE_MODEL=\"m\"\nZOE_TRIAGE_API_KEY_FILE=\""+key+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := triageTestOperation([]string{"--root", root, "--text", "查查今天新闻"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if input != "查查今天新闻" {
		t.Fatalf("input=%q", input)
	}
}

func TestTriageTestDefaultSentenceUnchanged(t *testing.T) {
	fakeServices(t, false)
	var input string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		input, _ = body["input"].(string)
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"progress"}]}]}`))
	}))
	defer s.Close()
	root := t.TempDir()
	key := filepath.Join(root, "key")
	if err := os.WriteFile(key, []byte("key"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_TRIAGE_BASE_URL=\""+s.URL+"/v1\"\nZOE_TRIAGE_MODEL=\"m\"\nZOE_TRIAGE_API_KEY_FILE=\""+key+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := triageTestOperation([]string{"--root", root}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if input != "好了吗" {
		t.Fatalf("input=%q", input)
	}
}
func TestProviderConfigureTestStatusUsePersistedDeployment(t *testing.T) {
	fakeServices(t, false)
	token := "private-provider-token"
	requests := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("provider request did not use supplied configuration")
		}
		io.WriteString(w, `{"data":[{"id":"chosen-model"}]}`)
	}))
	defer provider.Close()
	root := t.TempDir()
	secret := filepath.Join(root, "private-token")
	var out, errOut bytes.Buffer
	args := []string{"--root", root, "--url", provider.URL + "/v1", "--model", "chosen-model", "--secret-file", secret, "--token", token, "--non-interactive"}
	if err := providerConfigureOperation(args, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_MEM0_LLM_MODEL=memory-model\nZOE_MEM0_LLM_REASONING_EFFORT=high\nZOE_TRIAGE_MODEL=triage-model\nZOE_TRIAGE_REASONING_EFFORT=low\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := providerTestOperation([]string{"--root", root}, nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if err := statusOperation([]string{"--root", root}, nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("provider requests = %d", requests)
	}
	if !strings.Contains(out.String(), "chosen-model") || !strings.Contains(out.String(), "last-check=") {
		t.Fatal("status omitted persisted provider configuration")
	}
	if strings.Contains(out.String()+errOut.String(), token) {
		t.Fatal("token leaked to command output")
	}
}

func TestStatusShowsModelAndEffortWithoutSecrets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "zoe-runner.env"), []byte("ZOE_OPENCODE_MODEL=task-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_MEM0_LLM_MODEL=memory-model\nZOE_MEM0_LLM_REASONING_EFFORT=high\nZOE_TRIAGE_BASE_URL=https://gateway.example\nZOE_TRIAGE_MODEL=triage-model\nZOE_TRIAGE_REASONING_EFFORT=low\nZOE_TRIAGE_API_STYLE=chat\nZOE_TRIAGE_THINKING=disabled\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeServices(t, false)
	var out, errOut bytes.Buffer
	if err := statusOperation([]string{"--root", root}, nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"model=task-model effort=unsupported", "model=memory-model effort=high", "model=triage-model effort=low", "style=chat", "thinking=disabled"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status=%s", got)
		}
	}
	if strings.Contains(got, "secret") && strings.Contains(got, "token") {
		t.Fatalf("possible secret leak: %s", got)
	}
}

func TestStatusShowsStyleAndThinkingWithoutSecrets(t *testing.T) {
	t.Run("status", func(t *testing.T) { TestStatusShowsModelAndEffortWithoutSecrets(t) })
}

func TestTriageTestReportsWhichStyleWorked(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "key")
	if err := os.WriteFile(key, []byte("secret\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"task"},"finish_reason":"stop"}]}`))
	}))
	defer s.Close()
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_TRIAGE_BASE_URL="+s.URL+"\nZOE_TRIAGE_MODEL=m\nZOE_TRIAGE_API_KEY_FILE="+key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := triageTestOperation([]string{"--root", root}, nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "style=chat") {
		t.Fatalf("out=%s err=%s", out.String(), errOut.String())
	}
}

func TestStatusUsesConfiguredServerPortForReadiness(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("readiness path=%q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ready.Close()
	port := strings.TrimPrefix(ready.URL, "http://127.0.0.1:")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_SERVER_PORT="+port+"\nZOE_COMPOSE_FILE=/tmp/zoe-compose.yaml\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeServices(t, false)
	oldMode := os.Getenv("ZOE_DEPLOYMENT_MODE")
	if err := os.Setenv("ZOE_DEPLOYMENT_MODE", "compose"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("ZOE_DEPLOYMENT_MODE", oldMode) })
	var out, errOut bytes.Buffer
	if err := statusOperation([]string{"--root", root}, nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Instance key: configured") {
		t.Fatalf("status did not use configured port: %s", out.String())
	}
}

func TestMemoryConfigureConsumesCRLFAnswersFromSingleChunk(t *testing.T) {
	root := t.TempDir()
	compose := filepath.Join(root, "compose.yml")
	if err := os.WriteFile(compose, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_COMPOSE_FILE="+compose+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldMode := os.Getenv("ZOE_DEPLOYMENT_MODE")
	_ = os.Setenv("ZOE_DEPLOYMENT_MODE", "compose")
	t.Cleanup(func() { _ = os.Setenv("ZOE_DEPLOYMENT_MODE", oldMode) })
	old := opsManager
	opsManager = func() ops.Manager {
		return ops.NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "systemctl" && strings.Contains(strings.Join(args, " "), "show") {
				return []byte("LoadState=not-found\n"), errors.New("not found")
			}
			return nil, nil
		})
	}
	t.Cleanup(func() { opsManager = old })
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer memory-token" {
			t.Error("memory probe did not receive the prompted credential")
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-6.1-sol"}]}`)
	}))
	defer gateway.Close()
	var out, errOut bytes.Buffer
	answers := gateway.URL + "\r\ngpt-6.1-sol\r\nmemory-token\r\n"
	if err := memoryConfigureOperation([]string{"--root", root, "--secret", filepath.Join(root, "secrets", "mem0-llm-api-key")}, strings.NewReader(answers), &out, &errOut); err != nil {
		t.Fatalf("configure failed: %v; stderr=%s", err, errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), "invalid_configuration") {
		t.Fatalf("CRLF prompt input was misparsed: out=%s err=%s", out.String(), errOut.String())
	}
}

func TestVoiceConfigureAndStatus(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_COMPOSE_FILE=/tmp/zoe-compose.yaml\nCOMPOSE_PROJECT_NAME=zoe\n"), 0600)
	oldMode := os.Getenv("ZOE_DEPLOYMENT_MODE")
	_ = os.Setenv("ZOE_DEPLOYMENT_MODE", "compose")
	t.Cleanup(func() { _ = os.Setenv("ZOE_DEPLOYMENT_MODE", oldMode) })
	fakeServices(t, false)
	var out bytes.Buffer
	if err := voiceConfigureOperation([]string{"--root", root, "--url", "https://voice.example.test", "--token", "voice-secret", "--non-interactive"}, strings.NewReader(""), &out, &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "zoe-stack.env"))
	if err != nil || !strings.Contains(string(data), "ZOE_VOICE_GATEWAY_URL=\"https://voice.example.test\"") {
		t.Fatalf("unexpected voice config: %s (%v)", data, err)
	}
	out.Reset()
	if err := voiceStatusOperation([]string{"--root", root}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Voice gateway URL: https://voice.example.test") || strings.Contains(out.String(), "voice-secret") {
		t.Fatalf("status omitted voice settings: %s", out.String())
	}
}

func TestVoiceConfigureSystemdModeUsesComposeAndContainerOwnedSecret(t *testing.T) {
	root := t.TempDir()
	compose := filepath.Join(root, "compose.yml")
	stack := filepath.Join(root, "zoe-stack.env")
	if err := os.WriteFile(compose, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stack, []byte("ZOE_COMPOSE_FILE="+compose+"\nCOMPOSE_PROJECT_NAME=zoe\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "provider-deployment.json"), []byte(`{"Mode":"systemd","Root":"`+root+`","StackEnv":"`+stack+`","ComposeFile":"`+compose+`","ComposeProject":"zoe"}`), 0600); err != nil {
		t.Fatal(err)
	}
	oldMode := os.Getenv("ZOE_DEPLOYMENT_MODE")
	_ = os.Unsetenv("ZOE_DEPLOYMENT_MODE")
	t.Cleanup(func() { _ = os.Setenv("ZOE_DEPLOYMENT_MODE", oldMode) })
	calls := fakeServices(t, false)
	secret := filepath.Join(root, "secrets", "voice-gateway-api-key")
	var out bytes.Buffer
	if err := voiceConfigureOperation([]string{"--root", root, "--url", "https://voice.example.test", "--token", "voice-secret", "--secret-file", secret, "--non-interactive"}, strings.NewReader(""), &out, &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stack)
	if err != nil || !strings.Contains(string(data), "ZOE_VOICE_GATEWAY_URL") {
		t.Fatalf("voice env was not written: %v %s", err, data)
	}
	secretData, err := os.ReadFile(secret)
	if err != nil || string(secretData) != "voice-secret\n" {
		t.Fatalf("voice secret was not written: %v %q", err, secretData)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "docker compose --project-name zoe --file "+compose+" --env-file "+stack+" up -d --no-build --no-deps --force-recreate zoe-server") {
		t.Fatalf("systemd-mode configure did not reload zoe-server via Compose: %v", *calls)
	}
}

func TestVoiceConfigureWithoutComposeFileWritesNothing(t *testing.T) {
	root := t.TempDir()
	stack := filepath.Join(root, "zoe-stack.env")
	if err := os.WriteFile(stack, []byte("COMPOSE_PROJECT_NAME=zoe\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldMode := os.Getenv("ZOE_DEPLOYMENT_MODE")
	_ = os.Setenv("ZOE_DEPLOYMENT_MODE", "systemd")
	t.Cleanup(func() { _ = os.Setenv("ZOE_DEPLOYMENT_MODE", oldMode) })
	fakeServices(t, false)
	secret := filepath.Join(root, "secrets", "voice-gateway-api-key")
	err := voiceConfigureOperation([]string{"--root", root, "--url", "https://voice.example.test", "--token", "voice-secret", "--secret-file", secret, "--non-interactive"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "known Compose file") {
		t.Fatalf("error=%v, want clear missing Compose file error", err)
	}
	if _, statErr := os.Stat(secret); !os.IsNotExist(statErr) {
		t.Fatalf("secret was written despite missing Compose file: %v", statErr)
	}
	data, readErr := os.ReadFile(stack)
	if readErr != nil || strings.Contains(string(data), "ZOE_VOICE_") {
		t.Fatalf("stack env changed despite missing Compose file: %v %s", readErr, data)
	}
}
func TestKeyMenuInitializesRotatesAndRestoresService(t *testing.T) {
	calls := fakeServices(t, false)
	root := t.TempDir()
	database := filepath.Join(root, "control.sqlite")
	var terminal, out, errOut bytes.Buffer
	old := secretTerminal
	secretTerminal = func() (io.WriteCloser, error) { return nopWriteCloser{&terminal}, nil }
	t.Cleanup(func() { secretTerminal = old })
	firstPath := filepath.Join(root, "initial.json")
	nextPath := filepath.Join(root, "rotated.json")
	args := []string{"--root", root, "--database", database, "--server", "https://zoe.example", "--output", firstPath}
	if err := keyOperation(false, args, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var first, next ops.KeyResult
	read := func(path string, dst *ops.KeyResult) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, dst); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("key export is not private")
		}
	}
	read(firstPath, &first)
	terminal.Reset()
	args[len(args)-1] = nextPath
	args = append(args, "--yes")
	if err := keyOperation(true, args, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	read(nextPath, &next)
	if first.Key == next.Key || !strings.Contains(terminal.String(), next.InstallationURL) || !strings.Contains(terminal.String(), "█") {
		t.Fatal("rotation or terminal connection details missing")
	}
	if strings.Contains(out.String()+errOut.String(), next.Key) {
		t.Fatal("key leaked outside controlling terminal")
	}
	store, err := storage.Open(context.Background(), storage.Config{Path: database})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := control.InitializeInstanceAuth(context.Background(), store, first.Key); err == nil {
		t.Fatal("old credential remains valid")
	}
	if _, _, err := control.InitializeInstanceAuth(context.Background(), store, next.Key); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*calls, "\n")
	if strings.Count(joined, "systemctl stop zoe-compose.service") != 2 || strings.Count(joined, "systemctl start zoe-compose.service") != 2 {
		t.Fatalf("service lifecycle incomplete: %s", joined)
	}
}
func TestKeyFailureRestoresServiceAndDoesNotDisplaySecret(t *testing.T) {
	calls := fakeServices(t, false)
	root := t.TempDir()
	db := filepath.Join(root, "control.sqlite")
	store, err := storage.Open(context.Background(), storage.Config{Path: db})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var out, errOut bytes.Buffer
	err = keyOperation(false, []string{"--root", root, "--database", db, "--server", "https://host"}, strings.NewReader(""), &out, &errOut)
	if err == nil {
		t.Fatal("key operation ignored active database lock")
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "systemctl start zoe-compose.service") {
		t.Fatal("service not restored after key failure")
	}
	matches, _ := filepath.Glob(filepath.Join(root, "instance-key-*.json"))
	if len(matches) != 0 {
		t.Fatal("failed action left a private export")
	}
}
func TestKeyRefusesStaleDeploymentCredentialBeforeStoppingService(t *testing.T) {
	calls := fakeServices(t, false)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_INSTANCE_KEY_FILE=/private/existing-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := keyOperation(true, []string{"--root", root, "--database", filepath.Join(root, "control.sqlite"), "--server", "https://host", "--yes"}, strings.NewReader(""), &out, &out)
	if err == nil {
		t.Fatal("rotation accepted stale startup source")
	}
	for _, command := range *calls {
		if strings.Contains(command, " stop ") {
			t.Fatal("stale source check stopped services")
		}
	}
}
func TestKeyWithoutTerminalSavesExportWithoutPrintingCredentials(t *testing.T) {
	root := t.TempDir()
	old := secretTerminal
	secretTerminal = func() (io.WriteCloser, error) { return nil, errors.New("no tty") }
	t.Cleanup(func() { secretTerminal = old })
	var out bytes.Buffer
	if err := keyOperation(false, []string{"--offline", "--root", root, "--database", filepath.Join(root, "db"), "--server", "https://host"}, strings.NewReader(""), &out, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "zoe://") || !strings.Contains(out.String(), "No controlling terminal") {
		t.Fatal("non-terminal output exposed connection details")
	}
}

func TestKeyRotationRejectsResolvedComposeStartupCredential(t *testing.T) {
	for _, fixture := range []struct {
		name, config string
	}{
		{"fixed_secret_path", `{"services":{"zoe-server":{"environment":{"ZOE_SECRET_FILE_MAP":"ZOE_INSTANCE_KEY=/run/secrets/instance_key,ZOE_RUNNER_TOKEN=/run/secrets/runner"}}}}`},
		{"inline_key", `{"services":{"zoe-server":{"environment":{"ZOE_INSTANCE_KEY":"private-fixture-value"}}}}`},
		{"missing_server", `{"services":{}}`},
		{"invalid_config", `private-fixture-value`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_COMPOSE_FILE=/deployment/control.yaml\n"), 0600); err != nil {
				t.Fatal(err)
			}
			old := opsManager
			defer func() { opsManager = old }()
			var calls []string
			opsManager = func() ops.Manager {
				return ops.NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
					call := name + " " + strings.Join(args, " ")
					calls = append(calls, call)
					if name == "docker" {
						return []byte(fixture.config), nil
					}
					return []byte("LoadState=loaded\n"), nil
				})
			}
			db := filepath.Join(root, "untouched.sqlite")
			var out, errOut bytes.Buffer
			err := keyOperation(true, []string{"--root", root, "--database", db, "--server", "https://zoe.example", "--yes"}, strings.NewReader(""), &out, &errOut)
			if err == nil {
				t.Fatal("unsafe startup configuration accepted")
			}
			if strings.Contains(err.Error()+out.String()+errOut.String(), "private-fixture-value") {
				t.Fatal("resolved configuration leaked into diagnostics")
			}
			if _, err := os.Stat(db); !os.IsNotExist(err) {
				t.Fatal("key maintenance touched database before checking startup sources")
			}
			if strings.Contains(strings.Join(calls, "\n"), "systemctl stop") {
				t.Fatal("key maintenance stopped service before checking startup sources")
			}
		})
	}
}

func TestComposeKeyCheckAllowsDatabaseOnlyStartup(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "zoe-stack.env"), []byte("ZOE_COMPOSE_FILE=/deployment/control.yaml\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := opsManager
	defer func() { opsManager = old }()
	opsManager = func() ops.Manager {
		return ops.NewManager(func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "docker" {
				return []byte(`{"services":{"zoe-server":{"environment":{"ZOE_INSTANCE_KEY":null,"ZOE_SECRET_FILE_MAP":"ZOE_RUNNER_TOKEN=/run/secrets/runner_token"}}}}`), nil
			}
			return []byte("LoadState=loaded\n"), nil
		})
	}
	if err := checkComposeKeySources(context.Background(), root); err != nil {
		t.Fatal(err)
	}
}
