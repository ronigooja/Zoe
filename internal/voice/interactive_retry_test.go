package voice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/steward"
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
			if !errors.Is(s.interactiveModel(ctx, consumer, call), ErrModelConsumerStopped) || calls != 1 {
				t.Fatalf("consumer=%s status=%d calls=%d", consumer, status, calls)
			}
			var stopped bool
			if err = db.DB().QueryRowContext(ctx, `SELECT auth_failed FROM model_retry_state WHERE consumer=? AND scope=''`, consumer).Scan(&stopped); err != nil || !stopped {
				t.Fatalf("auth stop=%t err=%v", stopped, err)
			}
		}
	}
}

func TestInteractiveUnreadableStateCallsModel(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, storage.Config{Path: filepath.Join(t.TempDir(), "empty")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := newService(db.DB(), Config{})
	defer s.Stop()
	called := false
	if err := s.interactiveModel(ctx, "register", func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("called=%t err=%v", called, err)
	}
}

func TestInteractiveClassificationHTTPFailures(t *testing.T) {
	for _, consumer := range []string{"register", "steward"} {
		for _, status := range []int{503, 401, 403} {
			t.Run(fmt.Sprintf("%s_%d", consumer, status), func(t *testing.T) {
				var calls atomic.Int32
				model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(status) }))
				defer model.Close()
				_, db, _ := registerDisabledHTTPServer(t, Config{})
				if _, err := db.DB().Exec(`CREATE TABLE memory_write_outcomes(op_id TEXT PRIMARY KEY,status TEXT NOT NULL,error_kind TEXT NOT NULL DEFAULT '')`); err != nil {
					t.Fatal(err)
				}
				cfg := RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}
				s := newService(db.DB(), Config{ClassificationTimeout: 10 * time.Second})
				defer s.Stop()
				s.retry = backoff.Store{DB: db.DB()}
				if consumer == "register" {
					var err error
					s.register, err = NewRegister(db.DB(), RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: cfg, CompactModel: cfg, CompactCharacters: 1000})
					if err != nil {
						t.Fatal(err)
					}
					s.register.retryJitter = func() float64 { return .5 }
					s.register.retrySleep = func(context.Context, time.Duration) error { return nil }
				} else {
					values := map[string]string{"BASE_URL": model.URL, "MODEL": "fixture", "API_KEY_FILE": "fixture", "API_STYLE": "responses"}
					client, err := steward.FromEnv(func(k string) string { return values[strings.TrimPrefix(k, "ZOE_STEWARD_")] }, func(string) ([]byte, error) { return []byte("fixture"), nil })
					if err != nil {
						t.Fatal(err)
					}
					client.SetInstructions(testPrompts(t).Steward())
					s.steward = client
				}
				for i := 0; i < 2; i++ {
					id := fmt.Sprintf("retry-%d", i)
					_, err := db.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,exits,request) VALUES(?,'default','handoff-session',1,?,1,'test',X'01',? ,?,?,?)`, id, id, time.Now().UTC().Format(time.RFC3339Nano), id, func() any {
						if consumer == "steward" {
							return "task"
						}
						return nil
					}(), func() any {
						if consumer == "steward" {
							return "accepted request"
						}
						return nil
					}())
					if err != nil {
						t.Fatal(err)
					}
					s.classifyCommittedTurn(id)
					var exits, action, snapshot string
					if err := db.DB().QueryRow(`SELECT exits,steward_action,steward_snapshot FROM voice_turns WHERE id=?`, id).Scan(&exits, &action, &snapshot); err != nil {
						t.Fatal(err)
					}
					if consumer == "register" && (exits != "error" || action != "" || snapshot != "") {
						t.Fatalf("exits=%q action=%q snapshot=%q", exits, action, snapshot)
					}
					if consumer == "steward" && (exits != "task" || action != "error" || snapshot == "") {
						t.Fatalf("exits=%q action=%q snapshot=%q", exits, action, snapshot)
					}
					if status == 503 {
						break
					}
				}
				want := int32(1)
				if status == 503 {
					want = 3
				}
				if calls.Load() != want {
					t.Fatalf("calls=%d want=%d", calls.Load(), want)
				}
				if status != 503 {
					due, err := s.retry.Due(context.Background(), consumer, "", time.Now())
					if err != nil || due {
						t.Fatalf("due=%t err=%v", due, err)
					}
				}
				var tasks, writes int
				if err := db.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
					t.Fatal(err)
				}
				if err := db.DB().QueryRow(`SELECT count(*) FROM memory_write_outcomes`).Scan(&writes); err != nil {
					t.Fatal(err)
				}
				if tasks != 0 || writes != 0 {
					t.Fatalf("tasks=%d writes=%d", tasks, writes)
				}
			})
		}
	}
}

func TestInteractiveRegisterClassificationDeadline(t *testing.T) {
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer model.Close()
	_, db, _ := registerDisabledHTTPServer(t, Config{})
	cfg := RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"}
	s := newService(db.DB(), Config{ClassificationTimeout: 1500 * time.Millisecond})
	defer s.Stop()
	var err error
	s.register, err = NewRegister(db.DB(), RegisterConfig{RegisterInstruction: testPrompts(t).Register(), CompactInstruction: testPrompts(t).Compact(), Model: cfg, CompactModel: cfg, CompactCharacters: 1000})
	if err != nil {
		t.Fatal(err)
	}
	s.register.retryJitter = func() float64 { return .5 }
	var slept time.Duration
	s.register.retrySleep = func(ctx context.Context, d time.Duration) error {
		deadline, ok := ctx.Deadline()
		if !ok || d >= time.Until(deadline) {
			t.Errorf("sleep exceeds remaining deadline")
		}
		slept += d
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	_, err = db.DB().Exec(`INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES('deadline','default','handoff-session',1,'deadline',1,'test',X'01',?,'deadline')`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.classifyCommittedTurn("deadline")
	if elapsed := time.Since(start); elapsed > 2*time.Second || slept != time.Second || calls.Load() != 2 {
		t.Fatalf("elapsed=%v slept=%v calls=%d", elapsed, slept, calls.Load())
	}
	var exits string
	if err := db.DB().QueryRow(`SELECT exits FROM voice_turns WHERE id='deadline'`).Scan(&exits); err != nil || exits != "error" {
		t.Fatalf("exits=%q err=%v", exits, err)
	}
}
