package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/ops"
)

func workerMenuFixture(t *testing.T, status int) (string, string) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-key" || r.URL.Path != "/models" {
			t.Error("wrong Worker key or endpoint")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"data":[{"id":"worker-model"},{"id":"worker-changed"}]}`)
	}))
	t.Cleanup(s.Close)
	root := t.TempDir()
	key := filepath.Join(root, "worker-key")
	if err := os.WriteFile(key, []byte("fixture-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ops.WriteRunnerEnvAtomic(filepath.Join(root, "zoe-runner.env"), ops.ProviderConfig{BaseURL: s.URL, Model: "worker-model", SecretPath: key}); err != nil {
		t.Fatal(err)
	}
	return root, key
}

func TestWorkerMenuKeepsCurrentAndRetriesBaseURL(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "keep", true: "retry"}[retry], func(t *testing.T) {
			calls := fakeServices(t, false)
			root, key := workerMenuFixture(t, 200)
			base := "\n"
			if retry {
				d, cfg, err := menuProviderDeployment(context.Background(), root)
				if err != nil || d.Root != root {
					t.Fatal(err)
				}
				base = "http://remote.invalid\n" + cfg.BaseURL + "\n"
			}
			var out, errOut bytes.Buffer
			if err := menuWithRoot(strings.NewReader("6\n"+base+"\n\n\n\n\n\n7\n\n0\n"), &out, &errOut, root); err != nil {
				t.Fatal(err)
			}
			if strings.Count(out.String(), "PASS HTTP 2xx") != 2 || !strings.Contains(out.String(), "Saved.") || !strings.Contains(out.String(), "Reasoning effort") || strings.Contains(out.String(), "Thinking (") {
				t.Fatalf("Worker output=%s errors=%s", &out, &errOut)
			}
			if strings.Count(errOut.String(), "Nothing was saved.") != map[bool]int{false: 0, true: 1}[retry] {
				t.Fatal(errOut.String())
			}
			data, err := os.ReadFile(key)
			if err != nil || strings.TrimSpace(string(data)) != "fixture-key" {
				t.Fatal("current key not retained")
			}
			if !strings.Contains(strings.Join(*calls, "\n"), "reload-or-restart zoe-runner.service") {
				t.Fatal("Worker did not reload runner")
			}
		})
	}
}

func TestWorkerMenuProbeAndReloadFailuresPreserveConfiguration(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(map[bool]string{false: "probe", true: "reload"}[reload], func(t *testing.T) {
			fakeServices(t, reload)
			status := 401
			if reload {
				status = 200
			}
			root, key := workerMenuFixture(t, status)
			path := filepath.Join(root, "zoe-runner.env")
			before, _ := os.ReadFile(path)
			var out, errOut bytes.Buffer
			if err := menuWithRoot(strings.NewReader("6\n\nworker-changed\nhigh\n\n\n\n7\n\n0\n"), &out, &errOut, root); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(path)
			secret, _ := os.ReadFile(key)
			if !bytes.Equal(before, after) || string(secret) != "fixture-key\n" || !strings.Contains(errOut.String(), "Error:") || strings.Contains(out.String(), "Saved.") {
				t.Fatal("transaction failure not surfaced or configuration changed")
			}
			if reload && !strings.Contains(errOut.String(), "reload restored configuration") {
				t.Fatal("rollback reload failure missing")
			}
			if !reload && !strings.Contains(out.String(), "FAIL HTTP 4xx") {
				t.Fatal("Worker test failure output missing")
			}
		})
	}
}

func TestModelMenuReloadFailureAfterSave(t *testing.T) {
	containerKeyFixture(t)
	old := opsManager
	opsManager = func() ops.Manager {
		return ops.NewManager(func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "docker" {
				return nil, errors.New("simulated reload failure")
			}
			return nil, nil
		})
	}
	t.Cleanup(func() { opsManager = old })
	root := t.TempDir()
	var out, errOut bytes.Buffer
	err := menuModel(ConfigureMenuIO{Root: root, In: strings.NewReader("https://gateway.example\nm\nnone\ndisabled\n\nn\n"), Out: &out, ErrOut: &errOut, PromptSecret: func(io.Writer) ([]byte, error) { return []byte("fixture"), nil }}, "Register")
	if err == nil || !strings.Contains(err.Error(), "saved but service reload failed") || strings.Contains(err.Error()+errOut.String(), "Nothing was saved") {
		t.Fatalf("err=%v output=%s", err, &errOut)
	}
	data, readErr := os.ReadFile(filepath.Join(root, "zoe-stack.env"))
	if readErr != nil || !strings.Contains(string(data), `ZOE_REGISTER_MODEL="m"`) {
		t.Fatal("saved configuration lost")
	}
}

func TestModelMenuReloadAfterOptionalTestAndEOF(t *testing.T) {
	for _, answer := range []string{"y\n", ""} {
		t.Run(map[bool]string{true: "EOF", false: "test"}[answer == ""], func(t *testing.T) {
			containerKeyFixture(t)
			var events []string
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				events = append(events, "test")
				_, _ = io.WriteString(w, `{"output_text":"OK"}`)
			}))
			defer s.Close()
			old := opsManager
			opsManager = func() ops.Manager {
				return ops.NewManager(func(_ context.Context, name string, _ ...string) ([]byte, error) {
					if name == "docker" {
						events = append(events, "reload")
					}
					return nil, nil
				})
			}
			t.Cleanup(func() { opsManager = old })
			err := menuModel(ConfigureMenuIO{Root: t.TempDir(), In: strings.NewReader(s.URL + "\nm\nnone\ndisabled\n\n" + answer), Out: io.Discard, ErrOut: io.Discard, PromptSecret: func(io.Writer) ([]byte, error) { return []byte("fixture"), nil }}, "Register")
			want := "test,reload"
			if answer == "" {
				want = "reload"
				if !errors.Is(err, io.EOF) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if strings.Join(events, ",") != want {
				t.Fatalf("events=%v", events)
			}
		})
	}
}

type pauseErrorReader struct{ err error }

func (r pauseErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestMenuPauseEOFAndErrors(t *testing.T) {
	old := operations["status"]
	operations["status"] = func([]string, io.Reader, io.Writer, io.Writer) error { return errors.New("action failed") }
	t.Cleanup(func() { operations["status"] = old })
	broken := errors.New("input failed")
	for _, end := range []error{io.EOF, broken} {
		var out, errOut bytes.Buffer
		err := menuWithRoot(io.MultiReader(strings.NewReader("1\n"), pauseErrorReader{end}), &out, &errOut, t.TempDir())
		if (end == io.EOF && err != nil) || (end != io.EOF && !errors.Is(err, broken)) || strings.Count(out.String(), "Press Enter to return to the menu") != 1 || strings.Count(out.String(), "Zoe maintenance") != 1 {
			t.Fatalf("pause err=%v output=%s", err, &out)
		}
	}
	var out bytes.Buffer
	if err := menuWithRoot(strings.NewReader("99\n0\n"), &out, io.Discard, t.TempDir()); err != nil || strings.Contains(out.String(), "Press Enter") {
		t.Fatal("invalid choice or exit paused")
	}
}

func TestCLILoggingLevels(t *testing.T) {
	previous := logging.CurrentLevel()
	t.Cleanup(func() { logging.SetLevel(previous) })
	for value, want := range map[string]logging.Level{"": logging.Error, "invalid": logging.Error, "debug": logging.Debug, "info": logging.Info, "warn": logging.Warn, "error": logging.Error} {
		t.Setenv("ZOE_LOG_LEVEL", value)
		configureCLILogging()
		if logging.CurrentLevel() != want {
			t.Fatalf("wrong level for %s", value)
		}
	}
}
