package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

type DeletionHead struct {
	LineageID      string   `json:"lineage_id"`
	Generation     int64    `json:"generation"`
	Sequence       int64    `json:"sequence"`
	Replayed       int      `json:"replayed"`
	GateCleared    bool     `json:"gate_cleared"`
	BlockedTargets []string `json:"blocked_targets"`
}

// TrustedDeletionHead is copied from the authenticated backup manifest into
// the private restore-state record by zoe-restore.py. The snapshot fields bind
// replay to the exact restored authority image; head fields bind it to the
// complete retained external journal.
type TrustedDeletionHead struct {
	SnapshotLineageID       string `json:"snapshot_lineage_id"`
	SnapshotGeneration      int64  `json:"snapshot_generation"`
	SnapshotSequence        int64  `json:"snapshot_sequence"`
	SnapshotEventCount      int64  `json:"snapshot_event_count"`
	SnapshotEventSum        int64  `json:"snapshot_event_cursor_sum"`
	JournalLineageID        string `json:"journal_lineage_id"`
	JournalGeneration       int64  `json:"journal_generation"`
	JournalRecordGeneration int64  `json:"journal_record_generation"`
	JournalSequence         int64  `json:"journal_sequence"`
	JournalHeadSHA256       string `json:"journal_head_sha256"`
	JournalFileSHA256       string `json:"journal_file_sha256"`
}

func ReconcileDeletionJournalWithTrustedHead(ctx context.Context, store *Store, trusted TrustedDeletionHead) (DeletionHead, error) {
	if store == nil || trusted.SnapshotLineageID == "" || trusted.JournalLineageID == "" || trusted.JournalLineageID != trusted.SnapshotLineageID || (trusted.JournalSequence > 0 && trusted.JournalHeadSHA256 == "") || trusted.JournalFileSHA256 == "" || trusted.SnapshotGeneration < 1 || trusted.JournalGeneration < 1 || (trusted.JournalSequence == 0 && trusted.JournalRecordGeneration != 0) || (trusted.JournalSequence > 0 && (trusted.JournalRecordGeneration < 1 || trusted.JournalRecordGeneration > trusted.JournalGeneration)) || trusted.SnapshotSequence < 0 || trusted.JournalSequence < 0 || trusted.SnapshotEventCount < 0 || trusted.SnapshotEventSum < 0 || trusted.SnapshotSequence > trusted.JournalSequence {
		return DeletionHead{}, errors.New("protected restore state has an incomplete deletion baseline")
	}
	journal, err := NewDeletionJournal(store.Path())
	if err != nil {
		return DeletionHead{}, err
	}
	records, err := journal.ReadAll()
	if err != nil {
		return DeletionHead{}, err
	}
	if int64(len(records)) != trusted.JournalSequence {
		return DeletionHead{}, errors.New("retained journal does not match the authenticated external head")
	}
	if len(records) == 0 {
		if trusted.JournalLineageID != trusted.SnapshotLineageID || trusted.JournalRecordGeneration != 0 || trusted.JournalHeadSHA256 != "" {
			return DeletionHead{}, errors.New("empty deletion journal does not match its trusted head")
		}
	} else {
		last := records[len(records)-1]
		if last.LineageID != trusted.JournalLineageID || last.Generation != trusted.JournalRecordGeneration || last.Hash != trusted.JournalHeadSHA256 {
			return DeletionHead{}, errors.New("retained journal identity does not match the authenticated external head")
		}
	}
	file, err := os.Open(journalPath(store.Path()))
	if err != nil {
		return DeletionHead{}, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return DeletionHead{}, copyErr
	}
	if closeErr != nil {
		return DeletionHead{}, closeErr
	}
	if hex.EncodeToString(hash.Sum(nil)) != trusted.JournalFileSHA256 {
		return DeletionHead{}, errors.New("retained journal file hash does not match authenticated head")
	}
	return reconcileDeletionJournal(ctx, store, trusted, records)
}

func journalPath(databasePath string) string { return databasePath + ".deletion-ledger" }

// reconcileDeletionJournal validates fresh-schema restore state under the
// caller's exclusive Store lock. No entity replay adapters are registered.
func reconcileDeletionJournal(ctx context.Context, store *Store, trusted TrustedDeletionHead, records []DeletionRecord) (DeletionHead, error) {
	var out DeletionHead
	if store == nil {
		return out, errors.New("store is required")
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var currentLineage string
	var currentGeneration, currentSequence int64
	var gate int
	if err = tx.QueryRowContext(ctx, `SELECT lineage_id,generation,deletion_sequence,restore_deletion_reconciliation_required FROM zoe_instance_state WHERE singleton=1`).Scan(&currentLineage, &currentGeneration, &currentSequence, &gate); err != nil {
		return out, err
	}
	if currentLineage != trusted.SnapshotLineageID || int64(len(records)) != trusted.JournalSequence {
		return out, errors.New("authority lineage or retained journal does not match authenticated restore state")
	}
	var appliedSequence, activatedGeneration int64
	var stored TrustedDeletionHead
	progressErr := tx.QueryRowContext(ctx, `SELECT snapshot_lineage_id,snapshot_generation,snapshot_sequence,snapshot_event_count,snapshot_event_cursor_sum,journal_lineage_id,journal_generation,journal_record_generation,journal_sequence,journal_head_sha256,journal_file_sha256,applied_sequence,activated_generation FROM zoe_restore_deletion_progress WHERE singleton=1`).Scan(
		&stored.SnapshotLineageID, &stored.SnapshotGeneration, &stored.SnapshotSequence, &stored.SnapshotEventCount, &stored.SnapshotEventSum,
		&stored.JournalLineageID, &stored.JournalGeneration, &stored.JournalRecordGeneration, &stored.JournalSequence, &stored.JournalHeadSHA256, &stored.JournalFileSHA256, &appliedSequence, &activatedGeneration)
	if errors.Is(progressErr, sql.ErrNoRows) {
		var eventCount, eventSum int64
		if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(cursor),0) FROM events`).Scan(&eventCount, &eventSum); err != nil {
			return out, err
		}
		if currentGeneration != trusted.SnapshotGeneration || currentSequence != trusted.SnapshotSequence || eventCount != trusted.SnapshotEventCount || eventSum != trusted.SnapshotEventSum {
			return out, errors.New("restored authority does not match the authenticated snapshot baseline")
		}
		stored = trusted
		appliedSequence = currentSequence
		if _, err = tx.ExecContext(ctx, `INSERT INTO zoe_restore_deletion_progress(singleton,snapshot_lineage_id,snapshot_generation,snapshot_sequence,snapshot_event_count,snapshot_event_cursor_sum,journal_lineage_id,journal_generation,journal_record_generation,journal_sequence,journal_head_sha256,journal_file_sha256,applied_sequence,activated_generation) VALUES(1,?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			trusted.SnapshotLineageID, trusted.SnapshotGeneration, trusted.SnapshotSequence, trusted.SnapshotEventCount, trusted.SnapshotEventSum,
			trusted.JournalLineageID, trusted.JournalGeneration, trusted.JournalRecordGeneration, trusted.JournalSequence, trusted.JournalHeadSHA256, trusted.JournalFileSHA256, appliedSequence); err != nil {
			return out, err
		}
	} else if progressErr != nil {
		return out, progressErr
	} else if stored != trusted {
		return out, errors.New("authenticated restore baseline or journal head changed during reconciliation")
	}
	if currentSequence != appliedSequence || appliedSequence < trusted.SnapshotSequence || appliedSequence > trusted.JournalSequence {
		return out, errors.New("authority sequence does not match durable deletion replay progress")
	}
	allowedGeneration := max(trusted.SnapshotGeneration, trusted.JournalGeneration)
	if currentGeneration != trusted.SnapshotGeneration && (activatedGeneration == 0 || currentGeneration != activatedGeneration) {
		return out, errors.New("authority generation does not match authenticated restore progress")
	}
	// No generic entity replay adapter is registered. Never advance over a
	// retained deletion that this fresh-schema release cannot apply.
	if trusted.JournalSequence != 0 || trusted.SnapshotSequence != 0 {
		return out, errors.New("deletion replay is unsupported for this schema")
	}
	if currentSequence != trusted.JournalSequence {
		return out, errors.New("replay did not reach trusted deletion head")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE zoe_restore_deletion_progress SET applied_sequence=? WHERE singleton=1`, currentSequence); err != nil {
		return out, err
	}
	if trusted.JournalSequence == 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE zoe_deletion_reconciliation_checkpoints SET completed_sequence=0,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),last_error=''`); err != nil {
			return out, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE zoe_deletion_reconciliation_checkpoints SET completed_sequence=?,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),last_error='' WHERE target='source_ledger'`, currentSequence); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT target,completed_sequence,last_error FROM zoe_deletion_reconciliation_checkpoints ORDER BY target`)
	if err != nil {
		return out, err
	}
	allComplete := true
	for rows.Next() {
		var target, lastError string
		var completed int64
		if err = rows.Scan(&target, &completed, &lastError); err != nil {
			_ = rows.Close()
			return out, err
		}
		if completed < trusted.JournalSequence || lastError != "" {
			allComplete = false
			if lastError == "" {
				lastError = "adapter_missing"
			}
			out.BlockedTargets = append(out.BlockedTargets, target+":"+lastError)
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return out, err
	}
	if err = rows.Close(); err != nil {
		return out, err
	}
	if gate == 1 && allComplete {
		newGeneration := allowedGeneration + 1
		if _, err = tx.ExecContext(ctx, `UPDATE zoe_instance_state SET generation=?,restore_deletion_reconciliation_required=0,derived_retrieval_enabled=1,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE singleton=1`, newGeneration); err != nil {
			return out, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE zoe_restore_deletion_progress SET activated_generation=? WHERE singleton=1`, newGeneration); err != nil {
			return out, err
		}
		var stageGuard int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='zoe_restore_stage_guard'`).Scan(&stageGuard); err != nil {
			return out, err
		}
		if stageGuard > 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE zoe_restore_stage_guard SET pending=0 WHERE singleton=1`); err != nil {
				return out, err
			}
		}
		out.GateCleared = true
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	if gate == 1 && allComplete {
		currentGeneration = allowedGeneration + 1
	}
	out.LineageID, out.Generation, out.Sequence = currentLineage, currentGeneration, currentSequence
	return out, nil
}
