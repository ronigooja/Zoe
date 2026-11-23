package memory

import (
	"context"
	"errors"
	"math/rand"
	"strconv"
	"time"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
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
	return r.batchStepWithJitter(ctx, client, interval, now, rand.Float64)
}

func (r *Repository) batchStepWithJitter(ctx context.Context, client ingestClient, interval time.Duration, now time.Time, jitter func() float64) (stepErr error) {
	lineage, err := storage.InstanceLineage(ctx, r.db)
	if err != nil {
		return err
	}
	store := backoff.Store{DB: r.db}
	if config, ok := client.(interface {
		ConfigDigest(context.Context) (string, error)
	}); ok {
		if digest, err := config.ConfigDigest(ctx); err == nil {
			for _, consumer := range []string{"memory_ingest", "memory_write"} {
				if _, err := store.ObserveConfig(ctx, consumer, digest); err != nil {
					return err
				}
			}
		}
	}
	started := time.Now()
	owners, chunks := 0, 0
	defer func() {
		status := "ok"
		if stepErr != nil {
			status = "error"
		}
		logging.Infof("memory_batch status=%s owners=%d chunks=%d elapsed_ms=%d", status, owners, chunks, time.Since(started).Milliseconds())
	}()
	rows, err := r.db.QueryContext(ctx, `SELECT owner_id FROM voice_sessions WHERE owner_id<>'' UNION SELECT owner_id FROM voice_context_events WHERE owner_id<>''`)
	if err != nil {
		return err
	}
	var all []string
	for rows.Next() {
		var o string
		if err = rows.Scan(&o); err != nil {
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
		scope := memoryapi.User(lineage, o)
		due, err := store.Due(ctx, "memory_ingest", scope, now)
		if err != nil {
			if stepErr == nil {
				stepErr = err
			}
			continue
		}
		if !due {
			continue
		}
		n, err := r.runBatch(ctx, client, o, now, interval)
		if err != nil {
			kind := backoff.Classify(err)
			if kind == backoff.Auth || kind == backoff.Transient {
				if e := store.Record(ctx, "memory_ingest", scope, kind, now, backoff.Background, jitter); e != nil {
					if stepErr == nil {
						stepErr = e
					}
				} else if r.OnRetryChange != nil {
					r.OnRetryChange()
				}
			}
		} else if n > 0 {
			if e := store.Clear(ctx, "memory_ingest", scope); e != nil {
				if stepErr == nil {
					stepErr = e
				}
			} else if r.OnRetryChange != nil {
				r.OnRetryChange()
			}
		}
		owners++
		chunks += n
		if err != nil && stepErr == nil {
			stepErr = err
		}
	}
	return stepErr
}
