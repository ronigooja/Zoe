package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
)

const maxTranscriptTurnBytes = 64 << 10
const maxTranscriptBatchBytes = 512 << 10

// TranscriptTurn is a verbatim voice turn. Seq is assigned by the gateway per session.
type TranscriptTurn struct {
	Seq          int64           `json:"seq"`
	Role         string          `json:"role"`
	Text         string          `json:"text"`
	At           string          `json:"at"`
	ReceivedAt   string          `json:"received_at,omitempty"`
	DelegationID json.RawMessage `json:"delegation_id,omitempty"`
}

func (s *Service) handleTranscript(w http.ResponseWriter, r *http.Request) {
	p, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok {
		control.WriteError(w, apiError(http.StatusUnauthorized, "unauthenticated", "worker authentication required", nil))
		return
	}
	// Bound the entire wire body, including JSON overhead, before decoding. This
	// also prevents escaped strings and trailing whitespace bypassing the limit.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTranscriptBatchBytes))
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			control.WriteError(w, apiError(http.StatusRequestEntityTooLarge, "transcript_too_large", "transcript batch exceeds 512 KiB", nil))
		} else {
			control.WriteError(w, apiError(http.StatusBadRequest, "invalid_request", "cannot read transcript batch", nil))
		}
		return
	}
	var req struct {
		GatewaySessionID string           `json:"gateway_session_id"`
		MediaGeneration  int64            `json:"media_generation"`
		Turns            []TranscriptTurn `json:"turns"`
	}
	if err := json.Unmarshal(body, &req); err != nil || !utf8.Valid(body) || req.GatewaySessionID == "" || req.MediaGeneration < 1 || len(req.Turns) == 0 || len(req.Turns) > 200 {
		control.WriteError(w, apiError(http.StatusBadRequest, "invalid_request", "one to 200 transcript turns are required", nil))
		return
	}
	for i := range req.Turns {
		turn := &req.Turns[i]
		if len(turn.Text) > maxTranscriptTurnBytes {
			control.WriteError(w, apiError(http.StatusRequestEntityTooLarge, "transcript_too_large", "transcript turn exceeds 64 KiB", nil))
			return
		}
		at, err := time.Parse(time.RFC3339Nano, turn.At)
		if err != nil || turn.Seq < 0 || (turn.Role != "user" && turn.Role != "assistant") {
			control.WriteError(w, apiError(http.StatusBadRequest, "invalid_request", "nonnegative seq, user or assistant role, and RFC3339 at are required", nil))
			return
		}
		turn.At = at.UTC().Format(time.RFC3339Nano)
	}
	accepted, duplicates, err := s.storeTranscript(r.Context(), p, req.GatewaySessionID, req.MediaGeneration, req.Turns)
	if err != nil {
		control.WriteError(w, safeSessionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": accepted, "duplicates": duplicates})
}

func (s *Service) storeTranscript(ctx context.Context, principal contracts.RunnerPrincipal, gateway string, generation int64, turns []TranscriptTurn) (int, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var workspace, session, state, runner string
	var storedGeneration, zoeGeneration, bindings int64
	var ended sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT workspace_id,id,gateway_media_generation,generation,state,ended_at,lifecycle_runner_id,
 (SELECT count(*) FROM voice_sessions WHERE gateway_session_id=? AND gateway_media_generation=?)
 FROM voice_sessions WHERE gateway_session_id=? ORDER BY (gateway_media_generation=?) DESC,created_at DESC LIMIT 1`, gateway, generation, gateway, generation).Scan(&workspace, &session, &storedGeneration, &zoeGeneration, &state, &ended, &runner, &bindings)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, apiError(http.StatusNotFound, "not_found", "voice session not found", nil)
	}
	if err != nil {
		return 0, 0, err
	}
	if !control.HasRunnerScope(principal, "voice:turn:ingest", workspace) {
		return 0, 0, apiError(http.StatusForbidden, "forbidden", "voice:turn:ingest scope and session workspace authorization required", nil)
	}
	if generation != storedGeneration || bindings != 1 || state == "allocating" || state == "failed" {
		return 0, 0, apiError(http.StatusConflict, "voice_generation_stale", "gateway media generation does not match", nil)
	}
	if runner == "" || runner != principal.RunnerID {
		return 0, 0, apiError(http.StatusForbidden, "forbidden", "gateway runner does not own the session binding", nil)
	}
	now := s.now().UTC()
	if state == "ended" || ended.Valid {
		endedAt, parseErr := time.Parse(time.RFC3339Nano, ended.String)
		if parseErr != nil || now.After(endedAt.Add(10*time.Minute)) {
			return 0, 0, apiError(http.StatusConflict, "transcript_window_closed", "transcript completion window has closed", nil)
		}
	}
	accepted, duplicates := 0, 0
	received := now.Format(time.RFC3339Nano)
	for _, turn := range turns {
		var delegation string
		if turn.DelegationID != nil && (json.Unmarshal(turn.DelegationID, &delegation) != nil || delegation == "" || utf8.RuneCountInString(delegation) > 256 || turn.Role != "user" || strings.TrimSpace(turn.Text) == "") {
			return 0, 0, apiError(http.StatusBadRequest, "invalid_request", "delegation_id requires 1–256 code points and a nonblank user turn", nil)
		}
		var text, role string
		err := tx.QueryRowContext(ctx, `SELECT text,role FROM voice_transcript_turns WHERE session_id=? AND seq=?`, session, turn.Seq).Scan(&text, &role)
		if err == nil {
			if text != turn.Text {
				return 0, 0, apiError(http.StatusConflict, "transcript_conflict", "transcript sequence already has different text", nil)
			}
			if delegation != "" && role != "user" {
				return 0, 0, apiError(http.StatusBadRequest, "invalid_request", "delegation_id cannot mark an assistant turn", nil)
			}
			duplicates++
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, err
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO voice_transcript_turns(workspace_id,session_id,seq,role,text,at,received_at) VALUES(?,?,?,?,?,?,?)`, workspace, session, turn.Seq, turn.Role, turn.Text, turn.At, received); err != nil {
				return 0, 0, err
			}
			accepted++
		}
		if delegation != "" {
			if err := storeDelegationTx(ctx, tx, workspace, session, zoeGeneration, delegation, turn, received); err != nil {
				return 0, 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return accepted, duplicates, nil
}

func storeDelegationTx(ctx context.Context, tx *sql.Tx, workspace, session string, generation int64, delegation string, turn TranscriptTurn, received string) error {
	hash := sha256.Sum256([]byte(turn.Text))
	var existingHash []byte
	var existingText string
	err := tx.QueryRowContext(ctx, `SELECT content_hash,content FROM voice_turns WHERE session_id=? AND generation=? AND turn_id=? AND revision=1`, session, generation, delegation).Scan(&existingHash, &existingText)
	if err == nil {
		if !bytes.Equal(hash[:], existingHash) || existingText != turn.Text {
			return apiError(http.StatusConflict, "turn_revision_conflict", "delegation identity already has different text", nil)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id,transcript_seq) VALUES(?,?,?,?,?,1,?,?,?,?,?)`, uuid.NewString(), workspace, session, generation, delegation, turn.Text, hash[:], received, delegationKey(session, generation, delegation, 1), turn.Seq)
	return err
}

// ReadTranscriptTurns returns a workspace's session turns in sequence order,
// inclusive of firstSeq and lastSeq. Callers must supply their trusted workspace
// identity; no subject ownership restriction is applied to this internal helper.
func ReadTranscriptTurns(ctx context.Context, db *sql.DB, workspace, session string, firstSeq, lastSeq int64) ([]TranscriptTurn, error) {
	rows, err := db.QueryContext(ctx, `SELECT seq,role,text,at,received_at FROM voice_transcript_turns WHERE workspace_id=? AND session_id=? AND seq>=? AND seq<=? ORDER BY seq`, workspace, session, firstSeq, lastSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []TranscriptTurn{}
	for rows.Next() {
		var turn TranscriptTurn
		if err := rows.Scan(&turn.Seq, &turn.Role, &turn.Text, &turn.At, &turn.ReceivedAt); err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}
