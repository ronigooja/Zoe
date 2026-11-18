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

func TestRetryPublisher(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storage.Open(ctx, storage.Config{Path: filepath.Join(dir, "db")})
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
	path := filepath.Join(dir, "status")
	s := newService(db.DB(), Config{})
	s.retry = backoff.Store{DB: db.DB()}
	s.retryStatusPath = path
	s.retryPublish = make(chan struct{}, 1)
	defer s.Stop()
	if err = s.AfterMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("publisher timed out")
	}
	wait(func() bool { _, err := os.Stat(path); return err == nil })
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	before, err := old.Stat()
	if err != nil || before.Mode().Perm() != 0644 {
		t.Fatalf("mode: %v %v", before, err)
	}
	if err = s.retry.Record(ctx, "register", "", backoff.Auth, time.Now(), backoff.Interactive, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s.PublishRetryState()
	}
	wait(func() bool {
		snap, err := backoff.ReadSnapshot(path, time.Now())
		return err == nil && len(snap.States) == 1 && snap.States[0].AuthFailed
	})
	after, err := os.Stat(path)
	if err != nil || os.SameFile(before, after) {
		t.Fatal("snapshot was not atomically replaced")
	}
	oldBytes := make([]byte, before.Size())
	if _, err = old.Read(oldBytes); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(oldBytes, []byte("register")) {
		t.Fatal("old snapshot changed")
	}
	time.Sleep(2300 * time.Millisecond)
	again, err := os.Stat(path)
	if err != nil || !os.SameFile(after, again) {
		t.Fatal("requests were not coalesced")
	}
}

func TestRetryPublisherMissingDirectory(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, storage.Config{Path: filepath.Join(t.TempDir(), "db")})
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
	s := newService(db.DB(), Config{})
	s.retry = backoff.Store{DB: db.DB()}
	s.retryStatusPath = filepath.Join(t.TempDir(), "missing", "status")
	s.retryPublish = make(chan struct{}, 1)
	if err = s.AfterMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	s.PublishRetryState()
	s.Stop()
}
