package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/ronigooja/Zoe/internal/control"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestMaxRunsEnvDefaultValidAndInvalid(t *testing.T) {
	t.Setenv("ZOE_MAX_RUNS", "")
	if got := maxRunsEnv(); got != 12 {
		t.Fatalf("default=%d", got)
	}
	for _, value := range []string{"1", "64", "12"} {
		t.Setenv("ZOE_MAX_RUNS", value)
		if maxRunsEnv() == 0 {
			t.Fatalf("valid %s rejected", value)
		}
	}
	for _, value := range []string{"0", "65", "not-a-number"} {
		t.Setenv("ZOE_MAX_RUNS", value)
		if maxRunsEnv() != 0 {
			t.Fatalf("invalid %s accepted", value)
		}
	}
}

func TestWaitLimitEnvExactDurationAndConfigRange(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 30 * time.Minute}, {"30", 30 * time.Minute}, {"1", time.Minute}, {"1440", 24 * time.Hour},
		{"0", 0}, {"1441", 0}, {"x", 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("ZOE_WAIT_LIMIT_MINUTES", tc.value)
			if got := waitLimitEnv(); got != tc.want {
				t.Fatalf("waitLimitEnv(%q)=%v, want %v", tc.value, got, tc.want)
			}
			if tc.want > 0 {
				cfg := control.Config{WaitLimit: gotWaitLimitForConfig()}
				if cfg.WaitLimit < time.Minute || cfg.WaitLimit > 24*time.Hour {
					t.Fatalf("config WaitLimit=%v out of range", cfg.WaitLimit)
				}
			}
		})
	}
}

func gotWaitLimitForConfig() time.Duration { return waitLimitEnv() }

func TestServerSubprocessBootstrapAndRestart(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "zoe-server")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, output)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	db := filepath.Join(t.TempDir(), "state", "zoe.db")
	token := subprocessSecret(t)
	startupKey := token
	modelKey := filepath.Join(t.TempDir(), "model-key")
	if err := os.WriteFile(modelKey, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	start := func() *exec.Cmd {
		cmd := exec.Command(binary)
		cmd.Env = append(os.Environ(), "ZOE_LISTEN_ADDR="+addr, "ZOE_DATABASE_PATH="+db, "ZOE_INSTANCE_KEY="+startupKey, "ZOE_VOICE_GATEWAY_URL=", "ZOE_VOICE_GATEWAY_API_KEY=")
		for _, prefix := range []string{"ZOE_REGISTER", "ZOE_COMPACT"} {
			cmd.Env = append(cmd.Env, prefix+"_BASE_URL=https://model.invalid", prefix+"_MODEL=fixture", prefix+"_API_KEY_FILE="+modelKey)
		}
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	client := &http.Client{Timeout: time.Second}
	call := func(method, path, bearer string, body any) (*http.Response, []byte, error) {
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req, err := http.NewRequest(method, "http://"+addr+path, bytes.NewReader(data))
		if err != nil {
			return nil, nil, err
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		_, err = buf.ReadFrom(res.Body)
		return res, buf.Bytes(), err
	}
	callCommand := func(bearer, key string, body any) (*http.Response, []byte, error) {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/commands", bytes.NewReader(data))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Idempotency-Key", key)
		res, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		_, err = buf.ReadFrom(res.Body)
		return res, buf.Bytes(), err
	}
	cmd := start()
	t.Cleanup(func() { stopServer(t, cmd) })
	deadline := time.Now().Add(8 * time.Second)
	for {
		res, _, err := call(http.MethodGet, "/healthz", "", nil)
		if err == nil && res.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become ready: %v\nstderr:\n%s", err, stderr.String())
		}
		time.Sleep(30 * time.Millisecond)
	}
	lockedRotation := exec.Command(binary, "rotate-key", "--database", db, "--stdout")
	if output, e := lockedRotation.Output(); e == nil || len(output) != 0 {
		t.Fatal("rotation ran or disclosed secret while server owned database")
	}
	res, body, e := callCommand(token, "preserved-project", map[string]any{"kind": "project.create", "payload": map[string]string{"name": "preserved owner data"}})
	if e != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("pre-recovery project=%v %v %s", responseStatus(res), e, body)
	}
	stopServer(t, cmd)
	rotation := exec.Command(binary, "rotate-key", "--database", db, "--stdout")
	output, err := rotation.Output()
	if err != nil {
		t.Fatalf("offline rotation: %v", err)
	}
	var exported struct {
		Key string `json:"key"`
	}
	if err = json.Unmarshal(output, &exported); err != nil || exported.Key == "" {
		t.Fatal("missing explicit key export")
	}
	oldToken := token
	token = exported.Key
	newToken := token
	// Canonical deployments restart with database authority alone: no plaintext
	// startup secret needs to be synchronized after rotation.
	startupKey = ""
	cmd = start()
	t.Cleanup(func() { stopServer(t, cmd) })
	deadline = time.Now().Add(8 * time.Second)
	for {
		res, _, err = call(http.MethodGet, "/healthz", "", nil)
		if err == nil && res.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restarted server did not become ready: %v\nstderr:\n%s", err, stderr.String())
		}
		time.Sleep(30 * time.Millisecond)
	}
	res, body, err = call(http.MethodGet, "/v1/snapshot", oldToken, nil)
	if err != nil || res.StatusCode < http.StatusBadRequest {
		t.Fatalf("recovery left old bearer valid: status=%v err=%v", responseStatus(res), err)
	}
	res, body, err = call(http.MethodGet, "/v1/snapshot", newToken, nil)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("snapshot after restart status=%v err=%v body=%s", responseStatus(res), err, body)
	}
	if !bytes.Contains(body, []byte("preserved owner data")) {
		t.Fatal("recovery changed owner subject or lost project")
	}
	res, body, err = callCommand(newToken, "memory-smoke", map[string]any{"kind": "memory.remember", "payload": map[string]any{"scope": map[string]string{"kind": "personal"}, "subject": "server restart", "key": "memory readiness", "content": "memory command is available after restart", "source_kind": "user_note"}})
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("memory write after restart status=%v err=%v body=%s", responseStatus(res), err, body)
	}
	res, body, err = call(http.MethodGet, "/v1/memory?q=available+after+restart", newToken, nil)
	if err != nil || res.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("memory command is available after restart")) {
		t.Fatalf("memory search after restart status=%v err=%v body=%s", responseStatus(res), err, body)
	}
	res, body, err = call(http.MethodGet, "/v1/voice/config", newToken, nil)
	if err != nil || res.StatusCode != http.StatusServiceUnavailable || !bytes.Contains(body, []byte("voice_unavailable")) {
		t.Fatalf("voice candidate without gateway should fail closed after restart: status=%v err=%v body=%s", responseStatus(res), err, body)
	}
}

func subprocessSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func responseStatus(r *http.Response) any {
	if r == nil {
		return "no response"
	}
	return fmt.Sprint(r.StatusCode)
}

func stopServer(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Logf("server exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Error("server did not stop within grace period")
	}
}
