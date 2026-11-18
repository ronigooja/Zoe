package ops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMemoryProbeResponsesOnly(t *testing.T) {
	for _, tc := range []struct {
		status         int
		body, category string
	}{
		{200, `{"output_text":"OK"}`, ""},
		{200, `{}`, "invalid_response"},
		{401, `{}`, "authentication_failed"},
		{403, `{}`, "authentication_failed"},
		{429, `{}`, "provider_http_error"},
		{500, `{}`, "provider_http_error"},
		{404, `{}`, "provider_http_error"},
	} {
		t.Run(tc.category+http.StatusText(tc.status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			result := probeMemory(context.Background(), MemoryConfig{BaseURL: server.URL + "/v1", Model: "test", Secret: "test-token"}, server.Client())
			if calls != 1 || result.ErrorCategory != tc.category || result.ModelAvailable != (tc.category == "") {
				t.Fatalf("calls=%d result=%+v", calls, result)
			}
		})
	}
}

func TestMemoryProbeStagedKeyCleanup(t *testing.T) {
	for _, status := range []int{200, 401} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			final := filepath.Join(tmp, "final-key")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := os.Stat(final); !os.IsNotExist(err) {
					t.Error("final key exists during probe")
				}
				files, err := filepath.Glob(filepath.Join(tmp, "zoe-memory-probe-*"))
				if err != nil || len(files) != 1 {
					t.Errorf("temporary key count=%d err=%v", len(files), err)
				} else if info, err := os.Stat(files[0]); err != nil || info.Mode().Perm() != 0600 {
					t.Error("temporary key permissions")
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"output_text":"OK"}`))
			}))
			defer server.Close()
			probeMemory(context.Background(), MemoryConfig{BaseURL: server.URL, Model: "m", Secret: "fixture", SecretPath: final}, server.Client())
			files, _ := filepath.Glob(filepath.Join(tmp, "zoe-memory-probe-*"))
			if len(files) != 0 {
				t.Error("temporary key remains")
			}
		})
	}
}

func TestMemoryProbeTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer server.Close()
	client := server.Client()
	client.Timeout = 10 * time.Millisecond
	result := probeMemory(context.Background(), MemoryConfig{BaseURL: server.URL, Model: "m", Secret: "fixture"}, client)
	if result.ErrorCategory != "timeout" {
		t.Fatalf("result=%+v", result)
	}
}
