package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ronigooja/Zoe/internal/storage"
)

func TestIngestProvisioningUsesFixedScope(t *testing.T) {
	t.Setenv("ZOE_VOICE_INGEST_TOKEN_FILE", "")
	credential, err := loadVoiceIngestCredentialFromEnv()
	if err != nil || credential.Enabled {
		t.Fatalf("absent token enabled=%v err=%v", credential.Enabled, err)
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOE_VOICE_INGEST_TOKEN_FILE", path)
	credential, err = loadVoiceIngestCredentialFromEnv()
	if err != nil || credential.Enabled {
		t.Fatalf("empty token enabled=%v err=%v", credential.Enabled, err)
	}
	token := "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCA"
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOE_VOICE_INGEST_SCOPES", "*")
	t.Setenv("ZOE_VOICE_INGEST_ID", "")
	t.Setenv("ZOE_VOICE_INGEST_WORKSPACES", "")
	credential, err = loadVoiceIngestCredentialFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !credential.Enabled || credential.RunnerID != "voice-gateway" || !sameStringSet(credential.WorkspaceIDs, []string{"default"}) || !sameStringSet(credential.Scopes, []string{"voice:turn:ingest"}) {
		t.Fatalf("credential metadata enabled=%v id=%s workspaces=%v scopes=%v", credential.Enabled, credential.RunnerID, credential.WorkspaceIDs, credential.Scopes)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	credential.Scopes = []string{"*"}
	if err := registerVoiceIngestCredential(ctx, store, credential); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := store.DB().QueryRow(`SELECT scopes FROM runner_credentials WHERE runner_id='voice-gateway'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var scopes []string
	if err := json.Unmarshal(raw, &scopes); err != nil || !sameStringSet(scopes, []string{"voice:turn:ingest"}) {
		t.Fatalf("stored scopes=%v err=%v", scopes, err)
	}
	if _, err := store.DB().Exec(`UPDATE runner_credentials SET revoked_at='revoked' WHERE runner_id='voice-gateway'`); err != nil {
		t.Fatal(err)
	}
	if err := registerVoiceIngestCredential(ctx, store, credential); err == nil {
		t.Fatal("revoked credential silently reprovisioned")
	}
	t.Setenv("ZOE_VOICE_INGEST_WORKSPACES", "*")
	if _, err := loadVoiceIngestCredentialFromEnv(); err == nil {
		t.Fatal("wildcard workspace accepted")
	}
}
