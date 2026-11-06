package voice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/summarize"
)

type fakeSummaryModel func(context.Context, []summarize.Turn) ([]summarize.Summary, error)

func (f fakeSummaryModel) Summarize(ctx context.Context, t []summarize.Turn) ([]summarize.Summary, error) {
	return f(ctx, t)
}

type summaryFixture struct {
	w     *summaryWorker
	t     *testing.T
	now   time.Time
	calls int
}

func newSummaryFixture(t *testing.T, turns int) *summaryFixture {
	t.Helper()
	ts, store, key := transcriptHTTPServer(t)
	f := &summaryFixture{t: t, now: time.Now().UTC()}
	f.w = &summaryWorker{db: store.DB(), now: func() time.Time { return f.now }, settle: time.Minute}
	f.w.model = fakeSummaryModel(func(_ context.Context, in []summarize.Turn) ([]summarize.Summary, error) {
		f.calls++
		return []summarize.Summary{{Topic: "topic", Summary: "safe", FirstSeq: in[0].Seq, LastSeq: in[len(in)-1].Seq}}, nil
	})
	for i := 0; i < turns; i++ {
		if status, _ := postTranscript(t, ts, key, "handoff-session", transcriptTurn(int64(i), "user", "PRIVATE_TRANSCRIPT_CANARY")); status != 200 {
			t.Fatalf("ingest status %d", status)
		}
	}
	f.exec(`UPDATE voice_sessions SET state='ended',ended_at=?,updated_at=? WHERE id='handoff-session'`, f.now.Format(time.RFC3339Nano), f.now.Format(time.RFC3339Nano))
	return f
}
func (f *summaryFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.w.db.Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *summaryFixture) step() {
	f.t.Helper()
	if err := f.w.step(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}
func (f *summaryFixture) advance(d time.Duration) { f.now = f.now.Add(d) }
func (f *summaryFixture) state() (string, string, int, int) {
	f.t.Helper()
	var status, reason string
	var attempts, revision int
	if err := f.w.db.QueryRow(`SELECT status,reason,attempts,revision FROM voice_summary_jobs`).Scan(&status, &reason, &attempts, &revision); err != nil {
		f.t.Fatal(err)
	}
	return status, reason, attempts, revision
}
func (f *summaryFixture) rows() int {
	f.t.Helper()
	var n int
	if err := f.w.db.QueryRow(`SELECT count(*) FROM voice_call_summaries`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestSummaryJobEnqueuedOnceOnRepeatedEnd(t *testing.T) {
	f := newSummaryFixture(t, 0)
	f.exec(`UPDATE voice_sessions SET state='ended' WHERE id='handoff-session'`)
	var n int
	if err := f.w.db.QueryRow(`SELECT count(*) FROM voice_summary_jobs WHERE session_id='handoff-session' AND generation=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("end job count=%d want 1", n)
	}
}
func TestSummaryWritesRangesAfterSettle(t *testing.T) {
	f := newSummaryFixture(t, 2)
	f.w.model = fakeSummaryModel(func(_ context.Context, in []summarize.Turn) ([]summarize.Summary, error) {
		f.calls++
		if len(in) != 2 || in[0].Seq != 0 || in[1].Seq != 1 || in[0].Text != "PRIVATE_TRANSCRIPT_CANARY" {
			t.Fatal("worker did not pass whole verbatim transcript")
		}
		return []summarize.Summary{{Topic: "topic", Summary: "safe", FirstSeq: 0, LastSeq: 1}}, nil
	})
	f.step()
	if f.calls != 0 {
		t.Fatal("model called before settle")
	}
	f.advance(time.Minute)
	f.step()
	var first, last int
	var started, ended string
	if err := f.w.db.QueryRow(`SELECT first_seq,last_seq,started_at,ended_at FROM voice_call_summaries`).Scan(&first, &last, &started, &ended); err != nil {
		t.Fatalf("summary row missing: %v", err)
	}
	if first != 0 || last != 1 || started != "2026-11-06T10:34:56.123456789Z" || ended != started {
		t.Fatalf("wrong stored mapping: %d %d %s %s", first, last, started, ended)
	}
	if status, _, _, _ := f.state(); status != "done" {
		t.Fatalf("status=%s", status)
	}
}
func TestSummaryInvalidRangesRetryFive(t *testing.T) {
	for _, ranges := range [][]summarize.Summary{
		{{Topic: "x", Summary: "y", FirstSeq: 0, LastSeq: 99}},
		{{Topic: "x", Summary: "y", FirstSeq: 0, LastSeq: 1}, {Topic: "x", Summary: "y", FirstSeq: 1, LastSeq: 1}},
		{{Topic: "x", Summary: "y", FirstSeq: 1, LastSeq: 0}},
	} {
		t.Run("invalid", func(t *testing.T) {
			f := newSummaryFixture(t, 2)
			f.w.model = fakeSummaryModel(func(context.Context, []summarize.Turn) ([]summarize.Summary, error) { f.calls++; return ranges, nil })
			f.advance(time.Minute)
			for i := 1; i <= 5; i++ {
				f.step()
				status, reason, attempts, _ := f.state()
				want := "pending"
				if i == 5 {
					want = "failed"
				}
				if status != want || attempts != i || reason != "invalid_summary" || f.rows() != 0 {
					t.Fatalf("attempt %d: status=%s reason=%s attempts=%d rows=%d", i, status, reason, attempts, f.rows())
				}
				f.step()
				if f.calls != i {
					t.Fatal("retry ignored backoff")
				}
				f.advance(20 * time.Minute)
			}
		})
	}
}
func TestSummaryUnconfiguredPendingWithoutLoop(t *testing.T) {
	f := newSummaryFixture(t, 1)
	f.w.model = nil
	f.advance(time.Minute)
	f.step()
	var before, after int
	if err := f.w.db.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		f.step()
		f.advance(time.Hour)
	}
	if err := f.w.db.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("disabled worker churned storage: %d -> %d", before, after)
	}
	status, reason, attempts, _ := f.state()
	if status != "pending" || reason != "model_not_configured" || attempts != 0 {
		t.Fatalf("unconfigured status=%s reason=%s attempts=%d", status, reason, attempts)
	}
	if f.rows() != 0 {
		t.Fatal("unconfigured wrote summaries")
	}
}
func TestSummaryZeroTurnsDone(t *testing.T) {
	f := newSummaryFixture(t, 0)
	f.w.model = nil
	f.advance(time.Minute)
	f.step()
	if status, _, _, _ := f.state(); status != "done" || f.rows() != 0 {
		t.Fatalf("empty call status=%s rows=%d", status, f.rows())
	}
}
func TestSummaryLateTurnsRegenerateExactlyOnce(t *testing.T) {
	f := newSummaryFixture(t, 1)
	f.advance(time.Minute)
	f.step()
	insert := func(seq int) {
		f.exec(`INSERT INTO voice_transcript_turns(workspace_id,session_id,seq,role,text,at,received_at) VALUES('default','handoff-session',?,'assistant','late',?,?)`, seq, f.now.Format(time.RFC3339Nano), f.now.Format(time.RFC3339Nano))
	}
	insert(1)
	if status, _, _, revision := f.state(); status != "pending" || revision != 1 {
		t.Fatalf("late turn status=%s revision=%d", status, revision)
	}
	f.step()
	if f.calls != 1 {
		t.Fatal("regeneration skipped settle")
	}
	f.advance(time.Minute)
	f.step()
	var last int
	if err := f.w.db.QueryRow(`SELECT last_seq FROM voice_call_summaries`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != 1 || f.rows() != 1 || f.calls != 2 {
		t.Fatalf("regenerated mapping last=%d rows=%d calls=%d", last, f.rows(), f.calls)
	}
	insert(2)
	f.advance(time.Minute)
	f.step()
	if status, _, _, revision := f.state(); status != "done" || revision != 1 || f.calls != 2 {
		t.Fatalf("extra regeneration status=%s revision=%d calls=%d", status, revision, f.calls)
	}
}
func TestSummaryNeverLogsTranscriptOrModelErrors(t *testing.T) {
	f := newSummaryFixture(t, 1)
	var captured bytes.Buffer
	old := log.Writer()
	log.SetOutput(&captured)
	defer log.SetOutput(old)
	f.w.model = fakeSummaryModel(func(context.Context, []summarize.Turn) ([]summarize.Summary, error) {
		return nil, errors.New("PRIVATE_TRANSCRIPT_CANARY")
	})
	f.advance(time.Minute)
	f.step()
	status, reason, attempts, _ := f.state()
	if status != "pending" || attempts != 1 || reason != "model_error" {
		t.Fatalf("model failure not recorded: %s %s %d", status, reason, attempts)
	}
	if strings.Contains(captured.String()+reason, "PRIVATE_TRANSCRIPT_CANARY") {
		t.Fatal("transcript leaked")
	}
	if !strings.Contains(captured.String(), "status=pending") {
		t.Fatal("missing safe status log")
	}
}
func TestSummaryWorkerStopsOnCancel(t *testing.T) {
	f := newSummaryFixture(t, 1)
	f.advance(time.Minute)
	entered := make(chan struct{})
	f.w.model = fakeSummaryModel(func(ctx context.Context, _ []summarize.Turn) ([]summarize.Summary, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.w.run(ctx) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never entered model")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop on cancellation")
	}
	if status, _, attempts, _ := f.state(); status != "pending" || attempts != 0 {
		t.Fatalf("cancel consumed attempt: %s %d", status, attempts)
	}
}

func TestSummaryReaperEnqueuesOnce(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 1, 0, 0, time.UTC)
	store, svc := newVoiceTestService(t, Config{HeartbeatTimeout: 30 * time.Second, Now: func() time.Time { return now }, GatewayURL: "https://gateway.test", GatewayAPIKey: "key", AccountMediaLifecycle: &testAccountMedia{verified: true}})
	svc.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	insertReaperSession(t, store, "stale", now.Add(-time.Minute).Format(time.RFC3339Nano), now.Add(-time.Minute).Format(time.RFC3339Nano))
	reapWithTimeout(t, svc)
	reapWithTimeout(t, svc)
	var n int
	if err := store.DB().QueryRow(`SELECT count(*) FROM voice_summary_jobs WHERE session_id='stale'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reaper job count=%d want 1", n)
	}
}

func TestSummaryTurnsDuringModelTriggerOneRegeneration(t *testing.T) {
	f := newSummaryFixture(t, 1)
	base := f.w.model
	f.w.model = fakeSummaryModel(func(ctx context.Context, in []summarize.Turn) ([]summarize.Summary, error) {
		f.exec(`INSERT INTO voice_transcript_turns(workspace_id,session_id,seq,role,text,at,received_at) VALUES('default','handoff-session',1,'assistant','late',?,?)`, f.now.Format(time.RFC3339Nano), f.now.Format(time.RFC3339Nano))
		return base.Summarize(ctx, in)
	})
	f.advance(time.Minute)
	f.step()
	if status, _, _, rev := f.state(); status != "pending" || rev != 1 {
		t.Fatalf("inflight late turn lost: status=%s revision=%d", status, rev)
	}
	f.w.model = base
	f.advance(time.Minute)
	f.step()
	if status, _, _, rev := f.state(); status != "done" || rev != 1 || f.calls != 2 {
		t.Fatalf("inflight replacement status=%s revision=%d calls=%d", status, rev, f.calls)
	}
}

func TestSummaryInvalidJSONRetriesWithoutNetwork(t *testing.T) {
	f := newSummaryFixture(t, 1)
	client, err := summarize.FromEnv(func(name string) string {
		return map[string]string{"ZOE_SUMMARY_BASE_URL": "https://summary.invalid", "ZOE_SUMMARY_MODEL": "fake", "ZOE_SUMMARY_API_KEY_FILE": "/not-read"}[name]
	}, func(string) ([]byte, error) { return []byte("fake-key"), nil })
	if err != nil {
		t.Fatal(err)
	}
	client.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		f.calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output_text":"PRIVATE_TRANSCRIPT_CANARY invalid JSON"}`))}, nil
	})})
	f.w.model = client
	f.advance(time.Minute)
	f.step()
	if status, reason, attempts, _ := f.state(); status != "pending" || reason != "model_error" || attempts != 1 || f.calls != 1 {
		t.Fatalf("invalid JSON not retried safely: %s %s %d calls=%d", status, reason, attempts, f.calls)
	}
}
