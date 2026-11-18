package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/logging"
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
	store := backoff.Store{DB: db.DB()}
	model := RetryModelConfig{Endpoint: "https://example.invalid", Model: "model", Credential: key}
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
	var captured lockedLog
	oldWriter, oldLevel := log.Writer(), logging.CurrentLevel()
	log.SetOutput(&captured)
	logging.SetLevel(logging.Debug)
	defer func() { log.SetOutput(oldWriter); logging.SetLevel(oldLevel) }()
	if err = s.AfterMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.PublishRetryState(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publication request blocked")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(captured.String(), "model_retry_publish_failed") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s.Stop()
	if !strings.Contains(captured.String(), "model_retry_publish_failed") {
		t.Fatal("missing failure category")
	}
}

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestRetryObserveFailureContinues(t *testing.T) {
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
	if _, err = db.DB().Exec(`CREATE TRIGGER fail_register BEFORE INSERT ON model_config_fingerprints WHEN NEW.consumer='register' BEGIN SELECT RAISE(FAIL, 'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	cfg := RetryModelConfig{Endpoint: "https://example.invalid", Model: "fixture", Credential: []byte("fixture")}
	s := newService(db.DB(), Config{})
	s.retry = backoff.Store{DB: db.DB()}
	s.retryModels = map[string]RetryModelConfig{"register": cfg, "steward": cfg}
	s.retryStatusPath = filepath.Join(dir, "status")
	s.retryPublish = make(chan struct{}, 1)
	defer s.Stop()
	if err = s.AfterMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.DB().QueryRow(`SELECT count(*) FROM model_config_fingerprints WHERE consumer='steward'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("other consumer not observed: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = os.Stat(s.retryStatusPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("publisher did not start")
}

func TestRetryCredentialNotPersisted(t *testing.T) {
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
	key := []byte("fixture-key-not-in-any-table")
	s := newService(db.DB(), Config{})
	s.retry = backoff.Store{DB: db.DB()}
	s.retryPublish = make(chan struct{}, 1)
	s.retryModels = map[string]RetryModelConfig{"register": {Endpoint: "https://example.invalid", Model: "fixture", Credential: key}}
	s.retryStatusPath = filepath.Join(dir, "status")
	defer s.Stop()
	if err = s.AfterMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var published []byte
	for time.Now().Before(deadline) {
		published, err = os.ReadFile(s.retryStatusPath)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(key)
	check := func(data []byte) {
		t.Helper()
		if bytes.Contains(data, key) || bytes.Contains(data, []byte(fmt.Sprintf("%x", sum))) {
			t.Fatal("credential material persisted")
		}
	}
	check(published)
	tables, err := db.DB().Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err = tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err = tables.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		rows, err := db.DB().Query(`SELECT * FROM "` + strings.ReplaceAll(name, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]sql.RawBytes, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if err = rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			for _, value := range values {
				check(value)
			}
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
}
