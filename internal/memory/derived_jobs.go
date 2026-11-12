package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

const (
	derivedClaimScope    = "memory:jobs:claim"
	derivedCompleteScope = "memory:jobs:complete"
	derivedMaxPayload    = 64 << 10
)

// DerivedConfig bounds leases, retries, batches, and backoff. Zero values use
// conservative defaults. Runner identity and authority are always taken from
// authenticated request context, never from the request body.
type DerivedConfig struct {
	LeaseDuration            time.Duration
	MaxAttempts              int
	MaxBatch                 int
	RetryBase                time.Duration
	RetryMax                 time.Duration
	ExtractorID              string
	ModelCallLimit           int
	ModelTokenBudget         int
	BackupDeleteManifestPath string
}

func (c DerivedConfig) withDefaults() DerivedConfig {
	if c.LeaseDuration <= 0 || c.LeaseDuration > 15*time.Minute {
		c.LeaseDuration = 2 * time.Minute
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 20 {
		c.MaxAttempts = 8
	}
	if c.MaxBatch < 1 || c.MaxBatch > 100 {
		c.MaxBatch = 20
	}
	if c.RetryBase <= 0 {
		c.RetryBase = 2 * time.Second
	}
	if c.RetryMax < c.RetryBase || c.RetryMax > 24*time.Hour {
		c.RetryMax = 15 * time.Minute
	}
	if strings.TrimSpace(c.ExtractorID) == "" {
		c.ExtractorID = "mem0-controlled-v1"
	}
	if c.ModelCallLimit < 1 || c.ModelCallLimit > 100 {
		c.ModelCallLimit = 4
	}
	if c.ModelTokenBudget < 1 || c.ModelTokenBudget > 1_000_000 {
		c.ModelTokenBudget = 192_000
	}
	return c
}

// RegisterDerived installs the durable memory worker API. Call it after
// Register; its routes use the same provisioned runner credential boundary.
func RegisterDerived(s *control.Server, store *storage.Store, config DerivedConfig) error {
	if s == nil || store == nil || store.DB() == nil {
		return errorsf("derived memory registration requires control server and storage")
	}
	cfg := config.withDefaults()
	cfg.BackupDeleteManifestPath = strings.TrimSpace(cfg.BackupDeleteManifestPath)
	journal, _ := storage.NewDeletionJournal(store.Path())
	svc := &derivedService{db: store.DB(), cfg: cfg, deletionJournal: journal}
	return s.RegisterRunnerRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("POST /runner/v1/memory/jobs/claim", svc.handleClaim)
		mux.HandleFunc("POST /runner/v1/memory/jobs/{job_id}/validate", svc.handleValidate)
		mux.HandleFunc("POST /runner/v1/memory/jobs/{job_id}/complete", svc.handleComplete)
		mux.HandleFunc("POST /runner/v1/memory/jobs/{job_id}/fail", svc.handleFail)
		mux.HandleFunc("POST /runner/v1/memory/jobs/{job_id}/model-calls/reserve", svc.handleModelCallReserve)
		mux.HandleFunc("POST /runner/v1/memory/jobs/{job_id}/model-calls/{call_id}/finalize", svc.handleModelCallFinalize)
		mux.HandleFunc("POST /runner/v1/memory/deletion-jobs/claim", svc.handleDeletionClaim)
		mux.HandleFunc("POST /runner/v1/memory/deletion-jobs/{job_id}/validate", svc.handleDeletionValidate)
		mux.HandleFunc("POST /runner/v1/memory/deletion-jobs/{job_id}/complete", svc.handleDeletionComplete)
		mux.HandleFunc("POST /runner/v1/memory/deletion-jobs/{job_id}/fail", svc.handleDeletionFail)
		mux.HandleFunc("POST /runner/v1/memory/backup-delete-manifest/cleanup", svc.handleBackupDeleteManifestCleanup)
	})
}

type derivedService struct {
	db              *sql.DB
	cfg             DerivedConfig
	deletionJournal *storage.DeletionJournal
}

type derivedClaimRequest struct {
	Limit int `json:"limit"`
}

type derivedJob struct {
	ID            string          `json:"id"`
	WorkspaceID   string          `json:"workspace_id"`
	Kind          string          `json:"kind"`
	EntityID      string          `json:"entity_id"`
	EntityVersion int64           `json:"entity_version"`
	Target        string          `json:"target"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Attempt       int             `json:"attempt"`
	LeaseUntil    time.Time       `json:"lease_until"`
	Disposition   string          `json:"disposition,omitempty"`
	Claim         *derivedClaim   `json:"claim,omitempty"`
	Source        *derivedSource  `json:"source,omitempty"`
}

type derivedClaim struct {
	ID             string                 `json:"id"`
	WorkspaceID    string                 `json:"workspace_id"`
	Scope          Scope                  `json:"scope"`
	Subject        string                 `json:"subject"`
	Key            string                 `json:"key"`
	Content        string                 `json:"content"`
	ContentHash    string                 `json:"content_hash"`
	Authority      string                 `json:"authority_kind"`
	Status         string                 `json:"status"`
	Revision       int64                  `json:"revision"`
	SourceIDs      []string               `json:"source_ids"`
	SourceVersions []derivedSourceVersion `json:"source_versions"`
}

type derivedSourceVersion struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type derivedSource struct {
	ID             string `json:"id"`
	Version        int64  `json:"version"`
	Scope          Scope  `json:"scope"`
	Kind           string `json:"kind"`
	Reference      string `json:"reference"`
	ContentHash    string `json:"content_hash"`
	ExtractorInput string `json:"extractor_input"`
}

type derivedLeaseRequest struct {
	Attempt int `json:"attempt"`
}

type derivedValidation struct {
	ID          string `json:"id"`
	Allowed     bool   `json:"allowed"`
	Disposition string `json:"disposition,omitempty"`
}

type derivedFailRequest struct {
	Attempt   int    `json:"attempt"`
	ErrorCode string `json:"error_code"`
	Permanent bool   `json:"permanent"`
}

type derivedJobResult struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Disposition string `json:"disposition,omitempty"`
}

var safeMemoryErrorCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func (s *derivedService) handleClaim(w http.ResponseWriter, r *http.Request) {
	principal, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || principal.RunnerID == "" || !exactRunnerHasScope(principal, derivedClaimScope) {
		control.WriteError(w, control.NewAPIError(http.StatusForbidden, "forbidden", "runner memory job claim scope required", nil))
		return
	}
	var req derivedClaimRequest
	if err := decodeJSON(r, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	limit := req.Limit
	if limit <= 0 || limit > s.cfg.MaxBatch {
		limit = s.cfg.MaxBatch
	}
	jobs, err := s.claim(r.Context(), principal, limit)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": jobs})
}

func (s *derivedService) handleDeletionClaim(w http.ResponseWriter, r *http.Request) {
	principal, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || principal.RunnerID == "" || !exactRunnerHasScope(principal, derivedClaimScope) {
		control.WriteError(w, control.NewAPIError(http.StatusForbidden, "forbidden", "runner memory deletion claim scope required", nil))
		return
	}
	var req derivedClaimRequest
	if err := decodeJSON(r, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	limit := req.Limit
	if limit <= 0 || limit > s.cfg.MaxBatch {
		limit = s.cfg.MaxBatch
	}
	jobs, err := s.claimDeletionCleanup(r.Context(), principal, limit)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": jobs})
}

// Claim only tombstone-cleanup envelopes. This route never materializes a
// claim/source or calls reconcileTx, so it is safe while derived reads are gated.
func (s *derivedService) claimDeletionCleanup(ctx context.Context, p contracts.RunnerPrincipal, limit int) ([]derivedJob, error) {
	workspaces := exactRunnerWorkspaces(p)
	if len(workspaces) == 0 {
		return nil, control.NewAPIError(http.StatusForbidden, "forbidden", "runner has no exact workspace allowlist", nil)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	jobs := make([]derivedJob, 0, limit)
	for _, workspace := range workspaces {
		if len(jobs) >= limit {
			break
		}
		rows, queryErr := tx.QueryContext(ctx, `SELECT id,workspace_id,kind,entity_id,entity_version,target,payload,attempts FROM memory_jobs
WHERE workspace_id=? AND kind='delete_cleanup' AND attempts<? AND ((status IN ('pending','retry') AND available_at<=?) OR (status='leased' AND lease_until<=?))
ORDER BY available_at,created_at,id LIMIT ?`, workspace, s.cfg.MaxAttempts, nowText, nowText, limit-len(jobs))
		if queryErr != nil {
			return nil, queryErr
		}
		var candidates []derivedJob
		for rows.Next() {
			var job derivedJob
			var payload []byte
			if queryErr = rows.Scan(&job.ID, &job.WorkspaceID, &job.Kind, &job.EntityID, &job.EntityVersion, &job.Target, &payload, &job.Attempt); queryErr != nil {
				_ = rows.Close()
				return nil, queryErr
			}
			job.Payload = append(job.Payload[:0], payload...)
			candidates = append(candidates, job)
		}
		if queryErr = rows.Err(); queryErr != nil {
			_ = rows.Close()
			return nil, queryErr
		}
		if queryErr = rows.Close(); queryErr != nil {
			return nil, queryErr
		}
		for _, job := range candidates {
			leaseUntil := now.Add(s.cfg.LeaseDuration)
			result, updateErr := tx.ExecContext(ctx, `UPDATE memory_jobs SET status='leased',attempts=attempts+1,lease_owner=?,lease_until=?,updated_at=?
WHERE id=? AND workspace_id=? AND kind='delete_cleanup' AND attempts=? AND ((status IN ('pending','retry') AND available_at<=?) OR (status='leased' AND lease_until<=?))`, p.RunnerID, leaseUntil.Format(time.RFC3339Nano), nowText, job.ID, workspace, job.Attempt, nowText, nowText)
			if updateErr != nil {
				return nil, updateErr
			}
			changed, updateErr := result.RowsAffected()
			if updateErr != nil || changed != 1 {
				if updateErr != nil {
					return nil, updateErr
				}
				continue
			}
			job.Attempt++
			job.LeaseUntil = leaseUntil
			jobs = append(jobs, job)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *derivedService) claim(ctx context.Context, p contracts.RunnerPrincipal, limit int) ([]derivedJob, error) {
	workspaces := exactRunnerWorkspaces(p)
	if len(workspaces) == 0 {
		return nil, control.NewAPIError(http.StatusForbidden, "forbidden", "runner has no exact workspace allowlist", nil)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	for _, workspace := range workspaces {
		if err := s.reconcileTx(ctx, tx, workspace, now); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET status='blocked',lease_owner='',last_error_code='attempt_limit',updated_at=? WHERE workspace_id=? AND status='leased' AND lease_until<=? AND attempts>=?`, nowText, workspace, nowText, s.cfg.MaxAttempts); err != nil {
			return nil, err
		}
	}
	var jobs []derivedJob
	for _, workspace := range workspaces {
		if len(jobs) >= limit {
			break
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,workspace_id,kind,entity_id,entity_version,target,payload,attempts FROM memory_jobs
WHERE workspace_id=? AND NOT(kind='extract' AND target='voice_call_outcome') AND attempts<? AND ((status IN ('pending','retry') AND available_at<=?) OR (status='leased' AND lease_until<=?))
ORDER BY available_at,created_at,id LIMIT ?`, workspace, s.cfg.MaxAttempts, nowText, nowText, limit-len(jobs))
		if err != nil {
			return nil, err
		}
		var candidates []derivedJob
		for rows.Next() {
			var job derivedJob
			var payload []byte
			if err := rows.Scan(&job.ID, &job.WorkspaceID, &job.Kind, &job.EntityID, &job.EntityVersion, &job.Target, &payload, &job.Attempt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			job.Payload = append(json.RawMessage(nil), payload...)
			candidates = append(candidates, job)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
		for _, job := range candidates {
			leaseUntil := now.Add(s.cfg.LeaseDuration)
			res, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET status='leased',attempts=attempts+1,lease_owner=?,lease_until=?,updated_at=?
WHERE id=? AND workspace_id=? AND attempts=? AND ((status IN ('pending','retry') AND available_at<=?) OR (status='leased' AND lease_until<=?))`, p.RunnerID, leaseUntil.Format(time.RFC3339Nano), nowText, job.ID, workspace, job.Attempt, nowText, nowText)
			if err != nil {
				return nil, err
			}
			changed, err := res.RowsAffected()
			if err != nil || changed != 1 {
				if err != nil {
					return nil, err
				}
				continue
			}
			job.Attempt++
			job.LeaseUntil = leaseUntil
			current, disposition, err := s.materializeTx(ctx, tx, &job)
			if err != nil {
				return nil, err
			}
			if disposition != "" {
				state, code := "done", ""
				if disposition == "deletion_tombstone_missing" {
					state, code = "blocked", disposition
				}
				if _, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET status=?,last_error_code=?,updated_at=? WHERE id=? AND lease_owner=? AND attempts=?`, state, code, nowText, job.ID, p.RunnerID, job.Attempt); err != nil {
					return nil, err
				}
				continue
			}
			job.Disposition = disposition
			job.Claim = current.Claim
			job.Source = current.Source
			jobs = append(jobs, job)
			if len(jobs) >= limit {
				break
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *derivedService) materializeTx(ctx context.Context, tx *sql.Tx, job *derivedJob) (*derivedJob, string, error) {
	if job.Kind == "index" || job.Kind == "extract" {
		if err := storage.CheckRestoreDeletionReconciliationTx(ctx, tx); err != nil {
			return nil, "", err
		}
		versions, err := journalledDeletionVersionsTx(ctx, tx, s.deletionJournal)
		if err != nil {
			return nil, "", err
		}
		if job.EntityVersion <= versions[job.WorkspaceID+"\x00"+job.EntityID] {
			return &derivedJob{ID: job.ID, WorkspaceID: job.WorkspaceID, Kind: job.Kind, EntityID: job.EntityID, EntityVersion: job.EntityVersion, Target: job.Target}, "stale_generation", nil
		}
	}
	out := &derivedJob{ID: job.ID, WorkspaceID: job.WorkspaceID, Kind: job.Kind, EntityID: job.EntityID, EntityVersion: job.EntityVersion, Target: job.Target, Attempt: job.Attempt, LeaseUntil: job.LeaseUntil, Payload: job.Payload}
	switch job.Kind {
	case "index", "extract":
		var claim derivedClaim
		var scopeKind string
		err := tx.QueryRowContext(ctx, `SELECT c.id,c.workspace_id,c.scope_kind,c.scope_id,c.principal_id,c.subject,c.claim_key,c.authority_kind,c.status,c.revision,r.content
FROM memory_claims c JOIN memory_revisions r ON r.claim_id=c.id AND r.revision=c.revision
WHERE c.workspace_id=? AND c.id=? AND c.revision=? AND c.status IN ('active','conflicted') AND c.deleted_at IS NULL`, job.WorkspaceID, job.EntityID, job.EntityVersion).Scan(&claim.ID, &claim.WorkspaceID, &scopeKind, &claim.Scope.ID, &claim.Scope.PrincipalID, &claim.Subject, &claim.Key, &claim.Authority, &claim.Status, &claim.Revision, &claim.Content)
		if errors.Is(err, sql.ErrNoRows) {
			return out, "stale_generation", nil
		}
		if err != nil {
			return nil, "", err
		}
		claim.Scope.Kind = ScopeKind(scopeKind)
		claim.ContentHash = hashString(claim.Content)
		if job.Kind == "index" {
			sources, sourceVersions, valid, err := loadCurrentSources(ctx, tx, job.WorkspaceID, claim.ID, claim.Revision)
			if err != nil {
				return nil, "", err
			}
			if !valid {
				return out, "source_unavailable", nil
			}
			claim.SourceIDs = sources
			claim.SourceVersions = sourceVersions
			out.Claim = &claim
			return out, "", nil
		}
		source, material, err := loadExtractionSource(ctx, tx, job.WorkspaceID, claim.ID, claim.Revision, job.Target)
		if errors.Is(err, ErrInvalidSource) {
			return out, "source_unavailable", nil
		}
		if err != nil {
			return nil, "", err
		}
		if source == nil {
			return out, "source_unavailable", nil
		}
		if strings.TrimSpace(material) == "" {
			return out, "empty_source", nil
		}
		out.Claim = &claim
		out.Source = &derivedSource{ID: source.ID, Version: source.Version, Scope: source.Scope, Kind: source.Kind, Reference: source.Reference, ContentHash: source.ContentHash, ExtractorInput: material}
		return out, "", nil
	case "delete_cleanup":
		if job.Target != "claim" && job.Target != "source_links" && job.Target != "proposals" && job.Target != "mem0" && job.Target != "qdrant" && job.Target != "cache" && job.Target != "export_temp" && job.Target != "backup_delete_manifest" && job.Target != "source_ledger" {
			return nil, "", fmt.Errorf("unsupported memory cleanup target")
		}
		var revision int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM memory_tombstones WHERE workspace_id=? AND claim_id=?`, job.WorkspaceID, job.EntityID).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return out, "deletion_tombstone_missing", nil
		}
		if err != nil {
			return nil, "", err
		}
		if revision != job.EntityVersion {
			return out, "stale_generation", nil
		}
		var currentRevision int64
		var status string
		err = tx.QueryRowContext(ctx, `SELECT revision,status FROM memory_claims WHERE workspace_id=? AND id=?`, job.WorkspaceID, job.EntityID).Scan(&currentRevision, &status)
		if errors.Is(err, sql.ErrNoRows) || currentRevision != job.EntityVersion || status != string(ClaimTombstoned) {
			return out, "stale_generation", nil
		}
		return out, "", nil
	default:
		return nil, "", fmt.Errorf("unknown memory job kind")
	}
}

func loadCurrentSources(ctx context.Context, q querySource, workspace, claimID string, revision int64) ([]string, []derivedSourceVersion, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT s.id,s.version,s.scope_kind,s.scope_id,s.principal_id,s.kind,s.reference,s.content_hash,s.recorded_at,s.producer
FROM memory_revision_sources rs JOIN memory_sources s ON s.workspace_id=rs.workspace_id AND s.id=rs.source_id AND s.version=rs.source_version
WHERE rs.workspace_id=? AND rs.claim_id=? AND rs.revision=? AND s.deleted_at IS NULL ORDER BY s.id,s.version`, workspace, claimID, revision)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	var ids []string
	var versions []derivedSourceVersion
	for rows.Next() {
		var source Source
		var kind, recorded string
		if err := rows.Scan(&source.ID, &source.Version, &kind, &source.Scope.ID, &source.Scope.PrincipalID, &source.Kind, &source.Reference, &source.ContentHash, &recorded, &source.Producer); err != nil {
			return nil, nil, false, err
		}
		source.Scope.Kind = ScopeKind(kind)
		source.RecordedAt, _ = time.Parse(time.RFC3339Nano, recorded)
		valid, err := sourceStillAvailable(ctx, q, workspace, source)
		if err != nil {
			return nil, nil, false, err
		}
		if !valid {
			return nil, nil, false, nil
		}
		ids = append(ids, source.ID)
		versions = append(versions, derivedSourceVersion{ID: source.ID, Version: source.Version})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	if len(ids) == 0 {
		return nil, nil, false, nil
	}
	return ids, versions, true, nil
}

func loadExtractionSource(ctx context.Context, q querySource, workspace, claimID string, claimRevision int64, sourceID string) (*Source, string, error) {
	var source Source
	var scopeKind, recorded string
	err := q.QueryRowContext(ctx, `SELECT s.id,s.version,s.scope_kind,s.scope_id,s.principal_id,s.kind,s.reference,s.content_hash,s.recorded_at,s.producer
FROM memory_revision_sources rs JOIN memory_sources s ON s.workspace_id=rs.workspace_id AND s.id=rs.source_id AND s.version=rs.source_version
WHERE rs.workspace_id=? AND rs.claim_id=? AND rs.revision=? AND rs.source_id=? AND s.deleted_at IS NULL`, workspace, claimID, claimRevision, sourceID).Scan(&source.ID, &source.Version, &scopeKind, &source.Scope.ID, &source.Scope.PrincipalID, &source.Kind, &source.Reference, &source.ContentHash, &recorded, &source.Producer)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrInvalidSource
	}
	if err != nil {
		return nil, "", err
	}
	source.Scope.Kind = ScopeKind(scopeKind)
	source.RecordedAt, _ = time.Parse(time.RFC3339Nano, recorded)
	valid, err := sourceStillAvailable(ctx, q, workspace, source)
	if err != nil {
		return nil, "", err
	}
	if !valid {
		return nil, "", ErrInvalidSource
	}
	var material string
	switch source.Kind {
	case "task_entry":
		id := strings.TrimPrefix(source.Reference, "task_entry:")
		err = q.QueryRowContext(ctx, `SELECT content FROM task_entries WHERE id=? AND workspace_id=?`, id, workspace).Scan(&material)
	case "event":
		id := strings.TrimPrefix(source.Reference, "event:")
		var payload []byte
		err = q.QueryRowContext(ctx, `SELECT payload FROM events WHERE event_id=? AND workspace_id=?`, id, workspace).Scan(&payload)
		material = string(payload)
	case "evidence":
		id := strings.TrimPrefix(source.Reference, "evidence:")
		err = q.QueryRowContext(ctx, `SELECT summary FROM evidence WHERE id=? AND workspace_id=?`, id, workspace).Scan(&material)
	default:
		return &source, "", ErrInvalidSource
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrInvalidSource
	}
	if err != nil {
		return nil, "", err
	}
	if !utf8.ValidString(material) {
		material = strings.ToValidUTF8(material, "�")
	}
	if len(material) > 32<<10 {
		material = material[:32<<10]
		for !utf8.ValidString(material) {
			material = material[:len(material)-1]
		}
	}
	return &source, material, nil
}

func (s *derivedService) reconcileTx(ctx context.Context, tx *sql.Tx, workspace string, now time.Time) error {
	nowText := now.Format(time.RFC3339Nano)
	if err := s.reconcileSourceLedgerJobsTx(ctx, tx, workspace, nowText); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.revision FROM memory_claims c
WHERE c.workspace_id=? AND c.deleted_at IS NULL AND c.status IN ('active','conflicted')
AND NOT EXISTS (SELECT 1 FROM memory_jobs j WHERE j.workspace_id=c.workspace_id AND j.kind='index' AND j.entity_id=c.id AND j.entity_version=c.revision AND j.target='qdrant')
ORDER BY c.updated_at DESC,c.id LIMIT 500`, workspace)
	if err != nil {
		return err
	}
	type generation struct {
		id       string
		revision int64
	}
	var claims []generation
	for rows.Next() {
		var item generation
		if err := rows.Scan(&item.id, &item.revision); err != nil {
			_ = rows.Close()
			return err
		}
		claims = append(claims, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, claim := range claims {
		if err := insertDerivedJob(ctx, tx, workspace, "index", claim.id, claim.revision, "qdrant", map[string]any{"claim_id": claim.id, "revision": claim.revision}, nowText); err != nil {
			return err
		}
	}
	// Reconcile extraction independently from index jobs. A single claim can
	// have more sources than one batch, and every poll must be able to advance
	// through the remaining missing source jobs even after its index job exists.
	refs, err := tx.QueryContext(ctx, `SELECT rs.claim_id,rs.revision,rs.source_id,rs.source_version FROM memory_revision_sources rs
JOIN memory_claims c ON c.workspace_id=rs.workspace_id AND c.id=rs.claim_id AND c.revision=rs.revision
JOIN memory_sources s ON s.workspace_id=rs.workspace_id AND s.id=rs.source_id AND s.version=rs.source_version
WHERE rs.workspace_id=? AND c.deleted_at IS NULL AND c.status IN ('active','conflicted') AND s.deleted_at IS NULL AND s.kind IN ('task_entry','event','evidence')
AND NOT EXISTS (SELECT 1 FROM memory_jobs j WHERE j.workspace_id=rs.workspace_id AND j.kind='extract' AND j.entity_id=rs.claim_id AND j.entity_version=rs.revision AND j.target=rs.source_id)
ORDER BY rs.claim_id,rs.source_id,rs.source_version LIMIT 500`, workspace)
	if err != nil {
		return err
	}
	type ref struct {
		claimID  string
		revision int64
		id       string
		version  int64
	}
	var sources []ref
	for refs.Next() {
		var source ref
		if err := refs.Scan(&source.claimID, &source.revision, &source.id, &source.version); err != nil {
			_ = refs.Close()
			return err
		}
		sources = append(sources, source)
	}
	if err := refs.Err(); err != nil {
		_ = refs.Close()
		return err
	}
	_ = refs.Close()
	for _, source := range sources {
		if err := insertDerivedJob(ctx, tx, workspace, "extract", source.claimID, source.revision, source.id, map[string]any{"claim_id": source.claimID, "claim_revision": source.revision, "source_id": source.id, "source_version": source.version, "extractor_version": s.cfg.ExtractorID}, nowText); err != nil {
			return err
		}
	}
	return nil
}

const sourceLedgerReconcileBatch = 500

// reconcileSourceLedgerJobsTx performs source-ledger cleanup in the authority
// transaction. This target is deliberately not dispatched to the Python
// worker: the claim-owned links and revision history live in this database.
func (s *derivedService) reconcileSourceLedgerJobsTx(ctx context.Context, tx *sql.Tx, workspace, now string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,entity_id,entity_version,attempts FROM memory_jobs
WHERE workspace_id=? AND kind='delete_cleanup' AND target='source_ledger'
AND status IN ('pending','retry','blocked','leased')
AND last_error_code NOT IN ('deletion_tombstone_missing','deletion_tombstone_mismatch')
ORDER BY created_at,id LIMIT ?`, workspace, sourceLedgerReconcileBatch)
	if err != nil {
		return err
	}
	type item struct {
		id, claimID string
		version     int64
		attempts    int
	}
	var jobs []item
	for rows.Next() {
		var job item
		if err := rows.Scan(&job.id, &job.claimID, &job.version, &job.attempts); err != nil {
			_ = rows.Close()
			return err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, job := range jobs {
		if _, err := settleSourceLedgerJobTx(ctx, tx, workspace, job.id, job.claimID, job.version, job.attempts, now); err != nil {
			return err
		}
	}
	return refreshDeletionCheckpointTx(ctx, tx, "source_ledger", now)
}

// settleSourceLedgerJobTx validates the current tombstone fence and performs
// only claim-owned cleanup. Its job update clears any old worker lease in the
// same transaction, fencing delayed worker complete/fail requests.
func settleSourceLedgerJobTx(ctx context.Context, tx *sql.Tx, workspace, jobID, claimID string, jobVersion int64, attempts int, now string) (string, error) {
	tombstoneRevision, found, complete, err := currentCompleteTombstoneTx(ctx, tx, workspace, claimID)
	if err != nil {
		return "", err
	}
	disposition := ""
	state := "done"
	switch {
	case !found:
		disposition, state = "deletion_tombstone_missing", "blocked"
	case !complete:
		disposition, state = "deletion_tombstone_mismatch", "blocked"
	case jobVersion < 1 || jobVersion > tombstoneRevision:
		disposition, state = "deletion_tombstone_mismatch", "blocked"
	case jobVersion < tombstoneRevision:
		// This job describes an obsolete generation. Completing it says only
		// that this stale job is settled; it does not claim current erasure.
		disposition = "stale_generation"
	default:
		if _, err = tx.ExecContext(ctx, `DELETE FROM memory_revision_sources WHERE workspace_id=? AND claim_id=? AND revision<?`, workspace, claimID, tombstoneRevision); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM memory_terms WHERE workspace_id=? AND claim_id=? AND revision<?`, workspace, claimID, tombstoneRevision); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM memory_revisions WHERE claim_id=? AND revision<?`, claimID, tombstoneRevision); err != nil {
			return "", err
		}
		var remaining int
		if err = tx.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM memory_revisions WHERE claim_id=? AND revision<?) +
(SELECT count(*) FROM memory_revision_sources WHERE workspace_id=? AND claim_id=? AND revision<?) +
(SELECT count(*) FROM memory_terms WHERE workspace_id=? AND claim_id=? AND revision<?)`, claimID, tombstoneRevision, workspace, claimID, tombstoneRevision, workspace, claimID, tombstoneRevision).Scan(&remaining); err != nil {
			return "", err
		}
		if remaining != 0 {
			return "", fmt.Errorf("source-ledger cleanup verification failed")
		}
		disposition = "source_ledger_cleaned"
	}
	_, err = tx.ExecContext(ctx, `UPDATE memory_jobs SET status=?,lease_owner='',lease_until=NULL,last_error_code=?,updated_at=?
WHERE id=? AND workspace_id=? AND kind='delete_cleanup' AND target='source_ledger' AND attempts=?
AND status IN ('pending','retry','blocked','leased')`, state, disposition, now, jobID, workspace, attempts)
	return disposition, err
}

// currentCompleteTombstoneTx verifies the complete authoritative tombstone
// record before either stale-job settlement or destructive source-ledger
// cleanup. Tombstones are written with an empty revision and no derived rows;
// their claim, ledger, and revision timestamps must all identify one deletion.
func currentCompleteTombstoneTx(ctx context.Context, tx *sql.Tx, workspace, claimID string) (int64, bool, bool, error) {
	var tombstoneRevision int64
	var claimRevision sql.NullInt64
	var claimStatus, claimDeletedAt, tombstoneDeletedAt, revisionStatus, revisionContent, revisionRecordedAt sql.NullString
	var linkedSources, terms int
	err := tx.QueryRowContext(ctx, `SELECT t.revision,c.revision,c.status,c.deleted_at,t.deleted_at,
r.status,r.content,r.recorded_at,
(SELECT count(*) FROM memory_revision_sources rs WHERE rs.workspace_id=t.workspace_id AND rs.claim_id=t.claim_id AND rs.revision=t.revision),
(SELECT count(*) FROM memory_terms mt WHERE mt.workspace_id=t.workspace_id AND mt.claim_id=t.claim_id AND mt.revision=t.revision)
FROM memory_tombstones t
LEFT JOIN memory_claims c ON c.workspace_id=t.workspace_id AND c.id=t.claim_id
LEFT JOIN memory_revisions r ON r.claim_id=t.claim_id AND r.revision=t.revision
WHERE t.workspace_id=? AND t.claim_id=?`, workspace, claimID).Scan(
		&tombstoneRevision, &claimRevision, &claimStatus, &claimDeletedAt, &tombstoneDeletedAt,
		&revisionStatus, &revisionContent, &revisionRecordedAt, &linkedSources, &terms)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, err
	}
	complete := tombstoneRevision > 0 && claimRevision.Valid && claimRevision.Int64 == tombstoneRevision &&
		claimStatus.Valid && claimStatus.String == string(ClaimTombstoned) && claimDeletedAt.Valid && claimDeletedAt.String != "" &&
		tombstoneDeletedAt.Valid && tombstoneDeletedAt.String == claimDeletedAt.String &&
		revisionStatus.Valid && revisionStatus.String == string(ClaimTombstoned) && revisionContent.Valid && revisionContent.String == "" &&
		revisionRecordedAt.Valid && revisionRecordedAt.String == claimDeletedAt.String &&
		linkedSources == 0 && terms == 0
	return tombstoneRevision, true, complete, nil
}

func insertDerivedJob(ctx context.Context, tx *sql.Tx, workspace, kind, entityID string, version int64, target string, payload any, now string) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_jobs(id,workspace_id,kind,entity_id,entity_version,target,payload,status,available_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,'pending',?,?,?) ON CONFLICT(workspace_id,kind,entity_id,entity_version,target) DO NOTHING`, uuid.NewString(), workspace, kind, entityID, version, target, data, now, now, now)
	return err
}

func (s *derivedService) handleComplete(w http.ResponseWriter, r *http.Request) {
	p, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || p.RunnerID == "" || !exactRunnerHasScope(p, derivedCompleteScope) {
		control.WriteError(w, control.NewAPIError(http.StatusForbidden, "forbidden", "runner memory job completion scope required", nil))
		return
	}
	var req derivedLeaseRequest
	if err := decodeJSON(r, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	result, err := s.finish(r.Context(), p, r.PathValue("job_id"), req.Attempt, "", false, true)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *derivedService) handleDeletionComplete(w http.ResponseWriter, r *http.Request) {
	p, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || p.RunnerID == "" || !exactRunnerHasScope(p, derivedCompleteScope) {
		control.WriteError(w, control.NewAPIError(http.StatusForbidden, "forbidden", "runner deletion completion scope required", nil))
		return
	}
	if err := s.requireDeletionJob(r.Context(), p, r.PathValue("job_id")); err != nil {
		control.WriteError(w, err)
		return
	}
	var req derivedLeaseRequest
	if err := decodeJSON(r, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	result, err := s.finish(r.Context(), p, r.PathValue("job_id"), req.Attempt, "", false, true)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *derivedService) handleValidate(w http.ResponseWriter, r *http.Request) {
	p, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || p.RunnerID == "" || !exactRunnerHasScope(p, derivedClaimScope) {
		control.WriteError(w, control.NewAPIError(http.StatusForbidden, "forbidden", "runner memory job validation scope required", nil))
		return
	}
	var req derivedLeaseRequest
	if err := decodeJSON(r, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	result, err := s.validate(r.Context(), p, r.PathValue("job_id"), req.Attempt)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *derivedService) handleDeletionValidate(w http.ResponseWriter, r *http.Request) {
	p, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || p.RunnerID == "" || !exactRunnerHasScope(p, derivedClaimScope) {
		control.WriteError(w, control.NewAPIError(http.StatusForbidden, "forbidden", "runner deletion validation scope required", nil))
		return
	}
	id := r.PathValue("job_id")
	if err := s.requireDeletionJob(r.Context(), p, id); err != nil {
		control.WriteError(w, err)
		return
	}
	var req derivedLeaseRequest
	if err := decodeJSON(r, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	result, err := s.validate(r.Context(), p, id, req.Attempt)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *derivedService) validate(ctx context.Context, p contracts.RunnerPrincipal, id string, attempt int) (derivedValidation, error) {
	if id == "" || attempt < 1 {
		return derivedValidation{}, control.NewAPIError(http.StatusBadRequest, "invalid_request", "job ID and positive lease attempt are required", nil)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return derivedValidation{}, err
	}
	defer tx.Rollback()
	var job derivedJob
	var payload []byte
	var status, owner string
	var leaseUntil sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,workspace_id,kind,entity_id,entity_version,target,payload,attempts,status,lease_owner,lease_until FROM memory_jobs WHERE id=?`, id).Scan(&job.ID, &job.WorkspaceID, &job.Kind, &job.EntityID, &job.EntityVersion, &job.Target, &payload, &job.Attempt, &status, &owner, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return derivedValidation{}, control.NewAPIError(http.StatusNotFound, "not_found", "memory job not found", nil)
	}
	if err != nil {
		return derivedValidation{}, err
	}
	job.Payload = append(job.Payload[:0], payload...)
	if !exactRunnerHasWorkspace(p, job.WorkspaceID) || !exactRunnerHasScope(p, derivedClaimScope) {
		return derivedValidation{}, control.NewAPIError(http.StatusForbidden, "forbidden", "runner cannot validate this workspace job", nil)
	}
	if status != "leased" || owner != p.RunnerID || job.Attempt != attempt || !leaseUntil.Valid {
		return derivedValidation{}, control.NewAPIError(http.StatusConflict, "lease_lost", "memory job lease is no longer current", nil)
	}
	deadline, parseErr := time.Parse(time.RFC3339Nano, leaseUntil.String)
	if parseErr != nil || !deadline.After(time.Now().UTC()) {
		return derivedValidation{}, control.NewAPIError(http.StatusConflict, "lease_expired", "memory job lease expired", nil)
	}
	if job.Kind == "delete_cleanup" && job.Target == "source_ledger" {
		disposition, settleErr := settleSourceLedgerJobTx(ctx, tx, job.WorkspaceID, job.ID, job.EntityID, job.EntityVersion, job.Attempt, time.Now().UTC().Format(time.RFC3339Nano))
		if settleErr != nil {
			return derivedValidation{}, settleErr
		}
		if err := tx.Commit(); err != nil {
			return derivedValidation{}, err
		}
		return derivedValidation{ID: id, Allowed: false, Disposition: disposition}, nil
	}
	_, disposition, err := s.materializeTx(ctx, tx, &job)
	if err != nil {
		return derivedValidation{}, err
	}
	if disposition == "deletion_tombstone_missing" {
		_, err = tx.ExecContext(ctx, `UPDATE memory_jobs SET status='blocked',last_error_code=?,updated_at=? WHERE id=? AND status='leased' AND lease_owner=? AND attempts=?`, disposition, time.Now().UTC().Format(time.RFC3339Nano), id, p.RunnerID, attempt)
		if err != nil {
			return derivedValidation{}, err
		}
		if err := tx.Commit(); err != nil {
			return derivedValidation{}, err
		}
		return derivedValidation{ID: id, Allowed: false, Disposition: disposition}, nil
	}
	if err := tx.Commit(); err != nil {
		return derivedValidation{}, err
	}
	return derivedValidation{ID: id, Allowed: disposition == "", Disposition: disposition}, nil
}

func (s *derivedService) handleFail(w http.ResponseWriter, r *http.Request) {
	p, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || p.RunnerID == "" || !exactRunnerHasScope(p, derivedCompleteScope) {
		control.WriteError(w, control.NewAPIError(http.StatusForbidden, "forbidden", "runner memory job completion scope required", nil))
		return
	}
	var req derivedFailRequest
	if err := decodeJSON(r, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	if !safeMemoryErrorCode.MatchString(req.ErrorCode) {
		control.WriteError(w, control.NewAPIError(http.StatusBadRequest, "invalid_request", "error_code must be a short safe identifier", nil))
		return
	}
	result, err := s.finish(r.Context(), p, r.PathValue("job_id"), req.Attempt, req.ErrorCode, req.Permanent, false)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *derivedService) handleDeletionFail(w http.ResponseWriter, r *http.Request) {
	p, ok := control.RunnerPrincipalFromContext(r.Context())
	if !ok || p.RunnerID == "" || !exactRunnerHasScope(p, derivedCompleteScope) {
		control.WriteError(w, control.NewAPIError(http.StatusForbidden, "forbidden", "runner deletion completion scope required", nil))
		return
	}
	if err := s.requireDeletionJob(r.Context(), p, r.PathValue("job_id")); err != nil {
		control.WriteError(w, err)
		return
	}
	var req derivedFailRequest
	if err := decodeJSON(r, &req); err != nil {
		control.WriteError(w, err)
		return
	}
	if !safeMemoryErrorCode.MatchString(req.ErrorCode) {
		control.WriteError(w, control.NewAPIError(http.StatusBadRequest, "invalid_request", "error_code must be a short safe identifier", nil))
		return
	}
	result, err := s.finish(r.Context(), p, r.PathValue("job_id"), req.Attempt, req.ErrorCode, req.Permanent, false)
	if err != nil {
		control.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *derivedService) requireDeletionJob(ctx context.Context, p contracts.RunnerPrincipal, id string) error {
	var workspace, kind string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id,kind FROM memory_jobs WHERE id=?`, id).Scan(&workspace, &kind); errors.Is(err, sql.ErrNoRows) {
		return control.NewAPIError(http.StatusNotFound, "not_found", "memory deletion job not found", nil)
	} else if err != nil {
		return err
	}
	if kind != "delete_cleanup" || !exactRunnerHasWorkspace(p, workspace) {
		return control.NewAPIError(http.StatusForbidden, "forbidden", "deletion-only route accepts only an authorized cleanup job", nil)
	}
	return nil
}

func (s *derivedService) finish(ctx context.Context, p contracts.RunnerPrincipal, id string, attempt int, code string, permanent, complete bool) (derivedJobResult, error) {
	if id == "" || attempt < 1 {
		return derivedJobResult{}, control.NewAPIError(http.StatusBadRequest, "invalid_request", "job ID and positive lease attempt are required", nil)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return derivedJobResult{}, err
	}
	defer tx.Rollback()
	var workspace, status, owner, lastCode, kind, entityID, target string
	var entityVersion int64
	var payload []byte
	var attempts int
	var leaseUntil sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT workspace_id,status,lease_owner,lease_until,attempts,last_error_code,kind,entity_id,entity_version,target,payload FROM memory_jobs WHERE id=?`, id).Scan(&workspace, &status, &owner, &leaseUntil, &attempts, &lastCode, &kind, &entityID, &entityVersion, &target, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return derivedJobResult{}, control.NewAPIError(http.StatusNotFound, "not_found", "memory job not found", nil)
	}
	if err != nil {
		return derivedJobResult{}, err
	}
	if !exactRunnerHasWorkspace(p, workspace) || !exactRunnerHasScope(p, derivedCompleteScope) {
		return derivedJobResult{}, control.NewAPIError(http.StatusForbidden, "forbidden", "runner cannot complete this workspace job", nil)
	}
	if status == "done" && attempts == attempt && owner == p.RunnerID && complete {
		return derivedJobResult{ID: id, Status: "done", Disposition: lastCode}, nil
	}
	if attempts != attempt || owner != p.RunnerID || status != "leased" || !leaseUntil.Valid {
		return derivedJobResult{}, control.NewAPIError(http.StatusConflict, "lease_lost", "memory job lease is no longer current", nil)
	}
	deadline, parseErr := time.Parse(time.RFC3339Nano, leaseUntil.String)
	if parseErr != nil || !deadline.After(time.Now().UTC()) {
		return derivedJobResult{}, control.NewAPIError(http.StatusConflict, "lease_expired", "memory job lease expired", nil)
	}
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	if kind == "delete_cleanup" && target == "source_ledger" {
		disposition, settleErr := settleSourceLedgerJobTx(ctx, tx, workspace, id, entityID, entityVersion, attempt, nowText)
		if settleErr != nil {
			return derivedJobResult{}, settleErr
		}
		var settledStatus string
		if err = tx.QueryRowContext(ctx, `SELECT status FROM memory_jobs WHERE id=?`, id).Scan(&settledStatus); err != nil {
			return derivedJobResult{}, err
		}
		if err = refreshDeletionCheckpointTx(ctx, tx, target, nowText); err != nil {
			return derivedJobResult{}, err
		}
		if err = tx.Commit(); err != nil {
			return derivedJobResult{}, err
		}
		return derivedJobResult{ID: id, Status: settledStatus, Disposition: disposition}, nil
	}
	if complete {
		job := derivedJob{ID: id, WorkspaceID: workspace, Kind: kind, EntityID: entityID, EntityVersion: entityVersion, Target: target, Payload: payload, Attempt: attempt}
		_, disposition, err := s.materializeTx(ctx, tx, &job)
		if err != nil {
			return derivedJobResult{}, err
		}
		if disposition == "stale_generation" {
			if err := s.repairStaleEffectTx(ctx, tx, workspace, kind, entityID, entityVersion, target, nowText); err != nil {
				return derivedJobResult{}, err
			}
		}
		state := "done"
		if disposition == "deletion_tombstone_missing" {
			state = "blocked"
		}
		_, err = tx.ExecContext(ctx, `UPDATE memory_jobs SET status=?,last_error_code=?,updated_at=? WHERE id=? AND status='leased' AND lease_owner=? AND attempts=?`, state, disposition, nowText, id, p.RunnerID, attempt)
		if err != nil {
			return derivedJobResult{}, err
		}
		if kind == "delete_cleanup" {
			if err = refreshDeletionCheckpointTx(ctx, tx, target, nowText); err != nil {
				return derivedJobResult{}, err
			}
		}
		if err = tx.Commit(); err != nil {
			return derivedJobResult{}, err
		}
		return derivedJobResult{ID: id, Status: state, Disposition: disposition}, nil
	}
	state := "retry"
	available := now.Add(retryDelay(s.cfg, attempt))
	if permanent || attempt >= s.cfg.MaxAttempts {
		state = "blocked"
		available = now
	}
	_, err = tx.ExecContext(ctx, `UPDATE memory_jobs SET status=?,available_at=?,last_error_code=?,updated_at=? WHERE id=? AND status='leased' AND lease_owner=? AND attempts=?`, state, available.Format(time.RFC3339Nano), code, nowText, id, p.RunnerID, attempt)
	if err != nil {
		return derivedJobResult{}, err
	}
	if kind == "delete_cleanup" {
		if err = refreshDeletionCheckpointTx(ctx, tx, target, nowText); err != nil {
			return derivedJobResult{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return derivedJobResult{}, err
	}
	return derivedJobResult{ID: id, Status: state}, nil
}

func refreshDeletionCheckpointTx(ctx context.Context, tx *sql.Tx, target, now string) error {
	var head int64
	if err := tx.QueryRowContext(ctx, `SELECT deletion_sequence FROM zoe_instance_state WHERE singleton=1`).Scan(&head); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_jobs j JOIN memory_deletion_events e ON e.workspace_id=j.workspace_id AND e.claim_id=j.entity_id AND e.revision=j.entity_version WHERE j.kind='delete_cleanup' AND j.target=? AND e.instance_deletion_sequence<=? AND j.status!='done'`, target, head).Scan(&pending); err != nil {
		return err
	}
	lastError := ""
	completed := head
	if pending > 0 {
		completed = 0
		lastError = "cleanup_pending"
		var blocked int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_jobs j JOIN memory_deletion_events e ON e.workspace_id=j.workspace_id AND e.claim_id=j.entity_id AND e.revision=j.entity_version WHERE j.kind='delete_cleanup' AND j.target=? AND e.instance_deletion_sequence<=? AND j.status='blocked' AND j.last_error_code='adapter_missing'`, target, head).Scan(&blocked); err != nil {
			return err
		}
		if blocked > 0 {
			lastError = "adapter_missing"
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO zoe_deletion_reconciliation_checkpoints(target,completed_sequence,updated_at,last_error) VALUES(?,?,?,?) ON CONFLICT(target) DO UPDATE SET completed_sequence=excluded.completed_sequence,updated_at=excluded.updated_at,last_error=excluded.last_error`, target, completed, now, lastError)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return nil
	}
	return err
}

// repairStaleEffectTx schedules the current generation again after a worker
// reports that it completed an external effect for an obsolete generation.
// Validation happens before the effect, so a concurrent authority write can
// still land between validation and the external store operation.
func (s *derivedService) repairStaleEffectTx(ctx context.Context, tx *sql.Tx, workspace, kind, entityID string, entityVersion int64, target, now string) error {
	var currentRevision int64
	var status string
	err := tx.QueryRowContext(ctx, `SELECT revision,status FROM memory_claims WHERE workspace_id=? AND id=?`, workspace, entityID).Scan(&currentRevision, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == string(ClaimTombstoned) {
		cleanupTarget := ""
		switch kind {
		case "index":
			cleanupTarget = "qdrant"
		case "extract":
			cleanupTarget = "mem0"
		case "delete_cleanup":
			if target == "qdrant" || target == "mem0" {
				cleanupTarget = target
			}
		}
		if cleanupTarget == "" {
			return nil
		}
		if err := insertDerivedJob(ctx, tx, workspace, "delete_cleanup", entityID, currentRevision, cleanupTarget, map[string]any{"claim_id": entityID, "target": cleanupTarget}, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE memory_jobs SET id=?,status='pending',attempts=0,available_at=?,lease_owner='',lease_until=NULL,last_error_code='',updated_at=?
WHERE workspace_id=? AND kind='delete_cleanup' AND entity_id=? AND entity_version=? AND target=?`, uuid.NewString(), now, now, workspace, entityID, currentRevision, cleanupTarget)
		return err
	}
	if kind == "delete_cleanup" && target == "qdrant" && status != string(ClaimTombstoned) && currentRevision > entityVersion && (status == string(ClaimActive) || status == string(ClaimConflicted)) {
		if err := insertDerivedJob(ctx, tx, workspace, "index", entityID, currentRevision, "qdrant", map[string]any{"claim_id": entityID, "revision": currentRevision}, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE memory_jobs SET id=?,status='pending',attempts=0,available_at=?,lease_owner='',lease_until=NULL,last_error_code='',updated_at=?
WHERE workspace_id=? AND kind='index' AND entity_id=? AND entity_version=? AND target='qdrant'`, uuid.NewString(), now, now, workspace, entityID, currentRevision)
		return err
	}
	if kind != "index" || currentRevision <= entityVersion || status != string(ClaimActive) && status != string(ClaimConflicted) {
		return nil
	}
	if err := insertDerivedJob(ctx, tx, workspace, "index", entityID, currentRevision, "qdrant", map[string]any{"claim_id": entityID, "revision": currentRevision}, now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE memory_jobs SET id=?,status='pending',attempts=0,available_at=?,lease_owner='',lease_until=NULL,last_error_code='',updated_at=?
WHERE workspace_id=? AND kind='index' AND entity_id=? AND entity_version=? AND target='qdrant'`, uuid.NewString(), now, now, workspace, entityID, currentRevision)
	return err
}

func retryDelay(c DerivedConfig, attempt int) time.Duration {
	delay := c.RetryBase
	for n := 1; n < attempt && delay < c.RetryMax/2; n++ {
		delay *= 2
	}
	if delay > c.RetryMax {
		return c.RetryMax
	}
	return delay
}

func exactRunnerHasScope(p contracts.RunnerPrincipal, scope string) bool {
	for _, candidate := range p.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

func exactRunnerHasWorkspace(p contracts.RunnerPrincipal, workspace string) bool {
	for _, candidate := range p.WorkspaceIDs {
		if candidate == workspace {
			return true
		}
	}
	return false
}

func exactRunnerWorkspaces(p contracts.RunnerPrincipal) []string {
	var result []string
	for _, workspace := range p.WorkspaceIDs {
		if workspace != "" && workspace != "*" {
			result = append(result, workspace)
		}
	}
	sort.Strings(result)
	return result
}
