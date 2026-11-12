package memory

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
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
	log.Printf("memory_batch status=scheduler_started interval_hours=%d", interval/time.Hour)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	// Registration precedes migrations; the first tick happens after startup.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = r.batchStep(ctx, model, interval, time.Now().UTC())
		}
	}
}

func (r *Repository) batchStep(ctx context.Context, model BatchModel, interval time.Duration, now time.Time) (stepErr error) {
	started := time.Now()
	ownersLoaded := false
	defer func() {
		// Owner discovery failures have no owner or loaded input to report.
		if stepErr != nil && !ownersLoaded {
			logBatchFailure(stepErr, "load_input", "", "", batchInput{}, 0, time.Since(started))
		}
	}()
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
	ownersLoaded = true
	for _, o := range owners {
		if _, err = r.RunBatch(ctx, o.workspace, o.principal, now, interval, model); err != nil && first == nil {
			first = err
		}
	}
	return first
}

var errBatchOutputTooLarge = errors.New("batch memory content exceeds limits")

// batchErrorKind applies only to batch diagnostics. Never include error text in
// logs: database, network and decoding errors may contain private input.
func batchErrorKind(err error, stage, kind string) (string, int) {
	if kind != "" {
		return kind, 0
	}
	if errors.Is(err, errBatchOutputTooLarge) {
		return "output_too_large", 0
	}
	if errors.Is(err, ErrVersionConflict) {
		return "write_conflict", 0
	}
	if stage == "call_model" {
		var httpErr *modelclient.HTTPError
		if errors.As(err, &httpErr) {
			return "model_http", httpErr.StatusCode
		}
		var timeout net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
			return "model_timeout", 0
		}
		// modelclient currently returns these fixed errors without typed causes.
		switch err.Error() {
		case "model client is not configured", "could not create model request":
			return "config", 0
		case "invalid model response", "model response was incomplete":
			return "output_invalid", 0
		default:
			return "model_transport", 0
		}
	}
	var parseErr *time.ParseError
	if stage == "load_input" && errors.As(err, &parseErr) {
		return "input", 0
	}
	switch stage {
	case "read_boundary", "load_input", "write", "commit":
		return "db", 0
	case "parse_output":
		return "output_invalid", 0
	default:
		return "other", 0
	}
}

// input_chars counts Unicode code points in the serialized model input, not
// bytes or tokens. Counts stay zero until input serialization succeeds.
func batchLogFields(principal string, in batchInput, chars int, elapsed time.Duration) string {
	return fmt.Sprintf("owner_hash=%s input_turns=%d input_tasks=%d input_staged=%d input_chars=%d elapsed_ms=%d", hashString(principal)[:8], len(in.Transcript), len(in.Tasks), len(in.ExplicitRequests), chars, elapsed.Milliseconds())
}

func logBatchFailure(err error, stage, kind, principal string, in batchInput, chars int, elapsed time.Duration) {
	kind, status := batchErrorKind(err, stage, kind)
	httpField := ""
	if status != 0 {
		httpField = fmt.Sprintf(" http_status=%d", status)
	}
	log.Printf("memory_batch status=failed stage=%s error_kind=%s%s %s", stage, kind, httpField, batchLogFields(principal, in, chars, elapsed))
}

func logBatchSuccess(principal string, in batchInput, chars, claims int, elapsed time.Duration) {
	log.Printf("memory_batch status=ok %s claims=%d staged=%d boundary_advanced=true", batchLogFields(principal, in, chars, elapsed), claims, len(in.ExplicitRequests))
}
