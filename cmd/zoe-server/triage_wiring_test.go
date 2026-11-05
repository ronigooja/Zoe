package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ronigooja/Zoe/internal/triage"
)

func TestTriageAdapterForwardsCategoryFromConfiguredEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","output_text":"progress"}`))
	}))
	defer srv.Close()
	key := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(key, []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"ZOE_TRIAGE_BASE_URL": srv.URL + "/v1", "ZOE_TRIAGE_MODEL": "gpt-6.1-sol", "ZOE_TRIAGE_API_KEY_FILE": key}
	client, err := triage.FromEnv(func(k string) string { return env[k] }, os.ReadFile)
	if err != nil || client == nil {
		t.Fatalf("client=%v err=%v", client, err)
	}
	got, err := triageAdapter{client}.Classify(context.Background(), "好了吗")
	if err != nil || got != "progress" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestTriageAdapterReturnsErrorWhenEndpointFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	key := filepath.Join(t.TempDir(), "k")
	_ = os.WriteFile(key, []byte("test-key\n"), 0o600)
	env := map[string]string{"ZOE_TRIAGE_BASE_URL": srv.URL + "/v1", "ZOE_TRIAGE_MODEL": "m", "ZOE_TRIAGE_API_KEY_FILE": key}
	client, _ := triage.FromEnv(func(k string) string { return env[k] }, os.ReadFile)
	if _, err := (triageAdapter{client}).Classify(context.Background(), "x"); err == nil {
		t.Fatal("expected error")
	}
}
