package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/ronigooja/Zoe/internal/control"
)

type workerCredential struct {
	Enabled  bool
	Token    string
	RunnerID string
	Scopes   []string
}

func registerWorkerCredential(ctx context.Context, store interface{ DB() *sql.DB }, credential workerCredential) error {
	if !credential.Enabled || store == nil || store.DB() == nil {
		return errors.New("enabled worker credential and open store are required")
	}
	if err := control.RegisterRunnerCredentialIfMissing(ctx, store, credential.Token, credential.RunnerID, credential.Scopes); err != nil {
		return err
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(credential.Token)
	if err != nil {
		return errors.New("worker credential is invalid")
	}
	hash := sha256.Sum256(tokenBytes)
	var runnerID string
	var scopesRaw []byte
	var revoked sql.NullString
	if err := store.DB().QueryRowContext(ctx, `SELECT runner_id,scopes,revoked_at FROM runner_credentials WHERE token_hash=?`, hash[:]).Scan(&runnerID, &scopesRaw, &revoked); err != nil {
		return err
	}
	var scopes []string
	if err := json.Unmarshal(scopesRaw, &scopes); err != nil {
		return err
	}
	if revoked.Valid || runnerID != credential.RunnerID || !sameStringSet(scopes, credential.Scopes) {
		return errors.New("stored worker credential does not match its configured ID and exact scopes")
	}
	return nil
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(got))
	for _, value := range got {
		if value == "" {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	for _, value := range want {
		if _, exists := seen[value]; !exists {
			return false
		}
	}
	return true
}
