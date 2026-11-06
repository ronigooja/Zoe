package voice

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/internal/summarize"
)

type Summarizer interface {
	Summarize(context.Context, []summarize.Turn) ([]summarize.Summary, error)
}

// SummarySettleDelay parses the optional deployment setting without reading secrets.
func SummarySettleDelay(value string) (time.Duration, error) {
	if value == "" {
		return time.Minute, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 || seconds > 600 {
		return 0, errors.New("ZOE_SUMMARY_SETTLE_SECONDS must be between 0 and 600")
	}
	return time.Duration(seconds) * time.Second, nil
}

type summaryWorker struct {
	db     *sql.DB
	model  Summarizer
	now    func() time.Time
	settle time.Duration
	mu     sync.Mutex
}

func (w *summaryWorker) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := w.step(ctx); err != nil && ctx.Err() == nil {
			// Database errors can contain data; never interpolate them.
			log.Print("voice_summary status=storage_error")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// step performs at most one job. The mutex bounds model concurrency to one,
// while the durable claim and expiry also fence another server's worker.
func (w *summaryWorker) step(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := w.now()
	var session, workspace, settled string
	var generation int64
	var attempts, revision int
	err := w.db.QueryRowContext(ctx, `SELECT session_id,generation,workspace_id,settled_from,attempts,revision FROM voice_summary_jobs WHERE (status='pending' OR status='running') AND next_attempt_ms<=? AND (reason<>'model_not_configured' OR ?) ORDER BY next_attempt_ms,session_id LIMIT 1`, now.UnixMilli(), w.model != nil).Scan(&session, &generation, &workspace, &settled, &attempts, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	from, err := time.Parse(time.RFC3339Nano, settled)
	if err != nil {
		return errors.New("invalid summary settle timestamp")
	}
	if now.Before(from.Add(w.settle)) {
		_, err = w.db.ExecContext(ctx, `UPDATE voice_summary_jobs SET next_attempt_ms=? WHERE session_id=? AND generation=? AND status='pending'`, from.Add(w.settle).UnixMilli(), session, generation)
		return err
	}
	// An unconfigured deployment records a stable pending status, not attempts
	// or repeated model errors. Zero-turn jobs can still complete below.
	turns, err := ReadTranscriptTurns(ctx, w.db, workspace, session, 0, math.MaxInt64)
	if err != nil {
		return err
	}
	input := make([]summarize.Turn, len(turns))
	for i, t := range turns {
		input[i] = summarize.Turn{Seq: int(t.Seq), Role: t.Role, Text: t.Text}
	}
	if summarize.HasText(input) && w.model == nil {
		_, err = w.db.ExecContext(ctx, `UPDATE voice_summary_jobs SET status='pending',reason='model_not_configured',next_attempt_ms=0 WHERE session_id=? AND generation=? AND next_attempt_ms<=? AND reason<> 'model_not_configured'`, session, generation, now.UnixMilli())
		return err
	}
	lease := now.Add(11 * time.Minute).UnixMilli()
	claim, err := w.db.ExecContext(ctx, `UPDATE voice_summary_jobs SET status='running',next_attempt_ms=?,reason='' WHERE session_id=? AND generation=? AND revision=? AND (status='pending' OR status='running') AND next_attempt_ms<=?`, lease, session, generation, revision, now.UnixMilli())
	if err != nil {
		return err
	}
	n, err := claim.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	var result []summarize.Summary
	reason := ""
	if summarize.HasText(input) {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		result, err = w.model.Summarize(callCtx, input)
		cancel()
		if err != nil {
			reason = "model_error"
		} else if !validSummaryRanges(turns, result) {
			reason = "invalid_summary"
		}
	}
	if ctx.Err() != nil {
		// Do not consume an attempt on shutdown; a fresh worker can resume it.
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err = w.db.ExecContext(cleanup, `UPDATE voice_summary_jobs SET status='pending',next_attempt_ms=0 WHERE session_id=? AND generation=? AND status='running' AND next_attempt_ms=?`, session, generation, lease)
		return err
	}
	if reason != "" {
		attempts++
		status := "pending"
		if attempts >= 5 {
			status = "failed"
		}
		_, err = w.db.ExecContext(ctx, `UPDATE voice_summary_jobs SET status=?,reason=?,attempts=?,next_attempt_ms=? WHERE session_id=? AND generation=? AND status='running' AND next_attempt_ms=?`, status, reason, attempts, w.now().Add(time.Duration(1<<uint(attempts-1))*time.Minute).UnixMilli(), session, generation, lease)
		log.Printf("voice_summary session_id=%s generation=%d status=%s attempts=%d", session, generation, status, attempts)
		return err
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Fence stale claimants before replacing a previous revision's rows.
	var held int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM voice_summary_jobs WHERE session_id=? AND generation=? AND status='running' AND next_attempt_ms=?`, session, generation, lease).Scan(&held); err != nil {
		return err
	}
	if held == 0 {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM voice_call_summaries WHERE session_id=?`, session); err != nil {
		return err
	}
	bySeq := make(map[int64]TranscriptTurn, len(turns))
	for _, t := range turns {
		bySeq[t.Seq] = t
	}
	for _, s := range result {
		_, err = tx.ExecContext(ctx, `INSERT INTO voice_call_summaries(id,workspace_id,session_id,topic,started_at,ended_at,first_seq,last_seq,summary,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, uuid.NewString(), workspace, session, s.Topic, bySeq[int64(s.FirstSeq)].At, bySeq[int64(s.LastSeq)].At, s.FirstSeq, s.LastSeq, s.Summary, w.now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM voice_transcript_turns WHERE session_id=?`, session).Scan(&count); err != nil {
		return err
	}
	status := "done"
	if count > len(turns) && revision == 0 {
		status = "pending"
		revision = 1
	}
	_, err = tx.ExecContext(ctx, `UPDATE voice_summary_jobs SET status=?,reason='',attempts=0,revision=?,next_attempt_ms=0,settled_from=? WHERE session_id=? AND generation=?`, status, revision, w.now().UTC().Format(time.RFC3339Nano), session, generation)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("voice_summary session_id=%s generation=%d status=%s rows=%d", session, generation, status, len(result))
	return nil
}

func validSummaryRanges(turns []TranscriptTurn, result []summarize.Summary) bool {
	input := make([]summarize.Turn, len(turns))
	for i, t := range turns {
		input[i] = summarize.Turn{Seq: int(t.Seq), Role: t.Role, Text: t.Text}
	}
	return summarize.Validate(input, result) == nil
}
