package control

import (
	"context"
	"database/sql"

	"github.com/ronigooja/Zoe/contracts"
)

func executionCapacityAvailable(active, maxRuns int) bool { return active < maxRuns }

func loadExecutionCapacity(ctx context.Context, tx *sql.Tx) (contracts.ExecutionCapacity, error) {
	return loadExecutionCapacityWithLimit(ctx, tx, 12)
}
func loadExecutionCapacityWithLimit(ctx context.Context, tx *sql.Tx, maxRuns int) (contracts.ExecutionCapacity, error) {
	out := contracts.ExecutionCapacity{MaxRuns: maxRuns}
	// Unknown external effects retain their slot until authoritative reconciliation.
	err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(CASE WHEN status IN ('dispatching','active') THEN 1 ELSE 0 END),0),coalesce(sum(CASE WHEN status='uncertain' THEN 1 ELSE 0 END),0) FROM runs`).Scan(&out.ActiveRuns, &out.UncertainRuns)
	if err != nil {
		return out, err
	}
	out.AvailableSlots = max(0, out.MaxRuns-out.ActiveRuns-out.UncertainRuns)
	enabled, err := DispatchEnabledTx(ctx, tx)
	if err != nil {
		return out, err
	}
	switch {
	case !enabled:
		out.ReasonCode = "instance_dispatch_fenced"
	case out.AvailableSlots == 0 && out.UncertainRuns > 0:
		out.ReasonCode = "execution_reconciliation_required"
	case out.AvailableSlots == 0:
		out.ReasonCode = "execution_capacity_full"
	}
	out.DispatchBlocked = out.ReasonCode != ""
	return out, nil
}
