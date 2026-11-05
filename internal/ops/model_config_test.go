package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTriageConfigureWritesEnvAtomicallyAndPreservesOtherLines(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "stack.env")
	if err := os.WriteFile(p, []byte("# keep\nOTHER=value\nZOE_TRIAGE_MODEL=old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := TriageConfig{BaseURL: "https://gateway.example/v1", Model: "triage", SecretPath: filepath.Join(d, "key")}
	if err := WriteTriageEnvAtomic(p, c, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !strings.Contains(s, "# keep\nOTHER=value") || !strings.Contains(s, "ZOE_TRIAGE_BASE_URL=\"https://gateway.example/v1\"") {
		t.Fatalf("env=%s", s)
	}
}

func TestKeyStdinWritesSecretFileMode0600AndNeverEchoesKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	secret := []byte("private-key")
	if err := WriteTriageSecretAtomic(p, secret); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestConfigureRejectsBadURLsModelsAndWorldReadableKeyFile(t *testing.T) {
	for _, raw := range []string{"http://remote/v1", "https://gateway", "https://user:pass@gateway/v1", "https://gateway/v1?x=1"} {
		if err := ValidateModelBaseURL(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateKeyFile(p); err == nil {
		t.Fatal("accepted world-readable key")
	}
}

func TestTriageTestReportsCategoryAndLatencyAgainstFakeEndpoint(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"task"}]}]}`))
	}))
	defer s.Close()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("key"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := TestTriage(context.Background(), TriageConfig{BaseURL: s.URL + "/v1", Model: "m", SecretPath: p}, nil)
	if err != nil || r.Category != "task" || r.HTTPStatus != 200 || r.Latency < 0 {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}

func TestTriageTestUsesTheRuntimeRequestShape(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		instruction, ok := body["instructions"].(string)
		if !ok || !strings.Contains(instruction, "progress, recall, memory, task, or chat") {
			t.Fatalf("instructions=%v", body["instructions"])
		}
		if body["max_output_tokens"].(float64) < 128 {
			t.Fatalf("tokens=%v", body["max_output_tokens"])
		}
		if body["reasoning"] == nil {
			t.Fatal("missing reasoning")
		}
		if _, ok := body["temperature"]; ok {
			t.Fatal("temperature present")
		}
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"chat"}]}]}`))
	}))
	defer s.Close()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("key"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := TestTriage(context.Background(), TriageConfig{BaseURL: s.URL + "/v1", Model: "gpt-6.1-sol", SecretPath: p}, nil)
	if err != nil || r.Category != "chat" {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}

func TestTriageTestFailsWhenModelAnswersOutsideTheFiveCategories(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"Sure"}]}]}`))
	}))
	defer s.Close()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := TestTriage(context.Background(), TriageConfig{BaseURL: s.URL + "/v1", Model: "gpt-6.1-sol", SecretPath: p}, nil); err == nil {
		t.Fatal("unexpected success for invalid category")
	}
}

func TestTriageTestReports401HintAndNonZeroExit(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer s.Close()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("key"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := TestTriage(context.Background(), TriageConfig{BaseURL: s.URL + "/v1", Model: "m", SecretPath: p}, nil)
	if err == nil || err.Error() != "key rejected by endpoint" {
		t.Fatalf("err=%v", err)
	}
}

func TestTriageTestTimeout(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
	defer s.Close()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("key"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := TestTriage(ctx, TriageConfig{BaseURL: s.URL + "/v1", Model: "m", SecretPath: p}, nil)
	if err == nil {
		t.Fatal("timeout unexpectedly succeeded")
	}
}

func TestDisableClearsBaseURLButKeepsOtherValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stack.env")
	os.WriteFile(p, []byte("ZOE_TRIAGE_BASE_URL=\"https://x/v1\"\nZOE_TRIAGE_MODEL=\"m\"\n"), 0600)
	if err := WriteTriageEnvAtomic(p, TriageConfig{Model: "m", SecretPath: "/key"}, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "ZOE_TRIAGE_BASE_URL") || !strings.Contains(string(b), "ZOE_TRIAGE_MODEL") {
		t.Fatalf("env=%s", b)
	}
}

func TestMemoryConfigureAcceptsKeyStdinAndRotatesSecretFile(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "memory-key")
	if err := WriteSecretAtomic(p, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretAtomic(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "new" {
		t.Fatalf("key=%q err=%v", b, err)
	}
}
