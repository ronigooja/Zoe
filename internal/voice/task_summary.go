package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/modelclient"
)

// runTaskSummaries sweeps durable completion state, including tasks completed
// before startup. It runs independently of completion transactions and Compact.
func (r *ContextRegister) runTaskSummaries(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := r.summarizeTasks(ctx, time.Now()); err != nil && ctx.Err() == nil {
			logging.Warnf("task_summary status=sweep_failed error_kind=%s", backoff.Classify(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// summarizeTasks never holds a transaction across a model call. One global retry
// scope stops the consumer on authentication failure and persists its backoff.
func (r *ContextRegister) summarizeTasks(ctx context.Context, now time.Time) error {
	retry := backoff.Store{DB: r.db}
	due, err := retry.Due(ctx, "task_summary", "", now)
	if err != nil || !due {
		return err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM tasks WHERE status='complete' AND summary_result IS NULL ORDER BY created_at,id LIMIT ?`, r.limits.Int("task_summary_sweep_rows"))
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := r.summarizeTask(ctx, id); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			kind := backoff.Classify(err)
			// Invalid output and other model failures also retry without a bound.
			if kind != backoff.Auth {
				kind = backoff.Transient
			}
			if recordErr := retry.Record(ctx, "task_summary", "", kind, time.Now(), backoff.Background, rand.Float64); recordErr != nil {
				return recordErr
			}
			if r.requestPublish != nil {
				r.requestPublish()
			}
			return err
		}
		if err := retry.Clear(ctx, "task_summary", ""); err != nil {
			return err
		}
		if r.requestPublish != nil {
			r.requestPublish()
		}
	}
	return nil
}

func (r *ContextRegister) summarizeTask(ctx context.Context, id string) error {
	var input struct {
		MaxRunes  int      `json:"max_summary_characters"`
		Goal      string   `json:"goal"`
		Status    string   `json:"status"`
		Additions []string `json:"additions"`
		Result    string   `json:"result"`
	}
	input.MaxRunes = r.limits.Int("task_summary_max_runes")
	var version int64
	// Snapshot all input in one read, including additions in conversational order.
	var additions string
	err := r.db.QueryRowContext(ctx, `SELECT t.goal,t.status,t.result,t.version,
(SELECT json_group_array(content) FROM (SELECT content FROM task_entries WHERE task_id=t.id AND kind='user_message' AND julianday(created_at)>julianday(t.created_at) ORDER BY julianday(created_at),id))
FROM tasks t WHERE t.id=? AND t.status='complete' AND t.summary_result IS NULL`, id).Scan(&input.Goal, &input.Status, &input.Result, &version, &additions)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if err := json.Unmarshal([]byte(additions), &input.Additions); err != nil {
		return err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, r.compactTimeout)
	defer cancel()
	output, err := r.compactModel.Complete(modelclient.WithObservationComponent(callCtx, "task_summary"), r.taskSummaryInstruction, string(data))
	if err != nil {
		return err
	}
	output = strings.TrimSpace(output)
	if !utf8.ValidString(output) || utf8.RuneCountInString(output) > input.MaxRunes {
		return errors.New("invalid task summary output")
	}
	// A retry after an uncertain write is safe: it can only fill a still-NULL
	// summary, and the version/result fence rejects changed authoritative facts.
	_, err = r.db.ExecContext(ctx, `UPDATE tasks SET summary_result=? WHERE id=? AND status='complete' AND summary_result IS NULL AND version=? AND result=?`, output, id, version, input.Result)
	return err
}
