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

	"github.com/ronigooja/Zoe/internal/ops"
)

func TestWorkerMenuModelQualification(t *testing.T) {
	for _, tc := range []struct {
		name, current, input, want string
	}{
		{"default-provider", "cliproxyapi/gpt-6.1-sol", "gpt-6.1-terra", "cliproxyapi/gpt-6.1-terra"},
		{"current-provider", "custom/gpt-6.1-sol", "gpt-6.1-terra", "custom/gpt-6.1-terra"},
		{"explicit-provider", "custom/gpt-6.1-sol", "other/gpt-6.1-terra", "other/gpt-6.1-terra"},
		{"blank-qualified", "custom/gpt-6.1-sol", "", "custom/gpt-6.1-sol"},
		{"unqualified-current-default", "gpt-6.1-sol", "gpt-6.1-terra", "cliproxyapi/gpt-6.1-terra"},
		{"blank-unqualified", "gpt-6.1-sol", "", "gpt-6.1-sol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeServices(t, false)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if (r.URL.Path != "/models" && r.URL.Path != "/responses") || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Error("wrong model probe endpoint or authorization")
				}
				if r.URL.Path == "/responses" {
					_, _ = io.WriteString(w, `{"output_text":"OK"}`)
					return
				}
				_, _ = io.WriteString(w, `{"data":[{"id":"gpt-6.1-sol"},{"id":"gpt-6.1-terra"}]}`)
			}))
			t.Cleanup(server.Close)
			root := t.TempDir()
			writeTestLimits(t, root)
			key := filepath.Join(root, "worker-key")
			if err := os.WriteFile(key, []byte("fixture-key\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := ops.ProviderConfig{BaseURL: server.URL, Model: tc.current, ReasoningEffort: "high", SecretPath: key}
			if err := ops.WriteRunnerEnvAtomic(filepath.Join(root, "zoe-runner.env"), cfg); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			m := configureMenuIO(root, strings.NewReader("\n"+tc.input+"\n\n\n\nn\n"), &out, &errOut)
			if err := menuWorker(m); err != nil {
				t.Fatal(err)
			}
			_, saved, err := menuProviderDeployment(context.Background(), root)
			if err != nil || saved.Model != tc.want || saved.ReasoningEffort != "high" {
				t.Fatalf("saved model=%q effort=%q err=%v", saved.Model, saved.ReasoningEffort, err)
			}
			if requests != 2 {
				t.Fatalf("model probe requests=%d", requests)
			}
			if !strings.Contains(out.String(), "Model [gpt-6.1-sol]: ") || strings.Contains(out.String(), "hidden") || strings.Contains(out.String(), "fixture-key") || errOut.Len() != 0 {
				t.Fatal("wrong current model display, hidden-input claim, secret output, or diagnostics")
			}
		})
	}
}

func TestWorkerMenuModelEOFCancelsWithoutSaving(t *testing.T) {
	fakeServices(t, false)
	root, _ := workerMenuFixture(t, 200)
	path := filepath.Join(root, "zoe-runner.env")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = menuWorker(configureMenuIO(root, strings.NewReader("\n"), &out, io.Discard))
	if !errors.Is(err, io.EOF) || !strings.Contains(out.String(), "Cancelled. Nothing was saved.") {
		t.Fatal("model EOF did not cancel")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("model EOF changed configuration")
	}
}
