package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func TestComposedServerUsesWorkerOnlyMemoryAndRegistersExport(t *testing.T) {
	t.Setenv("ZOE_MEM0_LLM_BASE_URL", "invalid worker-only URL")
	t.Setenv("ZOE_MEM0_LLM_MODEL", "")
	t.Setenv("ZOE_MEMORY_BATCH_INTERVAL_HOURS", "24")
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := control.RegisterInstanceMaintenanceMigration(store); err != nil {
		t.Fatal(err)
	}
	server, err := composeControlServer(store, control.Config{}, voice.Config{}, capabilities.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var oldNamespace, ingestVersion int
	if err := store.DB().QueryRow(`SELECT count(*) FROM schema_migrations WHERE namespace='memory'`).Scan(&oldNamespace); err != nil || oldNamespace != 0 {
		t.Fatalf("removed namespace=%d err=%v", oldNamespace, err)
	}
	if err := store.DB().QueryRow(`SELECT version FROM schema_migrations WHERE namespace='memory_ingest'`).Scan(&ingestVersion); err != nil || ingestVersion != 222 {
		t.Fatalf("ingest version=%d err=%v", ingestVersion, err)
	}
	var memoryTables int
	if err := store.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name GLOB 'memory_*'`).Scan(&memoryTables); err != nil || memoryTables != 1 {
		t.Fatalf("expected only worker ingestion state, tables=%d err=%v", memoryTables, err)
	}
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes[:])
	if _, _, err := control.InitializeInstanceAuth(ctx, store, token); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/v1/export?scope=personal", http.StatusOK},
		{"/v1/voice/config", http.StatusServiceUnavailable},
		{"/v1/memory", http.StatusNotFound},
		{"/v1/memory/context", http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Fatalf("path=%s status=%d body=%s", tc.path, response.Code, response.Body.String())
		}
	}
}

func TestVoiceAccountMediaAdapterRequiresExplicitSecureGatewayConfiguration(t *testing.T) {
	t.Setenv("ZOE_VOICE_ACCOUNT_MEDIA_ENABLED", "true")
	if _, err := composeControlServer(nil, control.Config{}, voice.Config{}, capabilities.Config{}); err == nil {
		t.Fatal("account media adapter enabled without a configured HTTPS gateway and deployment key")
	}
}
