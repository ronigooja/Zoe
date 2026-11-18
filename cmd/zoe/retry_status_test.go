package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func TestWriteRetryStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status")
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	writeRetryStatus(&out, path, now)
	if out.Len() != 0 {
		t.Fatal("missing status produced output")
	}
	data := `{"published_at":"2026-11-18T10:00:00Z","states":[{"consumer":"memory_ingest","auth_failed":true},{"consumer":"compact","failures":3,"next_attempt_at":"2026-11-18T10:30:00Z"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	writeRetryStatus(&out, path, now)
	if !strings.Contains(out.String(), "memory_ingest: stopped (authentication failed)") || !strings.Contains(out.String(), "compact: backing off, 3 failures, next attempt 2026-11-18T10:30:00Z") {
		t.Fatal(out.String())
	}
	out.Reset()
	writeRetryStatus(&out, path, now.Add(25*time.Hour))
	if out.Len() != 0 {
		t.Fatal("stale status produced output")
	}
}

func TestWriteRetryStatusPublishedStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storage.Open(ctx, storage.Config{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := storage.RegisterCoreMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err := voice.RegisterMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	store := backoff.Store{DB: db.DB()}
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	if err := store.Record(ctx, "compact", "private-owner", backoff.Transient, now, backoff.Background, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(ctx, "register", "", backoff.Auth, now, backoff.Interactive, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "status")
	if err := store.Publish(ctx, path, now); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writeRetryStatus(&out, path, now)
	if !strings.Contains(out.String(), "register: stopped (authentication failed)") || !strings.Contains(out.String(), "compact: backing off, 1 failures, next attempt 2026-11-18T10:00:30Z") {
		t.Fatal(out.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private-owner")) {
		t.Fatal("published snapshot exposes scope")
	}
}
