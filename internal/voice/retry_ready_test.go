package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestRetryAfterMigrateObservation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db")
	db, err := storage.Open(ctx, storage.Config{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = storage.RegisterCoreMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err = RegisterMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key := []byte("fixture-credential-do-not-persist")
	keyPath := filepath.Join(dir, "key")
	if err = os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	store := backoff.Store{DB: db.DB()}
	model := RetryModelConfig{Endpoint: "https://example.invalid", Model: "model", CredentialFile: keyPath}
	newService := func() *Service {
		return &Service{retry: store, retryModels: map[string]RetryModelConfig{"register": model}}
	}
	if err = store.Record(ctx, "register", "", backoff.Auth, time.Now(), backoff.Interactive, nil); err != nil {
		t.Fatal(err)
	}
	if err = newService().AfterMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	if due, err := store.Due(ctx, "register", "", time.Now()); err != nil || due {
		t.Fatal("first observation cleared stop", err)
	}
	if err = newService().AfterMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	if due, err := store.Due(ctx, "register", "", time.Now()); err != nil || due {
		t.Fatal("unchanged observation cleared stop", err)
	}
	model.Model = "changed"
	if err = newService().AfterMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	if due, err := store.Due(ctx, "register", "", time.Now()); err != nil || !due {
		t.Fatal("changed observation retained stop", err)
	}
	var fingerprint string
	if err = db.DB().QueryRowContext(ctx, `SELECT fingerprint FROM model_config_fingerprints WHERE consumer='register'`).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(key)
	if bytes.Contains([]byte(fingerprint), key) || fingerprint == fmt.Sprintf("%x", sum) {
		t.Fatal("credential leaked")
	}
}
