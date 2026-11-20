package voice

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/logging"
)

// stewardSnapshot applies to the whole owner's task fact set, including absence.
// It fences new/clarify decisions as well as selected-task decisions.
func stewardSnapshot(ctx context.Context, q rosterQueryer, workspace, owner string) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT t.id,t.version,t.goal,t.status,t.result,t.interaction_state,t.interaction_run_id,t.interaction_action_digest,t.interaction_question,
 (SELECT json_group_array(json_array(e.id,e.kind,e.content,e.created_at)) FROM (SELECT * FROM task_entries ORDER BY created_at,id) e WHERE e.task_id=t.id AND e.workspace_id=t.workspace_id),
 (SELECT json_group_array(json_array(a.id,a.question,a.decision,a.authorization_used,a.expires_at)) FROM (SELECT * FROM pending_runtime_actions ORDER BY id) a WHERE a.task_id=t.id AND a.workspace_id=t.workspace_id)
 FROM tasks t WHERE t.workspace_id=? AND t.owner_subject_id=? ORDER BY t.id`, workspace, owner)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hash := sha256.New()
	for rows.Next() {
		values := make([]any, 11)
		dest := make([]any, 11)
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return "", err
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return "", err
		}
		hash.Write(encoded)
	}
	return hex.EncodeToString(hash.Sum(nil)), rows.Err()
}

func (s *Service) validateStewardSnapshotTx(ctx context.Context, tx *sql.Tx, p contracts.Principal, source handoffPayload) error {
	var accepted, id string
	if err := tx.QueryRowContext(ctx, `SELECT steward_snapshot,id FROM voice_turns WHERE workspace_id=? AND session_id=? AND generation=? AND turn_id=? AND revision=?`, p.WorkspaceID, source.SessionID, source.Generation, source.TurnID, source.Revision).Scan(&accepted, &id); err != nil {
		return err
	}
	current, err := stewardSnapshot(ctx, tx, p.WorkspaceID, p.SubjectID)
	if err != nil {
		return err
	}
	if accepted == "" || current != accepted {
		branch := "snapshot_mismatch"
		if accepted == "" {
			branch = "snapshot_empty"
		}
		logging.Infof("selection_stale branch=%s turn=%s accepted_empty=%t equal=%t", branch, id, accepted == "", current == accepted)
		return apiError(409, "selection_stale", "task facts changed; retry handoff to refresh Steward selection", nil)
	}
	return nil
}
