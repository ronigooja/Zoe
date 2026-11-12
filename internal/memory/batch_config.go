package memory

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/ronigooja/Zoe/internal/modelclient"
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

// BatchModelFromEnv reuses the existing deployment-selected memory endpoint.
// Unlike Mem0's per-claim add workflow, this call is stateless and writes only
// through the repository's batch-only trusted write flow.
func BatchModelFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (*modelclient.Client, error) {
	return modelclient.FromEnv("ZOE_MEM0_LLM", getenv, readFile, 120000, 5000, 600000)
}

func (r *Repository) runBatches(ctx context.Context, model BatchModel, interval time.Duration) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	// Registration precedes migrations; the first tick happens after startup.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.batchStep(ctx, model, interval, time.Now().UTC()); err != nil && ctx.Err() == nil {
				log.Print("memory_batch status=failed")
			}
		}
	}
}

func (r *Repository) batchStep(ctx context.Context, model BatchModel, interval time.Duration, now time.Time) error {
	rows, err := r.db.QueryContext(ctx, `SELECT workspace_id,owner_id FROM voice_sessions UNION SELECT workspace_id,owner_subject_id FROM tasks WHERE owner_subject_id<>'' UNION SELECT workspace_id,principal_id FROM memory_staged_writes WHERE status='pending'`)
	if err != nil {
		return err
	}
	type owner struct{ workspace, principal string }
	var owners []owner
	for rows.Next() {
		var o owner
		if err = rows.Scan(&o.workspace, &o.principal); err != nil {
			break
		}
		owners = append(owners, o)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var first error
	for _, o := range owners {
		if _, err = r.RunBatch(ctx, o.workspace, o.principal, now, interval, model); err != nil && first == nil {
			first = err
		}
	}
	return first
}
