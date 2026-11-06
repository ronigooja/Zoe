package voice

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/answer"
	"github.com/ronigooja/Zoe/internal/storage"
)

func recallRecoveryFixture(t *testing.T, cfg Config, count int) (*storage.Store, *Service) {
	t.Helper()
	_, store, _ := handoffHTTPServerWithConfig(t, Config{})
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC) }
	}
	svc := newService(store.DB(), cfg)
	t.Cleanup(svc.Stop)
	at := cfg.Now().UTC().Format(time.RFC3339Nano)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("job-%d", i)
		_, err := store.DB().Exec(`INSERT INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES(?,'default','handoff-session',1,?,'holding','recall_holding','delivered',?,?,?)`, "holding-"+id, id, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.DB().Exec(`INSERT INTO voice_recall_jobs(id,workspace_id,owner_id,session_id,generation,turn_id,question,holding_id) VALUES(?,'default','owner','handoff-session',1,?,'remember our conversations',?)`, id, id, "holding-"+id)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.DB().Exec(`INSERT INTO voice_call_summaries(id,workspace_id,session_id,topic,started_at,ended_at,first_seq,last_seq,summary,created_at) VALUES('summary','default','handoff-session','topic',?,?,1,1,'garden',?)`, at, at, at)
	if err != nil {
		t.Fatal(err)
	}
	return store, svc
}

func waitRecall(t *testing.T, test func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if test() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("recall condition did not become true")
}

func TestRecallRecoveryRestartAndLease(t *testing.T) {
	for _, mode := range []string{"restart", "lease", "existing_answer"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
			store, svc := recallRecoveryFixture(t, Config{Now: func() time.Time { return now }, RecallTimeout: time.Second}, 1)
			if mode == "lease" {
				svc.Start()
			}
			_, err := store.DB().Exec(`UPDATE voice_recall_jobs SET status='running'`)
			if err != nil {
				t.Fatal(err)
			}
			want := recallSpeech(recallUnavailable, nil)
			if mode == "existing_answer" {
				want = recallPrefix + "We discussed the garden."
				_, err = store.DB().Exec(`INSERT INTO voice_fast_replies(id,workspace_id,session_id,generation,turn_id,text,intent_kind,status,next_attempt_at,created_at,updated_at) VALUES('job-0','default','handoff-session',1,'recall-answer:job-0',?,'recall_answer','pending',?,?,?)`, want, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "lease" {
				svc.dispatchRecall(context.Background())
				var state string
				_ = store.DB().QueryRow(`SELECT status FROM voice_recall_jobs`).Scan(&state)
				if state != "running" {
					t.Fatal("job recovered before lease expiry")
				}
				now = now.Add(3 * time.Second)
				svc.dispatchRecall(context.Background())
			} else {
				svc.Start()
			}
			svc.dispatchRecall(context.Background())
			var state, text string
			var count int
			_ = store.DB().QueryRow(`SELECT status FROM voice_recall_jobs`).Scan(&state)
			_ = store.DB().QueryRow(`SELECT count(*),coalesce(max(text),'') FROM voice_fast_replies WHERE intent_kind='recall_answer'`).Scan(&count, &text)
			if state != "done" || count != 1 || text != want {
				t.Fatalf("recovery state=%s replies=%d text=%q", state, count, text)
			}
		})
	}
}

func TestRecallPersistenceRetriesThenFallback(t *testing.T) {
	var calls atomic.Int32
	model := recallModelFunc(func(context.Context, string, []answer.Candidate, []answer.Excerpt) (answer.Result, error) {
		calls.Add(1)
		return answer.Result{Status: "answer", Text: "We discussed the garden."}, nil
	})
	store, svc := recallRecoveryFixture(t, Config{Answerer: model}, 1)
	svc.Start()
	// ABORT rolls back the insert and job completion. Only final answer inserts
	// fail; a fallback must still be committed by the same transaction boundary.
	_, err := store.DB().Exec(`CREATE TRIGGER reject_recall_answer BEFORE INSERT ON voice_fast_replies WHEN NEW.intent_kind='recall_answer' AND NEW.text LIKE '%garden%' BEGIN SELECT RAISE(ABORT,'injected insert failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	svc.dispatchRecall(context.Background())
	waitRecall(t, func() bool {
		var status string
		_ = store.DB().QueryRow(`SELECT status FROM voice_recall_jobs`).Scan(&status)
		return status == "done"
	})
	if elapsed := time.Since(started); elapsed < 30*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("persistence retries were not bounded: %s", elapsed)
	}
	var text string
	_ = store.DB().QueryRow(`SELECT text FROM voice_fast_replies WHERE id='job-0'`).Scan(&text)
	if text != recallSpeech(recallUnavailable, nil) || calls.Load() != 1 {
		t.Fatalf("fallback text=%q model calls=%d", text, calls.Load())
	}
}

func TestRecallWorkersBoundedAndStopJoins(t *testing.T) {
	var active, peak, canceled atomic.Int32
	entered := make(chan struct{}, 12)
	release := make(chan struct{})
	defer close(release)
	model := recallModelFunc(func(ctx context.Context, _ string, _ []answer.Candidate, _ []answer.Excerpt) (answer.Result, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			canceled.Add(1)
		case <-release:
		}
		return answer.Result{}, ctx.Err()
	})
	store, svc := recallRecoveryFixture(t, Config{Answerer: model, RecallTimeout: 30 * time.Second}, 12)
	svc.Start()
	svc.dispatchRecall(context.Background())
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	var running int
	_ = store.DB().QueryRow(`SELECT count(*) FROM voice_recall_jobs WHERE status='running'`).Scan(&running)
	if running != 4 || peak.Load() > 4 {
		t.Errorf("unbounded workers: running=%d peak=%d", running, peak.Load())
	}
	stopped := make(chan struct{})
	go func() { svc.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not cancel and join recall workers")
	}
	if active.Load() != 0 || canceled.Load() != 4 {
		t.Fatalf("workers after Stop: active=%d canceled=%d", active.Load(), canceled.Load())
	}
	var done, pending int
	_ = store.DB().QueryRow(`SELECT count(*) FROM voice_recall_jobs WHERE status='done'`).Scan(&done)
	_ = store.DB().QueryRow(`SELECT count(*) FROM voice_recall_jobs WHERE status='pending'`).Scan(&pending)
	if done != 0 || pending != 8 {
		t.Fatalf("shutdown finalized work: done=%d pending=%d", done, pending)
	}
}
