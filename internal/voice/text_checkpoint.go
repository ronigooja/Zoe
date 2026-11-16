package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// TextSubmissionRejection applies only when an adapter can prove no external write occurred.
// Ordinary transport errors do not implement this contract and remain lookup-only.
type TextSubmissionRejection interface {
	error
	RejectedBeforeWrite() bool
}

// textClaimFence applies to every chunk attempt and receipt mutation. A single
// UPDATE is the transaction/CAS boundary; no database transaction spans HTTP.
const textClaimFence = ` EXISTS(SELECT 1 FROM voice_text_checkpoints c JOIN voice_sessions v ON v.id=c.session_id AND v.workspace_id=c.workspace_id AND v.owner_id=c.owner_id AND v.generation=c.generation AND v.gateway_session_id=c.gateway_session_id AND v.gateway_media_generation=c.media_generation AND v.state='active' WHERE c.workspace_id=? AND c.owner_id=? AND c.source_kind=? AND c.source_id=? AND c.claim=? AND c.lease_until>?) `

const TextCheckpointSchema = `
CREATE TABLE voice_text_checkpoints (
 workspace_id TEXT NOT NULL,owner_id TEXT NOT NULL,source_kind TEXT NOT NULL,source_id TEXT NOT NULL,
 content_hash TEXT NOT NULL,session_id TEXT NOT NULL,generation INTEGER NOT NULL,gateway_session_id TEXT NOT NULL,media_generation INTEGER NOT NULL,
 claim INTEGER NOT NULL DEFAULT 0,lease_until TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(workspace_id,owner_id,source_kind,source_id));
CREATE TABLE voice_text_chunks (
 workspace_id TEXT NOT NULL,owner_id TEXT NOT NULL,source_kind TEXT NOT NULL,source_id TEXT NOT NULL,ordinal INTEGER NOT NULL,
 delivery_id TEXT NOT NULL UNIQUE,content_hash TEXT NOT NULL,text TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('pending','attempted','confirmed','unknown')),
 PRIMARY KEY(workspace_id,owner_id,source_kind,source_id,ordinal),
 FOREIGN KEY(workspace_id,owner_id,source_kind,source_id) REFERENCES voice_text_checkpoints(workspace_id,owner_id,source_kind,source_id));`

// deliverDurableText applies to persisted non-task speech sources. No transaction
// is held during HTTP. Attempted chunks are reconciled, never blindly resubmitted.
func (s *Service) deliverDurableText(ctx context.Context, workspace, owner, kind, id, session string, generation int64, text string) (bool, string) {
	if s.resultReceiptAdapter == nil {
		return false, "receipt_capability_disabled"
	}
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return false, "invalid_text"
	}
	var gateway string
	var media int64
	if err := s.db.QueryRowContext(ctx, `SELECT gateway_session_id,gateway_media_generation FROM voice_sessions WHERE id=? AND workspace_id=? AND owner_id=? AND generation=? AND state='active'`, session, workspace, owner, generation).Scan(&gateway, &media); err != nil || gateway == "" || media < 1 {
		return false, "awaiting_context"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "checkpoint_storage"
	}
	defer tx.Rollback()
	args := []any{workspace, owner, kind, id}
	hash := resultHash(text)
	now := s.now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_text_checkpoints(workspace_id,owner_id,source_kind,source_id,content_hash,session_id,generation,gateway_session_id,media_generation) VALUES(?,?,?,?,?,?,?,?,?)`, workspace, owner, kind, id, hash, session, generation, gateway, media); err != nil {
		return false, "checkpoint_storage"
	}
	res, err := tx.ExecContext(ctx, `UPDATE voice_text_checkpoints SET claim=claim+1,lease_until=? WHERE workspace_id=? AND owner_id=? AND source_kind=? AND source_id=? AND content_hash=? AND session_id=? AND generation=? AND gateway_session_id=? AND media_generation=? AND lease_until<=?`, now.Add(time.Minute).Format(time.RFC3339Nano), workspace, owner, kind, id, hash, session, generation, gateway, media, stamp)
	if err != nil {
		return false, "checkpoint_storage"
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, "checkpoint_identity_or_busy"
	}
	var claim int64
	if err := tx.QueryRowContext(ctx, `SELECT claim FROM voice_text_checkpoints WHERE workspace_id=? AND owner_id=? AND source_kind=? AND source_id=?`, args...).Scan(&claim); err != nil {
		return false, "checkpoint_storage"
	}
	for ordinal, remaining := 0, text; len(remaining) > 0; ordinal++ {
		n := min(8000, len(remaining))
		for n < len(remaining) && !utf8.RuneStart(remaining[n]) {
			n--
		}
		chunk := remaining[:n]
		remaining = remaining[n:]
		identity, _ := json.Marshal([]any{workspace, owner, kind, id, hash, session, generation, gateway, media, ordinal, resultHash(chunk)})
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO voice_text_chunks VALUES(?,?,?,?,?,?,?,?,'pending')`, workspace, owner, kind, id, ordinal, resultHash(string(identity)), resultHash(chunk), chunk); err != nil {
			return false, "checkpoint_storage"
		}
	}
	if err := tx.Commit(); err != nil {
		return false, "checkpoint_storage"
	}
	defer func() {
		_, _ = s.db.ExecContext(context.Background(), `UPDATE voice_text_checkpoints SET lease_until='' WHERE workspace_id=? AND owner_id=? AND source_kind=? AND source_id=? AND claim=? AND lease_until>?`, workspace, owner, kind, id, claim, s.now().UTC().Format(time.RFC3339Nano))
	}()
	for ordinal := 0; ; ordinal++ {
		fenceArgs := []any{workspace, owner, kind, id, claim, s.now().UTC().Format(time.RFC3339Nano)}
		var valid bool
		if err := s.db.QueryRowContext(ctx, `SELECT `+textClaimFence, fenceArgs...).Scan(&valid); err != nil || !valid {
			return false, "checkpoint_lease_lost"
		}
		var request ResultChunkRequest
		request.MediaGeneration = media
		var state string
		err := s.db.QueryRowContext(ctx, `SELECT delivery_id,content_hash,text,state FROM voice_text_chunks WHERE workspace_id=? AND owner_id=? AND source_kind=? AND source_id=? AND ordinal=?`, workspace, owner, kind, id, ordinal).Scan(&request.DeliveryID, &request.ContentHash, &request.Text, &state)
		if err == sql.ErrNoRows {
			return true, "gateway_accepted"
		}
		if err != nil {
			return false, "checkpoint_storage"
		}
		if state == "confirmed" {
			continue
		}
		var receipt ResultChunkReceipt
		// Persist a fenced attempt before Submit, including a checked row count.
		// Existing attempted/unknown chunks stay lookup-only, even after takeover.
		attemptArgs := append([]any{request.DeliveryID, state}, workspace, owner, kind, id, claim, s.now().UTC().Format(time.RFC3339Nano))
		res, err := s.db.ExecContext(ctx, `UPDATE voice_text_chunks SET state='attempted' WHERE delivery_id=? AND state=? AND `+textClaimFence, attemptArgs...)
		if err != nil {
			return false, "checkpoint_storage"
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return false, "checkpoint_lease_lost"
		}
		if state == "pending" {
			receipt, err = s.resultReceiptAdapter.Submit(ctx, gateway, request)
		} else {
			receipt, err = s.resultReceiptAdapter.Lookup(ctx, gateway, request)
		}
		next, code := "confirmed", "gateway_accepted"
		if err != nil {
			next, code = "unknown", "receipt_transport_unknown"
			if state != "pending" {
				code = "receipt_lookup_unavailable"
			} else {
				var rejected TextSubmissionRejection
				if errors.As(err, &rejected) && rejected.RejectedBeforeWrite() {
					// The shared schema has no rejected state. Preserve the no-replay
					// boundary until a rejected state is installed by its owner.
					code = "receipt_rejected"
				}
			}
		} else if !receiptApplied(request, receipt) {
			next, code = "unknown", "receipt_invalid"
		}
		updateArgs := append([]any{next, request.DeliveryID}, workspace, owner, kind, id, claim, s.now().UTC().Format(time.RFC3339Nano))
		res, err = s.db.ExecContext(ctx, `UPDATE voice_text_chunks SET state=? WHERE delivery_id=? AND state='attempted' AND `+textClaimFence, updateArgs...)
		if err != nil {
			return false, "checkpoint_storage"
		}
		n, err = res.RowsAffected()
		if err != nil || n != 1 {
			return false, "checkpoint_lease_lost"
		}
		if next != "confirmed" {
			return false, code
		}
	}
}
