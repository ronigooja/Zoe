package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
