package voice

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestInteractiveAuthStops(t *testing.T) {
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
	defer s.Stop()
	s.retryPublish = make(chan struct{}, 1)
	for _, consumer := range []string{"register", "steward"} {
		for _, status := range []int{401, 403} {
			if _, err = db.DB().ExecContext(ctx, `DELETE FROM model_retry_state WHERE consumer=?`, consumer); err != nil {
				t.Fatal(err)
			}
			calls := 0
			call := func() error { calls++; return &modelclient.HTTPError{StatusCode: status} }
			if s.interactiveModel(ctx, consumer, call) == nil {
				t.Fatal("expected auth error")
			}
			select {
			case <-s.retryPublish:
			default:
				t.Fatal("publication not requested")
			}
			if s.interactiveModel(ctx, consumer, call) == nil || calls != 1 {
				t.Fatalf("consumer=%s status=%d calls=%d", consumer, status, calls)
			}
			var stopped bool
			if err = db.DB().QueryRowContext(ctx, `SELECT auth_failed FROM model_retry_state WHERE consumer=? AND scope=''`, consumer).Scan(&stopped); err != nil || !stopped {
				t.Fatalf("auth stop=%t err=%v", stopped, err)
			}
		}
	}
}
