package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// StageWrite durably queues an explicit request for the next scheduled batch.
// For a repeated owner-scoped key, the first text and timestamp are retained,
// including after processing. Accepted text is stored without normalization.
func (r *Repository) StageWrite(ctx context.Context, workspace, principal, key, text string, at time.Time) error {
	if strings.TrimSpace(workspace) == "" || strings.TrimSpace(principal) == "" || strings.TrimSpace(key) == "" || strings.TrimSpace(text) == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > 4000 {
		return errors.New("invalid staged memory scope, key or text")
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO memory_staged_writes(workspace_id,principal_id,idempotency_key,original_text,staged_at,priority) VALUES(?,?,?,?,?,100) ON CONFLICT(workspace_id,principal_id,idempotency_key) DO NOTHING`, workspace, principal, key, text, at.UnixNano())
	return err
}

// BoundaryAt returns this owner's last successful batch boundary, or zero time
// when no successful batch has advanced it. It never creates a boundary.
func (r *Repository) BoundaryAt(ctx context.Context, workspace, principal string) (time.Time, error) {
	var ns int64
	err := r.db.QueryRowContext(ctx, `SELECT boundary_ns FROM memory_batch_boundaries WHERE workspace_id=? AND principal_id=?`, workspace, principal).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && ns == 0) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, ns).UTC(), nil
}

type batchStagedWrite struct {
	Key      string    `json:"-"`
	Text     string    `json:"text"`
	StagedAt time.Time `json:"staged_at"`
	Priority int       `json:"priority"`
}

func (r *Repository) pendingStagedWrites(ctx context.Context, workspace, principal string, until time.Time) ([]batchStagedWrite, error) {
	items := []batchStagedWrite{}
	rows, err := r.db.QueryContext(ctx, `SELECT idempotency_key,original_text,staged_at,priority FROM memory_staged_writes WHERE workspace_id=? AND principal_id=? AND status='pending' AND staged_at<=? ORDER BY priority DESC,staged_at,idempotency_key`, workspace, principal, until.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item batchStagedWrite
		var ns int64
		if err := rows.Scan(&item.Key, &item.Text, &ns, &item.Priority); err != nil {
			return nil, err
		}
		item.StagedAt = time.Unix(0, ns).UTC()
		items = append(items, item)
	}
	return items, rows.Err()
}
