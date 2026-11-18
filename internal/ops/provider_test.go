package ops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSecretAtomicPrivateAndReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "token")
	if err := WriteSecretAtomic(path, []byte("old-token")); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretAtomic(path, []byte("new-token")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new-token" {
		t.Fatalf("secret = %q", got)
	}
}

func TestWriteRunnerEnvAtomicDoesNotIncludeSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zoe-runner.env")
	c := ProviderConfig{BaseURL: "http://127.0.0.1:18080/v1", Model: "gpt-test", SecretPath: "/tmp/secret"}
	if err := WriteRunnerEnvAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	for _, want := range []string{"ZOE_OPENCODE_PROVIDER_BASE_URL=", "ZOE_OPENCODE_PROVIDER_KEY_FILE=", "ZOE_OPENCODE_MODEL="} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if strings.Contains(s, "token") {
		t.Fatal("secret leaked into env")
	}
}

func TestProbeCategoriesAndModel(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		want      string
		available bool
	}{
		{"auth", http.StatusUnauthorized, `{}`, "authentication_failed", false},
		{"missing", http.StatusOK, `{"data":[{"id":"other"}]}`, "model_unavailable", false},
		{"ok", http.StatusOK, `{"data":[{"id":"gpt-test"}]}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Fatal("missing auth")
				}
				if r.URL.Path == "/responses" {
					_, _ = w.Write([]byte(`{"output_text":"OK"}`))
					return
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			secret := filepath.Join(t.TempDir(), "token")
			if err := WriteSecretAtomic(secret, []byte("secret")); err != nil {
				t.Fatal(err)
			}
			result := Probe(context.Background(), ProviderConfig{BaseURL: server.URL, Model: "gpt-test", SecretPath: secret}, server.Client())
			if result.ErrorCategory != tt.want || result.ModelAvailable != tt.available {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestProbeMatchesOpenCodeProviderPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			_, _ = w.Write([]byte(`{"output_text":"OK"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6.1-sol"},{"id":"org/model"}]}`))
	}))
	defer server.Close()
	for _, model := range []string{"cliproxyapi/gpt-6.1-sol", "gpt-6.1-sol", "org/model"} {
		result := Probe(context.Background(), ProviderConfig{BaseURL: server.URL, Model: model, Secret: "secret"}, server.Client())
		if !result.ModelAvailable || result.ErrorCategory != "" {
			t.Fatalf("model %q result=%+v", model, result)
		}
	}
	wrong := Probe(context.Background(), ProviderConfig{BaseURL: server.URL, Model: "cliproxyapi/other", Secret: "secret"}, server.Client())
	if wrong.ModelAvailable || wrong.ErrorCategory != "model_unavailable" {
		t.Fatalf("wrong model result=%+v", wrong)
	}
}

func TestProviderProbeBothEndpoints(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method+" "+r.URL.Path)
				if r.URL.Path == "/models" {
					_, _ = w.Write([]byte(`{"data":[{"id":"model"}]}`))
					return
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"output_text":"OK"}`))
			}))
			defer server.Close()
			result := Probe(context.Background(), ProviderConfig{BaseURL: server.URL, Model: "model", Secret: "fixture"}, server.Client())
			want := ""
			if status == 401 || status == 403 {
				want = "authentication_failed"
			} else if status != 200 {
				want = "provider_http_error"
			}
			if result.ErrorCategory != want || len(paths) != 2 || paths[0] != "GET /models" || paths[1] != "POST /responses" {
				t.Fatalf("result=%+v paths=%v", result, paths)
			}
		})
	}
}

func TestProbeInvalidConfigurationIncludesSafeReason(t *testing.T) {
	result := Probe(context.Background(), ProviderConfig{BaseURL: "https://gateway.example/v1?secret=do-not-leak", Model: "gpt-test", Secret: "secret"}, nil)
	if result.ErrorCategory != "invalid_configuration" || !strings.Contains(result.ErrorReason, "without credentials") || strings.Contains(result.ErrorReason, "do-not-leak") {
		t.Fatalf("result=%+v", result)
	}
}

func TestReadSecretDoesNotEchoValue(t *testing.T) {
	var prompt strings.Builder
	secret, err := ReadSecret(&prompt, func() ([]byte, error) { return []byte(" hidden-token \n"), nil })
	if err != nil || string(secret) != "hidden-token" {
		t.Fatalf("secret=%q err=%v", secret, err)
	}
	if strings.Contains(prompt.String(), "hidden-token") {
		t.Fatal("prompt leaked token")
	}
}
