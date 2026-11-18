package backoff_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func TestConfigAndPublishedState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	open := func() *storage.Store {
		db, err := storage.Open(ctx, storage.Config{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if err = storage.RegisterCoreMigrations(db); err != nil {
			t.Fatal(err)
		}
		if err = voice.RegisterMigrations(db); err != nil {
			t.Fatal(err)
		}
		if err = db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open()
	s := backoff.Store{DB: db.DB()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	key := []byte("fixture-sensitive-key")
	plain := sha256.Sum256(key)
	fingerprint := backoff.Fingerprint("endpoint", "model", "high", "enabled", key)
	if fingerprint == hex.EncodeToString(plain[:]) {
		t.Fatal("plain key hash persisted")
	}
	if changed, err := s.ObserveConfig(ctx, "compact", fingerprint); err != nil || changed {
		t.Fatalf("first observation %v %v", changed, err)
	}
	if err := s.Record(ctx, "compact", "private-owner", backoff.Auth, now, backoff.Background, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	defer db.Close()
	s.DB = db.DB()
	if changed, err := s.ObserveConfig(ctx, "compact", fingerprint); err != nil || changed {
		t.Fatalf("unchanged %v %v", changed, err)
	}
	if due, err := s.Due(ctx, "compact", "private-owner", now); err != nil || due {
		t.Fatalf("stop lost %v %v", due, err)
	}
	status := filepath.Join(dir, "status.json")
	if err := s.Publish(ctx, status, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), string(key)) || strings.Contains(string(data), "private-owner") {
		t.Fatal("private data in status")
	}
	info, err := os.Stat(status)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("status mode: %v %v", info, err)
	}
	snapshot, err := backoff.ReadSnapshot(status, now)
	if err != nil || len(snapshot.States) != 1 || !snapshot.States[0].AuthFailed {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	snapshot, err = backoff.ReadSnapshot(status, now.Add(25*time.Hour))
	if err != nil || len(snapshot.States) != 0 {
		t.Fatal("stale snapshot")
	}
	if changed, err := s.ObserveConfig(ctx, "compact", backoff.Fingerprint("endpoint", "model", "high", "enabled", []byte("new-key"))); err != nil || !changed {
		t.Fatalf("change %v %v", changed, err)
	}
	if due, err := s.Due(ctx, "compact", "private-owner", now); err != nil || !due {
		t.Fatalf("not cleared %v %v", due, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), string(key)) {
		t.Fatal("key in db")
	}
}
