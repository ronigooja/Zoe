package backoff_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func TestStorePersistenceConcurrencyAndClear(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	open := func() *storage.Store {
		s, err := storage.Open(ctx, storage.Config{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if err = storage.RegisterCoreMigrations(s); err != nil {
			t.Fatal(err)
		}
		if err = voice.RegisterMigrations(s); err != nil {
			t.Fatal(err)
		}
		if err = s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	}
	db := open()
	store := backoff.Store{DB: db.DB()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Record(ctx, "compact", "owner", backoff.Transient, now, backoff.Background, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	states, err := store.List(ctx)
	if err != nil || len(states) != 1 || states[0].Failures != 12 {
		t.Fatalf("states=%+v err=%v", states, err)
	}
	if due, err := store.Due(ctx, "compact", "owner", now); err != nil || due {
		t.Fatalf("due=%v err=%v", due, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	defer db.Close()
	store.DB = db.DB()
	states, err = store.List(ctx)
	if err != nil || len(states) != 1 || states[0].Failures != 12 || states[0].NextAttemptAt.IsZero() {
		t.Fatalf("restart states=%+v err=%v", states, err)
	}
	if err = store.Record(ctx, "compact", "owner", backoff.Auth, now, backoff.Background, nil); err != nil {
		t.Fatal(err)
	}
	if err = store.Clear(ctx, "compact", "owner"); err != nil {
		t.Fatal(err)
	}
	if due, _ := store.Due(ctx, "compact", "owner", now.Add(24*time.Hour)); due {
		t.Fatal("auth stop was cleared")
	}
	for i := 0; i < 2; i++ {
		if err = store.ClearAuth(ctx, "compact"); err != nil {
			t.Fatal(err)
		}
	}
	if due, err := store.Due(ctx, "compact", "owner", now); err != nil || !due {
		t.Fatalf("due=%v err=%v", due, err)
	}
}
