package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

// orderedRunEntries uses the same instant + id ordering as task entry pagination.
func orderedRunEntries(ctx context.Context, tx *sql.Tx, runID string, undelivered bool) ([]contracts.RunEntry, error) {
	query := `SELECT e.id,e.content,e.created_at FROM task_entries e JOIN runs r ON r.task_id=e.task_id AND r.workspace_id=e.workspace_id WHERE r.id=? AND e.kind='user_message'`
	if undelivered {
		query += ` AND NOT EXISTS (SELECT 1 FROM run_entry_deliveries d WHERE d.run_id=r.id AND d.entry_id=e.id)`
	}
	rows, err := tx.QueryContext(ctx, query, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []contracts.RunEntry
	for rows.Next() {
		var e contracts.RunEntry
		var stamp string
		if err := rows.Scan(&e.ID, &e.Content, &stamp); err != nil {
			return nil, err
		}
		e.CreatedAt, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})
	return entries, nil
}

func markRunEntries(ctx context.Context, tx *sql.Tx, runID, kind string, entries []contracts.RunEntry) error {
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO run_entry_deliveries(run_id,entry_id,delivered_at,prompt_kind) VALUES(?,?,?,?)`, runID, e.ID, time.Now().UTC().Format(time.RFC3339Nano), kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleEntryDelivery(w http.ResponseWriter, r *http.Request) {
	p, ok := RunnerPrincipalFromContext(r.Context())
	if !ok || !runnerHasAnyWorkspaceScope(p, "runner:control:read") {
		writeError(w, apiError(403, "forbidden", "runner lacks control scope", nil))
		return
	}
	var in contracts.EntryDelivery
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, apiError(400, "invalid_request", "invalid delivery", nil))
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback()
	var workspace string
	err = tx.QueryRowContext(r.Context(), `SELECT workspace_id FROM runs WHERE id=? AND runner_id=? AND attempt_id=? AND fence=? AND status='active' AND desired_control='run'`, r.PathValue("run_id"), p.RunnerID, in.AttemptID, in.Fence).Scan(&workspace)
	if err != nil || !HasRunnerScope(p, "runner:control:read", workspace) {
		writeError(w, apiError(409, "fence_conflict", "delivery does not match active run", nil))
		return
	}
	entries, err := orderedRunEntries(r.Context(), tx, r.PathValue("run_id"), false)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, e := range entries {
		if e.ID == in.EntryID {
			if err = markRunEntries(r.Context(), tx, r.PathValue("run_id"), "append", []contracts.RunEntry{e}); err == nil {
				err = tx.Commit()
			}
			if err != nil {
				writeError(w, err)
				return
			}
			w.WriteHeader(204)
			return
		}
	}
	writeError(w, apiError(404, "not_found", "entry not found", nil))
}
