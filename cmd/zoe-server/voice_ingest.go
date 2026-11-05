package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Voice ingestion is a non-execution service with an immutable scope set.
func loadVoiceIngestCredentialFromEnv() (memoryRunnerCredential, error) {
	path := strings.TrimSpace(os.Getenv("ZOE_VOICE_INGEST_TOKEN_FILE"))
	if path == "" {
		return memoryRunnerCredential{}, nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return memoryRunnerCredential{}, fmt.Errorf("read voice ingest token file: %w", err)
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return memoryRunnerCredential{}, nil
	}
	if !validBearer(token) {
		return memoryRunnerCredential{}, errors.New("voice ingest token file must contain a 32-byte base64url credential")
	}
	if token == strings.TrimSpace(os.Getenv("ZOE_RUNNER_TOKEN")) || token == strings.TrimSpace(os.Getenv("ZOE_INSTANCE_KEY")) {
		return memoryRunnerCredential{}, errors.New("voice ingest credential must be distinct from owner and execution credentials")
	}
	id := env("ZOE_VOICE_INGEST_ID", "voice-gateway")
	if id == env("ZOE_RUNNER_ID", "configured-runner") || id == env("ZOE_MEMORY_RUNNER_ID", "memory-worker") || strings.ContainsAny(id, ", \t\r\n") {
		return memoryRunnerCredential{}, errors.New("voice ingest ID must be a distinct non-empty worker ID")
	}
	workspaces := splitEnv("ZOE_VOICE_INGEST_WORKSPACES", "default")
	if !validExactWorkspaceIDs(workspaces) || len(workspaces) == 0 {
		return memoryRunnerCredential{}, errors.New("voice ingest workspace allowlist must contain unique exact IDs")
	}
	return memoryRunnerCredential{Enabled: true, Token: token, RunnerID: id, WorkspaceIDs: workspaces, Scopes: []string{"voice:turn:ingest"}}, nil
}

func registerVoiceIngestCredential(ctx context.Context, store interface{ DB() *sql.DB }, credential memoryRunnerCredential) error {
	// Reuse the worker registration's digest storage, exact identity/allowlist
	// checks, and revocation preservation, but never accept caller-supplied scopes.
	credential.Scopes = []string{"voice:turn:ingest"}
	if err := registerMemoryRunnerCredential(ctx, store, credential); err != nil {
		return fmt.Errorf("voice ingest credential: %w", err)
	}
	return nil
}
