package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/memoryapi"
)

// BatchInterval applies the deployment's hourly batch cadence; unset means 24h.
func BatchInterval(value string) (time.Duration, error) {
	if value == "" {
		return 24 * time.Hour, nil
	}
	hours, err := strconv.Atoi(value)
	if err != nil || hours < 1 || hours > 168 {
		return 0, errors.New("ZOE_MEMORY_BATCH_INTERVAL_HOURS must be between 1 and 168")
	}
	return time.Duration(hours) * time.Hour, nil
}

func ingestBodyFits(user, text string) bool {
	data, _ := json.Marshal(map[string]string{"user": user, "text": text})
	return len(data) <= 256*1024
}

// StartBatches starts no goroutine when the worker client is disabled.
func (r *Repository) StartBatches(ctx context.Context, client *memoryapi.Client, interval time.Duration) bool {
	if client == nil {
		return false
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				_ = r.batchStep(ctx, client, interval, now.UTC())
			}
		}
	}()
	return true
}

func (r *Repository) batchStep(ctx context.Context, client ingestClient, interval time.Duration, now time.Time) (stepErr error) {
	started := time.Now()
	owners, chunks := 0, 0
	defer func() {
		status := "ok"
		if stepErr != nil {
			status = "error"
		}
		logging.Infof("memory_batch status=%s owners=%d chunks=%d elapsed_ms=%d", status, owners, chunks, time.Since(started).Milliseconds())
	}()
	rows, err := r.db.QueryContext(ctx, `SELECT workspace_id,owner_id FROM voice_sessions WHERE owner_id<>'' UNION SELECT workspace_id,owner_subject_id FROM tasks WHERE owner_subject_id<>''`)
	if err != nil {
		return err
	}
	type owner struct{ workspace, principal string }
	var all []owner
	for rows.Next() {
		var o owner
		if err = rows.Scan(&o.workspace, &o.principal); err != nil {
			break
		}
		all = append(all, o)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, o := range all {
		n, err := r.runBatch(ctx, client, o.workspace, o.principal, now, interval)
		owners++
		chunks += n
		if err != nil && stepErr == nil {
			stepErr = err
		}
	}
	return stepErr
}
