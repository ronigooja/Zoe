package control

import (
	"context"
	"database/sql"
	"io"

	"github.com/ronigooja/Zoe/contracts"
)

// Voice transcripts follow the session owner's visibility in either export
// scope. Optional feature tables may be absent on instances without voice.
func writeVoiceTranscriptExport(ctx context.Context, tx *sql.Tx, out io.Writer, p contracts.Principal, counts *exportCounter) error {
	for _, item := range []struct {
		table string
		query string
		count *int64
	}{
		{"voice_transcript_turns", `SELECT json_object('workspace_id',v.workspace_id,'session_id',v.session_id,'seq',v.seq,'role',v.role,'text',v.text,'at',v.at,'received_at',v.received_at) FROM voice_transcript_turns v JOIN voice_sessions s ON s.id=v.session_id AND s.workspace_id=v.workspace_id WHERE v.workspace_id=? AND s.owner_id=? ORDER BY v.session_id,v.seq`, &counts.VoiceTranscripts},
	} {
		if _, err := io.WriteString(out, `,"`+item.table+`":`); err != nil {
			return err
		}
		exists, err := tableExistsTx(ctx, tx, item.table)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := io.WriteString(out, `[]`); err != nil {
				return err
			}
			continue
		}
		if err := writeRows(ctx, tx, out, item.count, item.query, []any{p.WorkspaceID, p.SubjectID}); err != nil {
			return err
		}
	}
	return nil
}

func validateVoiceTranscriptExportBounds(ctx context.Context, tx *sql.Tx, p contracts.Principal) error {
	for _, item := range []struct{ table, field string }{
		{"voice_transcript_turns", "length(CAST(v.text AS BLOB))"},
	} {
		exists, err := tableExistsTx(ctx, tx, item.table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		var size int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(`+item.field+`),0) FROM `+item.table+` v JOIN voice_sessions s ON s.id=v.session_id AND s.workspace_id=v.workspace_id WHERE v.workspace_id=? AND s.owner_id=?`, p.WorkspaceID, p.SubjectID).Scan(&size); err != nil {
			return err
		}
		if size > maxScopedExportFieldBytes {
			return errExportFieldTooLarge
		}
	}
	return nil
}
