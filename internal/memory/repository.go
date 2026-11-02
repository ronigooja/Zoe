package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

var (
	ErrForbidden       = errors.New("memory access forbidden")
	ErrNotFound        = errors.New("memory record not found")
	ErrVersionConflict = errors.New("memory revision conflict")
	ErrInvalidSource   = errors.New("memory source is unavailable or inaccessible")
	ErrContextStale    = errors.New("memory context changed during assembly")
)

type Repository struct {
	db              *sql.DB
	deletionJournal *storage.DeletionJournal
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

func NewRepositoryWithStore(store *storage.Store) *Repository {
	if store == nil {
		return &Repository{}
	}
	journal, _ := storage.NewDeletionJournal(store.Path())
	return &Repository{db: store.DB(), deletionJournal: journal}
}

func (r *Repository) replayPendingDeletionsTx(ctx context.Context, tx *sql.Tx) error {
	if r == nil || r.deletionJournal == nil {
		return nil
	}
	var lineage string
	var generation, sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT lineage_id,generation,deletion_sequence FROM zoe_instance_state WHERE singleton=1`).Scan(&lineage, &generation, &sequence); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return err
	}
	return replayPendingDeletionJournalTx(ctx, tx, r.deletionJournal, lineage, generation, &sequence)
}

func journalledDeletionVersionsTx(ctx context.Context, tx *sql.Tx, journal *storage.DeletionJournal) (map[string]int64, error) {
	versions := map[string]int64{}
	if journal == nil {
		return versions, nil
	}
	var lineage string
	if err := tx.QueryRowContext(ctx, `SELECT lineage_id FROM zoe_instance_state WHERE singleton=1`).Scan(&lineage); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return versions, nil
		}
		return nil, err
	}
	records, err := journal.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read independent deletion journal: %w", err)
	}
	for _, record := range records {
		if record.LineageID != lineage || record.EntityKind != "memory_claim" {
			continue
		}
		key := record.WorkspaceID + "\x00" + record.EntityID
		if record.EntityVersion > versions[key] {
			versions[key] = record.EntityVersion
		}
	}
	return versions, nil
}

func sourceHasJournalledDeletionTx(ctx context.Context, tx *sql.Tx, journal *storage.DeletionJournal, workspace, sourceID string, sourceVersion int64) (bool, error) {
	versions, err := journalledDeletionVersionsTx(ctx, tx, journal)
	if err != nil || len(versions) == 0 {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT claim_id FROM memory_revision_sources WHERE workspace_id=? AND source_id=? AND source_version=?`, workspace, sourceID, sourceVersion)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var claimID string
		if err := rows.Scan(&claimID); err != nil {
			return false, err
		}
		if versions[workspace+"\x00"+claimID] > 0 {
			return true, nil
		}
	}
	return false, rows.Err()
}

type WriteInput struct {
	WorkspaceID       string
	ActorID           string
	Claim             Claim
	Source            Source
	AdditionalSources []Source
	Expected          int64
}

type DeleteInput struct {
	WorkspaceID string
	ActorID     string
	ClaimID     string
	Expected    int64
	ReasonCode  string
}

type SearchFilter struct {
	ScopeKind ScopeKind
	ScopeID   string
	TaskID    string
	ProjectID string
	Limit     int
	At        *time.Time
}

type SearchResult struct {
	Items     []Claim  `json:"items"`
	Missing   []string `json:"missing"`
	Conflicts []string `json:"conflicts"`
}

func (r *Repository) PutTx(ctx context.Context, tx *sql.Tx, in WriteInput) (Claim, error) {
	c := in.Claim
	if c.WorkspaceID != in.WorkspaceID || c.Authority != AuthorityUserConfirmed {
		return Claim{}, errors.New("user memory writes must be workspace-bound and user-confirmed")
	}
	if in.ActorID == "" || in.Source.Kind == "" || in.Source.Reference == "" {
		return Claim{}, errors.New("actor and validated source are required")
	}
	sources := append([]Source{in.Source}, in.AdditionalSources...)
	for _, source := range sources {
		if !sourceContains(source.Scope, c.Scope) {
			return Claim{}, errors.New("claim scope cannot be broader than its source")
		}
	}
	if c.ID != "" {
		return Claim{}, errors.New("memory ID is assigned by the authority")
	}
	c.ID = uuid.NewString()
	c.Revision = 1
	if err := c.ValidateForWrite(); err != nil {
		return Claim{}, err
	}
	for _, source := range sources {
		if err := insertSourceTx(ctx, tx, in.WorkspaceID, source); err != nil {
			return Claim{}, err
		}
	}

	// A different value for the same key is preserved as a conflict. It is
	// never resolved by last-writer-wins or by retrieval similarity.
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.revision,r.content,c.status FROM memory_claims c JOIN memory_revisions r ON r.claim_id=c.id AND r.revision=c.revision
WHERE c.workspace_id=? AND c.scope_kind=? AND c.scope_id=? AND c.principal_id=? AND c.subject=? AND c.claim_key=? AND c.status IN ('active','conflicted') AND c.deleted_at IS NULL`,
		in.WorkspaceID, c.Scope.Kind, c.Scope.ID, c.Scope.PrincipalID, c.Subject, c.Key)
	if err != nil {
		return Claim{}, err
	}
	type existingClaim struct {
		id      string
		rev     int64
		content string
		status  ClaimStatus
	}
	var existing []existingClaim
	for rows.Next() {
		var v existingClaim
		if err := rows.Scan(&v.id, &v.rev, &v.content, &v.status); err != nil {
			_ = rows.Close()
			return Claim{}, err
		}
		existing = append(existing, v)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Claim{}, err
	}
	_ = rows.Close()
	if in.Expected > 0 {
		if len(existing) != 1 || existing[0].rev != in.Expected {
			return Claim{}, ErrVersionConflict
		}
		old := existing[0]
		c.ID, c.Revision, c.Status = old.id, old.rev+1, ClaimActive
		if err := insertRevisionTx(ctx, tx, in.WorkspaceID, c, in.ActorID, "", sources); err != nil {
			return Claim{}, err
		}
		if err := copyRevisionSourcesAndAdd(ctx, tx, in.WorkspaceID, old.id, old.rev, c.Revision, sources); err != nil {
			return Claim{}, err
		}
		return c, nil
	}
	// An unresolved conflict is a barrier. A later unversioned observation
	// must not silently become active merely because it is the third value.
	for _, old := range existing {
		if old.status == ClaimConflicted {
			return Claim{}, ErrVersionConflict
		}
	}
	conflicting := false
	for _, old := range existing {
		if normalizeText(old.content) == normalizeText(c.Content) {
			continue
		}
		conflicting = true
	}
	if conflicting {
		for _, old := range existing {
			if err := setStatusRevisionTx(ctx, tx, in.WorkspaceID, old.id, old.rev, ClaimConflicted, in.ActorID); err != nil {
				return Claim{}, err
			}
		}
	}
	if len(existing) == 1 && !conflicting && normalizeText(existing[0].content) == normalizeText(c.Content) {
		old := existing[0]
		c.ID = old.id
		c.Revision = old.rev + 1
		c.Status = ClaimActive
		if err := insertRevisionTx(ctx, tx, in.WorkspaceID, c, in.ActorID, "", sources); err != nil {
			return Claim{}, err
		}
		if err := copyRevisionSourcesAndAdd(ctx, tx, in.WorkspaceID, old.id, old.rev, c.Revision, sources); err != nil {
			return Claim{}, err
		}
		return c, nil
	}
	c.Revision = 1
	if conflicting {
		c.Status = ClaimConflicted
	} else {
		c.Status = ClaimActive
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_claims(id,workspace_id,scope_kind,scope_id,principal_id,subject,claim_key,authority_kind,status,revision,valid_from,valid_to,supersedes_id,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.ID, c.WorkspaceID, c.Scope.Kind, c.Scope.ID, c.Scope.PrincipalID, c.Subject, c.Key, c.Authority, c.Status, c.Revision, timeValue(c.ValidFrom), timeValue(c.ValidTo), nullString(c.Supersedes), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return Claim{}, err
	}
	if err := insertRevisionTx(ctx, tx, in.WorkspaceID, c, in.ActorID, "", sources); err != nil {
		return Claim{}, err
	}
	return c, nil
}

func (r *Repository) DeleteTx(ctx context.Context, tx *sql.Tx, in DeleteInput) (Claim, error) {
	if in.Expected < 1 || in.ActorID == "" || in.ReasonCode != "user_requested" {
		return Claim{}, errors.New("delete requires expected revision, actor, and user_requested reason")
	}
	var lineageID string
	var generation, currentSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT lineage_id,generation,deletion_sequence FROM zoe_instance_state WHERE singleton=1`).Scan(&lineageID, &generation, &currentSequence); err != nil {
		if !strings.Contains(err.Error(), "no such table") {
			return Claim{}, fmt.Errorf("read deletion instance lineage: %w", err)
		}
		lineageID, generation, currentSequence = "legacy-memory-test", 1, 0
	}
	// A sidecar append intentionally precedes the SQLite tombstone. If the
	// authority transaction later rolls back, replay that durable intent before
	// allowing another delete to allocate a sequence.
	if r.deletionJournal != nil && lineageID != "legacy-memory-test" {
		if err := replayPendingDeletionJournalTx(ctx, tx, r.deletionJournal, lineageID, generation, &currentSequence); err != nil {
			return Claim{}, err
		}
	}
	var c Claim
	var validFrom, validTo sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id,workspace_id,scope_kind,scope_id,principal_id,subject,claim_key,authority_kind,status,revision,valid_from,valid_to FROM memory_claims WHERE workspace_id=? AND id=? AND deleted_at IS NULL`, in.WorkspaceID, in.ClaimID).Scan(
		&c.ID, &c.WorkspaceID, &c.Scope.Kind, &c.Scope.ID, &c.Scope.PrincipalID, &c.Subject, &c.Key, &c.Authority, &c.Status, &c.Revision, &validFrom, &validTo)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, ErrNotFound
	}
	if err != nil {
		return Claim{}, err
	}
	if c.Revision != in.Expected {
		return Claim{}, ErrVersionConflict
	}
	deletedAt := time.Now().UTC()
	cleanupTargets := []string{"claim", "source_links", "proposals", "mem0", "qdrant", "cache", "export_temp", "backup_delete_manifest", "source_ledger"}
	globalSequence := currentSequence + 1
	if r.deletionJournal != nil {
		if _, err := r.deletionJournal.Append(storage.DeletionRecord{Sequence: globalSequence, LineageID: lineageID, Generation: generation, WorkspaceID: in.WorkspaceID, EntityKind: "memory_claim", EntityID: c.ID, EntityVersion: c.Revision + 1, ActorID: in.ActorID, ReasonCode: in.ReasonCode, DeletedAt: deletedAt.Format(time.RFC3339Nano), CleanupTargets: cleanupTargets}); err != nil {
			return Claim{}, fmt.Errorf("durable deletion journal append failed: %w", err)
		}
	}
	if err := clearMemoryClaimLocalArtifactsTx(ctx, tx, in.WorkspaceID, c.ID, deletedAt.Format(time.RFC3339Nano)); err != nil {
		return Claim{}, err
	}
	if err := invalidateExportTempsTx(ctx, tx, in.WorkspaceID, globalSequence, deletedAt.Format(time.RFC3339Nano)); err != nil {
		return Claim{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_revision_sources WHERE claim_id=?`, c.ID); err != nil {
		return Claim{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_terms WHERE claim_id=?`, c.ID); err != nil {
		return Claim{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_revisions WHERE claim_id=?`, c.ID); err != nil {
		return Claim{}, err
	}
	c.Revision++
	c.Status = ClaimTombstoned
	c.DeletedAt = &deletedAt
	if _, err := tx.ExecContext(ctx, `UPDATE memory_claims SET status='tombstoned',revision=?,updated_at=?,deleted_at=? WHERE workspace_id=? AND id=? AND revision=?`, c.Revision, deletedAt.Format(time.RFC3339Nano), deletedAt.Format(time.RFC3339Nano), in.WorkspaceID, c.ID, in.Expected); err != nil {
		return Claim{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_revisions(claim_id,revision,content,authority_kind,status,recorded_at,actor_id) VALUES(?,?, '', ?, 'tombstoned', ?, ?)`, c.ID, c.Revision, c.Authority, deletedAt.Format(time.RFC3339Nano), in.ActorID); err != nil {
		return Claim{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_tombstones(claim_id,workspace_id,revision,actor_id,reason,deleted_at) VALUES(?,?,?,?,?,?)`, c.ID, in.WorkspaceID, c.Revision, in.ActorID, in.ReasonCode, deletedAt.Format(time.RFC3339Nano)); err != nil {
		return Claim{}, err
	}
	var ledgerGeneration int
	if err := tx.QueryRowContext(ctx, `SELECT ledger_generation FROM memory_deletion_ledger_state WHERE singleton=1`).Scan(&ledgerGeneration); err != nil {
		return Claim{}, fmt.Errorf("read memory deletion ledger generation: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_deletion_events(ledger_generation,event_type,workspace_id,claim_id,revision,deleted_at,instance_lineage_id,instance_generation,entity_kind,entity_target_version,instance_deletion_sequence) VALUES(?,'claim_deleted',?,?,?,?,?,?, 'memory_claim',?,?)`, ledgerGeneration, in.WorkspaceID, c.ID, c.Revision, deletedAt.Format(time.RFC3339Nano), lineageID, generation, c.Revision, globalSequence)
	if err != nil {
		return Claim{}, fmt.Errorf("append memory claim deletion event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE zoe_instance_state SET deletion_sequence=? WHERE singleton=1 AND generation=? AND deletion_sequence=?`, globalSequence, generation, currentSequence); err != nil {
		if !strings.Contains(err.Error(), "no such table") {
			return Claim{}, fmt.Errorf("advance instance deletion sequence: %w", err)
		}
	}
	for _, target := range cleanupTargets {
		if target == "claim" || target == "source_links" || target == "proposals" || target == "cache" || target == "source_ledger" {
			continue
		}
		id := uuid.NewString()
		payloadValue := map[string]any{"claim_id": c.ID, "target": target, "deletion_sequence": globalSequence}
		if target == "backup_delete_manifest" {
			payloadValue["expire_after_days"] = 30
		}
		payload, _ := json.Marshal(payloadValue)
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_jobs(id,workspace_id,kind,entity_id,entity_version,target,payload,status,available_at,created_at,updated_at) VALUES(?,?, 'delete_cleanup', ?, ?, ?, ?, 'pending', ?, ?, ?) ON CONFLICT(workspace_id,kind,entity_id,entity_version,target) DO NOTHING`, id, in.WorkspaceID, c.ID, c.Revision, target, payload, deletedAt.Format(time.RFC3339Nano), deletedAt.Format(time.RFC3339Nano), deletedAt.Format(time.RFC3339Nano)); err != nil {
			return Claim{}, err
		}
	}
	if err := completeLocalDeletionCheckpointsTx(ctx, tx, globalSequence, deletedAt.Format(time.RFC3339Nano)); err != nil {
		return Claim{}, err
	}
	return c, nil
}

func clearMemoryClaimLocalArtifactsTx(ctx context.Context, tx *sql.Tx, workspace, claimID, deletedAt string) error {
	// Context manifests are derived, short-lived caches. Invalidate the whole
	// workspace in the same transaction as the tombstone so no cache entry can
	// retain a deleted claim after the local cache checkpoint advances. The
	// shared embedding model cache is intentionally not claim-owned.
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_context_manifests WHERE workspace_id=?`, workspace); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_proposals SET status='deleted',content='',version=version+1,decided_at=?
WHERE workspace_id=? AND status='pending' AND EXISTS (
  SELECT 1 FROM memory_revision_sources rs WHERE rs.workspace_id=memory_proposals.workspace_id
    AND rs.claim_id=? AND rs.source_id=memory_proposals.source_id AND rs.source_version=memory_proposals.source_version
)`, deletedAt, workspace, claimID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE memory_sources AS s SET deleted_at=?
WHERE s.workspace_id=? AND EXISTS (
  SELECT 1 FROM memory_revision_sources rs WHERE rs.workspace_id=s.workspace_id AND rs.claim_id=? AND rs.source_id=s.id AND rs.source_version=s.version
) AND NOT EXISTS (
  SELECT 1 FROM memory_revision_sources other
  JOIN memory_claims c ON c.workspace_id=other.workspace_id AND c.id=other.claim_id
  WHERE other.workspace_id=s.workspace_id AND other.source_id=s.id AND other.source_version=s.version
    AND other.claim_id!=? AND c.deleted_at IS NULL AND c.status IN ('active','conflicted')
)`, deletedAt, workspace, claimID, claimID)
	return err
}

func completeLocalDeletionCheckpointsTx(ctx context.Context, tx *sql.Tx, sequence int64, now string) error {
	for _, target := range []string{"claim", "source_links", "proposals", "cache", "source_ledger"} {
		_, err := tx.ExecContext(ctx, `INSERT INTO zoe_deletion_reconciliation_checkpoints(target,completed_sequence,updated_at,last_error) VALUES(?,?,?,'')
ON CONFLICT(target) DO UPDATE SET completed_sequence=max(completed_sequence,excluded.completed_sequence),updated_at=excluded.updated_at,last_error=''`, target, sequence, now)
		if err != nil && strings.Contains(err.Error(), "no such table") {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func invalidateExportTempsTx(ctx context.Context, tx *sql.Tx, workspace string, sequence int64, now string) error {
	_, err := tx.ExecContext(ctx, `UPDATE zoe_export_temp_registry SET status='invalidated',invalidation_sequence=?,updated_at=?,last_error=''
WHERE workspace_id=? AND status='active' AND created_deletion_sequence<?`, sequence, now, workspace, sequence)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		// Older isolated memory-only fixtures do not register the control-plane
		// export migration. Production migration installs this table before use.
		return nil
	}
	return err
}

func (r *Repository) Search(ctx context.Context, p contracts.Principal, query string, filter SearchFilter) (SearchResult, error) {
	if !hasAnyMemoryAction(p, "read") || p.WorkspaceID == "" || p.SubjectID == "" {
		return SearchResult{}, ErrForbidden
	}
	if filter.Limit <= 0 || filter.Limit > 100 {
		filter.Limit = 20
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SearchResult{}, err
	}
	defer tx.Rollback()
	if err := storage.CheckRestoreDeletionReconciliationTx(ctx, tx); err != nil {
		return SearchResult{}, err
	}
	at := time.Now().UTC()
	if filter.At != nil {
		at = filter.At.UTC()
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.workspace_id,c.scope_kind,c.scope_id,c.principal_id,c.subject,c.claim_key,c.authority_kind,c.status,c.revision,c.valid_from,c.valid_to,c.supersedes_id,c.deleted_at,r.content
FROM memory_claims c JOIN memory_revisions r ON r.claim_id=c.id AND r.revision=c.revision
WHERE c.workspace_id=? AND c.deleted_at IS NULL AND c.status IN ('active','conflicted') ORDER BY c.updated_at DESC,c.id`, p.WorkspaceID)
	if err != nil {
		return SearchResult{}, err
	}
	terms := tokenize(query)
	journalled, err := journalledDeletionVersionsTx(ctx, tx, r.deletionJournal)
	if err != nil {
		return SearchResult{}, err
	}
	type candidate struct {
		claim   Claim
		content string
	}
	var candidates []candidate
	for rows.Next() {
		var c Claim
		var validFrom, validTo, supersedes, deleted sql.NullString
		var content string
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.Scope.Kind, &c.Scope.ID, &c.Scope.PrincipalID, &c.Subject, &c.Key, &c.Authority, &c.Status, &c.Revision, &validFrom, &validTo, &supersedes, &deleted, &content); err != nil {
			_ = rows.Close()
			return SearchResult{}, err
		}
		c.ValidFrom, c.ValidTo = parseTime(validFrom), parseTime(validTo)
		c.Supersedes = supersedes.String
		candidates = append(candidates, candidate{claim: c, content: content})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return SearchResult{}, err
	}
	_ = rows.Close()
	type scored struct {
		claim Claim
		score int
	}
	var found []scored
	result := SearchResult{Items: []Claim{}, Missing: []string{}, Conflicts: []string{}}
	for _, candidate := range candidates {
		c, content := candidate.claim, candidate.content
		if c.Revision <= journalled[c.WorkspaceID+"\x00"+c.ID] {
			continue
		}
		allowed, err := mayAccessScope(ctx, tx, p, c.Scope)
		if err != nil {
			return SearchResult{}, err
		}
		if !mayReadScope(p, c.Scope) || !allowed || !matchesFilter(c.Scope, filter) {
			continue
		}
		if c.ValidFrom != nil && c.ValidFrom.After(at) || c.ValidTo != nil && !c.ValidTo.After(at) {
			continue
		}
		sources, err := currentSources(ctx, tx, c.ID, c.Revision, c.WorkspaceID)
		if err != nil {
			return SearchResult{}, err
		}
		if len(sources) == 0 {
			result.Missing = append(result.Missing, c.ID+":source_unavailable")
			continue
		}
		c.Sources = sources
		if c.Status == ClaimConflicted {
			result.Conflicts = append(result.Conflicts, c.Subject+"/"+c.Key)
		}
		score := tokenScore(terms, tokenize(strings.Join([]string{c.Subject, c.Key, content}, " ")))
		if len(terms) == 0 || score > 0 || c.Status == ClaimConflicted {
			c.Content = content
			found = append(found, scored{claim: c, score: score})
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].score != found[j].score {
			return found[i].score > found[j].score
		}
		return found[i].claim.ID < found[j].claim.ID
	})
	if len(found) > filter.Limit {
		found = found[:filter.Limit]
	}
	for _, item := range found {
		result.Items = append(result.Items, item.claim)
	}
	result.Missing = uniqueStrings(result.Missing)
	result.Conflicts = uniqueStrings(result.Conflicts)
	if query != "" && len(result.Items) == 0 {
		result.Missing = append(result.Missing, "no_matching_memory")
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return SearchResult{}, err
	}
	return result, nil
}

// AssembleContext returns only source-verified claims visible to the recipient
// and records the exact revisions disclosed in a durable context manifest.
func (r *Repository) AssembleContext(ctx context.Context, principal contracts.Principal, taskID, recipientID, query string, budget int) (ContextManifest, []Claim, error) {
	return r.assembleContext(ctx, principal, taskID, recipientID, query, budget, nil)
}

func (r *Repository) assembleContext(ctx context.Context, principal contracts.Principal, taskID, recipientID, query string, budget int, beforeSave func()) (ContextManifest, []Claim, error) {
	if principal.WorkspaceID == "" || principal.SubjectID == "" || recipientID == "" {
		return ContextManifest{}, nil, ErrForbidden
	}
	if budget < 1 || budget > contracts.MemoryContextMaxBudgetTokens {
		return ContextManifest{}, nil, errors.New("context budget must be between 1 and 32000 tokens")
	}
	if recipientID != principal.SubjectID && !hasScope(principal.Scopes, "memory:context:any") {
		return ContextManifest{}, nil, ErrForbidden
	}
	filter := SearchFilter{TaskID: taskID, Limit: 100}
	if taskID != "" {
		tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return ContextManifest{}, nil, err
		}
		defer tx.Rollback()
		if err := storage.CheckRestoreDeletionReconciliationTx(ctx, tx); err != nil {
			return ContextManifest{}, nil, err
		}
		var projectID string
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(project_id,'') FROM tasks WHERE workspace_id=? AND id=?`, principal.WorkspaceID, taskID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ContextManifest{}, nil, ErrNotFound
			}
			return ContextManifest{}, nil, err
		}
		callerCanAccess, err := control.PrincipalCanAccessTask(ctx, tx, principal, taskID)
		if err != nil {
			return ContextManifest{}, nil, err
		}
		recipient := principal
		recipient.SubjectID = recipientID
		recipientCanAccess, err := control.PrincipalCanAccessTask(ctx, tx, recipient, taskID)
		if err != nil {
			return ContextManifest{}, nil, err
		}
		if !callerCanAccess || !recipientCanAccess {
			return ContextManifest{}, nil, ErrForbidden
		}
		filter.ProjectID = projectID
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			return ContextManifest{}, nil, err
		}
	}
	snapshotSequence, err := r.contextDeletionSequence(ctx)
	if err != nil {
		return ContextManifest{}, nil, err
	}
	reader := principal
	reader.SubjectID = recipientID
	result, err := r.Search(ctx, reader, query, filter)
	if err != nil {
		return ContextManifest{}, nil, err
	}
	manifest := ContextManifest{
		ID: uuid.NewString(), WorkspaceID: principal.WorkspaceID, TaskID: taskID,
		RecipientID: recipientID, Budget: budget, Missing: append([]string(nil), result.Missing...),
		Conflicts: append([]string(nil), result.Conflicts...), CreatedAt: time.Now().UTC(),
		Scopes: []Scope{}, Entries: []ContextEntry{},
	}
	claims := make([]Claim, 0, len(result.Items))
	used := 0
	scopeSeen := make(map[Scope]bool)
	for _, claim := range result.Items {
		// Leave headroom for metadata and tokenization variance. The estimate is
		// intentionally conservative and does not claim model-token precision.
		cost := (len([]rune(claim.Subject+claim.Key+claim.Content)) + 3) / 4
		if cost < 1 {
			cost = 1
		}
		if used+cost > budget {
			manifest.Missing = append(manifest.Missing, "context_budget_exhausted")
			break
		}
		used += cost
		sourceIDs := make([]string, 0, len(claim.Sources))
		for _, source := range claim.Sources {
			sourceIDs = append(sourceIDs, source.ID)
		}
		manifest.Entries = append(manifest.Entries, ContextEntry{ClaimID: claim.ID, Revision: claim.Revision, Authority: claim.Authority, SourceIDs: sourceIDs, ContentHash: hashString(claim.Content)})
		if !scopeSeen[claim.Scope] {
			manifest.Scopes = append(manifest.Scopes, claim.Scope)
			scopeSeen[claim.Scope] = true
		}
		claims = append(claims, claim)
	}
	manifest.Missing = uniqueStrings(manifest.Missing)
	if beforeSave != nil {
		beforeSave()
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ContextManifest{}, nil, err
	}
	defer tx.Rollback()
	if err := storage.CheckRestoreDeletionReconciliationTx(ctx, tx); err != nil {
		return ContextManifest{}, nil, err
	}
	currentSequence, err := contextDeletionSequenceTx(ctx, tx)
	if err != nil {
		return ContextManifest{}, nil, err
	}
	if currentSequence != snapshotSequence {
		return ContextManifest{}, nil, ErrContextStale
	}
	if err := r.SaveContext(ctx, tx, manifest); err != nil {
		return ContextManifest{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return ContextManifest{}, nil, err
	}
	return manifest, claims, nil
}

func (r *Repository) SaveContext(ctx context.Context, tx *sql.Tx, manifest ContextManifest) error {
	if manifest.ID == "" || manifest.WorkspaceID == "" || manifest.RecipientID == "" || manifest.Budget <= 0 {
		return errors.New("invalid context manifest")
	}
	journalled, err := journalledDeletionVersionsTx(ctx, tx, r.deletionJournal)
	if err != nil {
		return err
	}
	for _, entry := range manifest.Entries {
		if entry.ClaimID == "" || entry.Revision < 1 || journalled[manifest.WorkspaceID+"\x00"+entry.ClaimID] >= entry.Revision {
			return ErrContextStale
		}
		var revision int64
		var status string
		var deletedAt sql.NullString
		var content string
		err := tx.QueryRowContext(ctx, `SELECT c.revision,c.status,c.deleted_at,r.content
FROM memory_claims c JOIN memory_revisions r ON r.claim_id=c.id AND r.revision=c.revision
WHERE c.workspace_id=? AND c.id=?`, manifest.WorkspaceID, entry.ClaimID).Scan(&revision, &status, &deletedAt, &content)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrContextStale
		}
		if err != nil {
			return err
		}
		if revision != entry.Revision || (status != string(ClaimActive) && status != string(ClaimConflicted)) || deletedAt.Valid || hashString(content) != entry.ContentHash {
			return ErrContextStale
		}
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_context_manifests(id,workspace_id,task_id,recipient_id,manifest,created_at) VALUES(?,?,?,?,?,?)`, manifest.ID, manifest.WorkspaceID, manifest.TaskID, manifest.RecipientID, body, manifest.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (r *Repository) contextDeletionSequence(ctx context.Context) (int64, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err = storage.CheckRestoreDeletionReconciliationTx(ctx, tx); err != nil {
		return 0, err
	}
	return contextDeletionSequenceTx(ctx, tx)
}

func contextDeletionSequenceTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, `SELECT deletion_sequence FROM zoe_instance_state WHERE singleton=1`).Scan(&sequence)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return 0, nil
	}
	return sequence, err
}

func insertSourceTx(ctx context.Context, tx *sql.Tx, workspace string, s Source) error {
	if err := s.Validate(); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_sources(workspace_id,id,version,scope_kind,scope_id,principal_id,kind,reference,content_hash,recorded_at,producer) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(workspace_id,id,version) DO NOTHING`, workspace, s.ID, s.Version, s.Scope.Kind, s.Scope.ID, s.Scope.PrincipalID, s.Kind, s.Reference, s.ContentHash, s.RecordedAt.UTC().Format(time.RFC3339Nano), s.Producer)
	if err != nil {
		return err
	}
	var kind, reference, hash, scopeKind, scopeID, principalID string
	if err := tx.QueryRowContext(ctx, `SELECT kind,reference,content_hash,scope_kind,scope_id,principal_id FROM memory_sources WHERE workspace_id=? AND id=? AND version=? AND deleted_at IS NULL`, workspace, s.ID, s.Version).Scan(&kind, &reference, &hash, &scopeKind, &scopeID, &principalID); err != nil {
		return ErrInvalidSource
	}
	if kind != s.Kind || reference != s.Reference || hash != s.ContentHash || (Scope{Kind: ScopeKind(scopeKind), ID: scopeID, PrincipalID: principalID}) != s.Scope {
		return errors.New("source identity/version is already bound to different evidence")
	}
	return nil
}

func insertRevisionTx(ctx context.Context, tx *sql.Tx, workspace string, c Claim, actor, extractor string, sources []Source) error {
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_revisions(claim_id,revision,content,authority_kind,status,recorded_at,actor_id,extractor_version) VALUES(?,?,?,?,?,?,?,?)`, c.ID, c.Revision, c.Content, c.Authority, c.Status, now.Format(time.RFC3339Nano), actor, extractor); err != nil {
		return err
	}
	for _, source := range sources {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_revision_sources(claim_id,revision,workspace_id,source_id,source_version) VALUES(?,?,?,?,?)`, c.ID, c.Revision, workspace, source.ID, source.Version); err != nil {
			return err
		}
	}
	for _, term := range tokenize(strings.Join([]string{c.Subject, c.Key, c.Content}, " ")) {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_terms(workspace_id,claim_id,revision,term) VALUES(?,?,?,?)`, workspace, c.ID, c.Revision, term); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE memory_claims SET authority_kind=?,status=?,revision=?,valid_from=?,valid_to=?,supersedes_id=?,updated_at=? WHERE workspace_id=? AND id=?`, c.Authority, c.Status, c.Revision, timeValue(c.ValidFrom), timeValue(c.ValidTo), nullString(c.Supersedes), now.Format(time.RFC3339Nano), workspace, c.ID)
	return err
}

func setStatusRevisionTx(ctx context.Context, tx *sql.Tx, workspace, id string, revision int64, status ClaimStatus, actor string) error {
	var c Claim
	var content string
	var validFrom, validTo, supersedes sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT c.id,c.workspace_id,c.scope_kind,c.scope_id,c.principal_id,c.subject,c.claim_key,c.authority_kind,c.revision,c.valid_from,c.valid_to,c.supersedes_id,r.content FROM memory_claims c JOIN memory_revisions r ON r.claim_id=c.id AND r.revision=c.revision WHERE c.workspace_id=? AND c.id=? AND c.revision=?`, workspace, id, revision).Scan(&c.ID, &c.WorkspaceID, &c.Scope.Kind, &c.Scope.ID, &c.Scope.PrincipalID, &c.Subject, &c.Key, &c.Authority, &c.Revision, &validFrom, &validTo, &supersedes, &content); err != nil {
		return err
	}
	c.Content, c.Status = content, status
	c.ValidFrom, c.ValidTo, c.Supersedes = parseTime(validFrom), parseTime(validTo), supersedes.String
	c.Revision++
	_, err := tx.ExecContext(ctx, `UPDATE memory_claims SET status=?,revision=?,updated_at=? WHERE workspace_id=? AND id=? AND revision=?`, status, c.Revision, time.Now().UTC().Format(time.RFC3339Nano), workspace, id, revision)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_revisions(claim_id,revision,content,authority_kind,status,recorded_at,actor_id) VALUES(?,?,?,?,?,?,?)`, id, c.Revision, content, c.Authority, status, time.Now().UTC().Format(time.RFC3339Nano), actor); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_revision_sources(claim_id,revision,workspace_id,source_id,source_version) SELECT claim_id,?,workspace_id,source_id,source_version FROM memory_revision_sources WHERE claim_id=? AND revision=?`, c.Revision, id, revision); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_terms(workspace_id,claim_id,revision,term) SELECT workspace_id,claim_id,?,term FROM memory_terms WHERE claim_id=? AND revision=?`, c.Revision, id, revision); err != nil {
		return err
	}
	return nil
}

func copyRevisionSourcesAndAdd(ctx context.Context, tx *sql.Tx, workspace, id string, fromRevision, toRevision int64, sources []Source) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_revision_sources(claim_id,revision,workspace_id,source_id,source_version) SELECT claim_id,?,workspace_id,source_id,source_version FROM memory_revision_sources WHERE workspace_id=? AND claim_id=? AND revision=?`, toRevision, workspace, id, fromRevision); err != nil {
		return err
	}
	for _, source := range sources {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_revision_sources(claim_id,revision,workspace_id,source_id,source_version) VALUES(?,?,?,?,?)`, id, toRevision, workspace, source.ID, source.Version); err != nil {
			return err
		}
	}
	return nil
}

type queryRows interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type querySource interface {
	queryRows
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func currentSources(ctx context.Context, q querySource, claimID string, revision int64, workspace string) ([]Source, error) {
	rows, err := q.QueryContext(ctx, `SELECT s.id,s.version,s.scope_kind,s.scope_id,s.principal_id,s.kind,s.reference,s.content_hash,s.recorded_at,s.producer FROM memory_revision_sources rs JOIN memory_sources s ON s.workspace_id=rs.workspace_id AND s.id=rs.source_id AND s.version=rs.source_version WHERE rs.claim_id=? AND rs.revision=? AND rs.workspace_id=? AND s.deleted_at IS NULL ORDER BY s.id,s.version`, claimID, revision, workspace)
	if err != nil {
		return nil, err
	}
	var candidates []Source
	for rows.Next() {
		var s Source
		var recorded string
		var scopeKind string
		if err := rows.Scan(&s.ID, &s.Version, &scopeKind, &s.Scope.ID, &s.Scope.PrincipalID, &s.Kind, &s.Reference, &s.ContentHash, &recorded, &s.Producer); err != nil {
			return nil, err
		}
		s.Scope.Kind = ScopeKind(scopeKind)
		s.RecordedAt, _ = time.Parse(time.RFC3339Nano, recorded)
		candidates = append(candidates, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []Source
	for _, source := range candidates {
		valid, err := sourceStillAvailable(ctx, q, workspace, source)
		if err != nil {
			return nil, err
		}
		if valid {
			out = append(out, source)
		}
	}
	return out, nil
}

func sourceStillAvailable(ctx context.Context, q querySource, workspace string, source Source) (bool, error) {
	var current string
	switch source.Kind {
	case "user_note":
		return true, nil
	case "task_entry":
		id := strings.TrimPrefix(source.Reference, "task_entry:")
		err := q.QueryRowContext(ctx, `SELECT content FROM task_entries WHERE id=? AND workspace_id=?`, id, workspace).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	case "event":
		id := strings.TrimPrefix(source.Reference, "event:")
		var payload []byte
		err := q.QueryRowContext(ctx, `SELECT payload FROM events WHERE event_id=? AND workspace_id=?`, id, workspace).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return hashBytes(payload) == source.ContentHash, nil
	case "evidence":
		id := strings.TrimPrefix(source.Reference, "evidence:")
		err := q.QueryRowContext(ctx, `SELECT summary FROM evidence WHERE id=? AND workspace_id=?`, id, workspace).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	default:
		return false, nil
	}
	return hashString(current) == source.ContentHash, nil
}

func hasScope(scopes []string, scope string) bool {
	for _, candidate := range scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

func mayReadScope(p contracts.Principal, scope Scope) bool {
	return p.SubjectID != "" && (scope.Kind != ScopePersonal || scope.PrincipalID == p.SubjectID) && hasMemoryAction(p, "read", scope)
}

// mayAccessScope combines capability grants with authoritative ownership or
// membership. Project/task IDs in the same workspace do not imply access.
func mayAccessScope(ctx context.Context, tx *sql.Tx, p contracts.Principal, scope Scope) (bool, error) {
	switch scope.Kind {
	case ScopePersonal, ScopeWorkspace:
		return true, nil
	case ScopeProject:
		return control.PrincipalCanAccessProject(ctx, tx, p, scope.ID)
	case ScopeTask:
		return control.PrincipalCanAccessTask(ctx, tx, p, scope.ID)
	default:
		return false, nil
	}
}

func mayWriteAccessScope(ctx context.Context, tx *sql.Tx, p contracts.Principal, scope Scope) (bool, error) {
	switch scope.Kind {
	case ScopePersonal, ScopeWorkspace:
		return true, nil
	case ScopeProject:
		return control.PrincipalCanWriteProject(ctx, tx, p, scope.ID)
	case ScopeTask:
		return control.PrincipalCanWriteTask(ctx, tx, p, scope.ID)
	default:
		return false, nil
	}
}

func mayWriteScope(p contracts.Principal, scope Scope) bool {
	if p.WorkspaceID == "" || p.SubjectID == "" || !hasMemoryAction(p, "write", scope) {
		return false
	}
	if scope.Kind == ScopePersonal {
		return scope.PrincipalID == p.SubjectID
	}
	return scope.Kind == ScopeWorkspace || scope.Kind == ScopeProject || scope.Kind == ScopeTask
}

func mayDeleteScope(p contracts.Principal, scope Scope) bool {
	if p.WorkspaceID == "" || p.SubjectID == "" || !hasMemoryAction(p, "delete", scope) {
		return false
	}
	if scope.Kind == ScopePersonal {
		return scope.PrincipalID == p.SubjectID
	}
	return scope.Kind == ScopeWorkspace || scope.Kind == ScopeProject || scope.Kind == ScopeTask
}

// A workspace grant includes records attached to that principal's workspace
// projects and tasks. Narrower project/task grants remain available for
// actors and workers that should see only selected scopes.
func hasMemoryAction(p contracts.Principal, action string, scope Scope) bool {
	if scope.Kind == ScopePersonal && scope.PrincipalID != p.SubjectID {
		return false
	}
	if hasScope(p.Scopes, "memory:"+action+":"+string(scope.Kind)) {
		return true
	}
	if scope.Kind == ScopeProject || scope.Kind == ScopeTask {
		return hasScope(p.Scopes, "memory:"+action+":"+string(scope.Kind)+":"+scope.ID) || hasScope(p.Scopes, "memory:"+action+":workspace")
	}
	return false
}

func hasAnyMemoryAction(p contracts.Principal, action string) bool {
	prefix := "memory:" + action + ":"
	for _, scope := range p.Scopes {
		if strings.HasPrefix(scope, prefix) && len(scope) > len(prefix) {
			return true
		}
	}
	return false
}

func sourceContains(source, claim Scope) bool {
	if source == claim || source.Kind == ScopeWorkspace {
		return true
	}
	return false
}

func matchesFilter(scope Scope, f SearchFilter) bool {
	if f.ScopeKind != "" && scope.Kind != f.ScopeKind {
		return false
	}
	if f.ScopeID != "" && scope.ID != f.ScopeID {
		return false
	}
	if f.TaskID != "" {
		switch scope.Kind {
		case ScopePersonal, ScopeWorkspace:
		case ScopeProject:
			if f.ProjectID == "" || scope.ID != f.ProjectID {
				return false
			}
		case ScopeTask:
			if scope.ID != f.TaskID {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// tokenize combines Latin word tokens with Chinese and Japanese single-rune
// terms and adjacent bigrams. This makes one- and two-character CJK queries
// deterministic without claiming semantic retrieval.
func tokenize(input string) []string {
	runes := []rune(strings.ToLower(input))
	set := make(map[string]struct{})
	var latin strings.Builder
	flush := func() {
		if latin.Len() > 0 {
			set[latin.String()] = struct{}{}
			latin.Reset()
		}
	}
	for i, r := range runes {
		if isCJK(r) {
			flush()
			set[string(r)] = struct{}{}
			if i+1 < len(runes) && isCJK(runes[i+1]) {
				set[string([]rune{r, runes[i+1]})] = struct{}{}
			}
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			latin.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	out := make([]string, 0, len(set))
	for term := range set {
		out = append(out, term)
	}
	sort.Strings(out)
	return out
}

func isCJK(r rune) bool {
	return r >= 0x3400 && r <= 0x9fff || r >= 0xf900 && r <= 0xfaff || r >= 0x3040 && r <= 0x30ff
}

func tokenScore(query, document []string) int {
	doc := make(map[string]struct{}, len(document))
	for _, term := range document {
		doc[term] = struct{}{}
	}
	score := 0
	for _, term := range query {
		if _, ok := doc[term]; ok {
			score++
		}
	}
	return score
}

func normalizeText(s string) string {
	// Keep the search tokenizer's semantics but preserve token order and
	// repetition for equality/conflict checks.
	runes := []rune(strings.ToLower(s))
	var out []string
	var latin strings.Builder
	flush := func() {
		if latin.Len() > 0 {
			out = append(out, latin.String())
			latin.Reset()
		}
	}
	for i, r := range runes {
		if isCJK(r) {
			flush()
			out = append(out, string(r))
			if i+1 < len(runes) && isCJK(runes[i+1]) {
				out = append(out, string([]rune{r, runes[i+1]}))
			}
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			latin.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(out, " ")
}
func timeValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func parseTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil
	}
	return &t
}
func uniqueStrings(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func (c Claim) String() string { return fmt.Sprintf("%s@%d", c.ID, c.Revision) }
