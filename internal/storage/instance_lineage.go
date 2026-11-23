package storage

import (
	"context"
	"database/sql"
	"errors"
)

// InstanceLineage returns the immutable namespace for effects outside SQLite.
// Unlike instance identity and epoch, lineage survives a fenced restore.
func InstanceLineage(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	var lineage string
	if err := q.QueryRowContext(ctx, `SELECT lineage_id FROM zoe_instance_state WHERE singleton=1`).Scan(&lineage); err != nil {
		return "", err
	}
	if lineage == "" {
		return "", errors.New("instance lineage is missing")
	}
	return lineage, nil
}
