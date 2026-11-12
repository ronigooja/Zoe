package memory

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ronigooja/Zoe/contracts"
)

type Proposal struct {
	ID               string    `json:"id"`
	Version          int64     `json:"version"`
	WorkspaceID      string    `json:"workspace_id"`
	SourceID         string    `json:"source_id"`
	SourceVersion    int64     `json:"source_version"`
	ExtractorVersion string    `json:"extractor_version"`
	Scope            Scope     `json:"scope"`
	Subject          string    `json:"subject"`
	Key              string    `json:"key"`
	Content          string    `json:"content"`
	Status           string    `json:"status"`
	BaselineClaimID  string    `json:"baseline_claim_id,omitempty"`
	BaselineRevision int64     `json:"baseline_revision,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

func (r *Repository) CreateProposal(ctx context.Context, runner contracts.RunnerPrincipal, req runnerProposalRequest) (Proposal, error) {
	if err := req.validate(); err != nil {
		return Proposal{}, featureError(http.StatusBadRequest, "invalid_request", err.Error(), nil)
	}
	if req.WorkspaceID == "" || !runnerHasWorkspace(runner, req.WorkspaceID) {
		return Proposal{}, ErrForbidden
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Proposal{}, err
	}
	defer tx.Rollback()
	proposal, err := r.createProposalTx(ctx, tx, req)
	if err != nil {
		return Proposal{}, err
	}
	if err := tx.Commit(); err != nil {
		return Proposal{}, err
	}
	return proposal, nil
}

// createProposalTx applies the existing source, baseline and review contract
// when a repository-owned batch needs one atomic write for several proposals.
func (r *Repository) createProposalTx(ctx context.Context, tx *sql.Tx, req runnerProposalRequest) (Proposal, error) {
	if err := req.validate(); err != nil {
		return Proposal{}, err
	}
	if err := r.replayPendingDeletionsTx(ctx, tx); err != nil {
		return Proposal{}, err
	}
	deletedSource, err := sourceHasJournalledDeletionTx(ctx, tx, r.deletionJournal, req.WorkspaceID, req.SourceID, req.SourceVersion)
	if err != nil {
		return Proposal{}, err
	}
	if deletedSource {
		return Proposal{}, ErrInvalidSource
	}
	var source Source
	var sourceScope string
	var recorded string
	err = tx.QueryRowContext(ctx, `SELECT id,version,scope_kind,scope_id,principal_id,kind,reference,content_hash,recorded_at,producer FROM memory_sources WHERE workspace_id=? AND id=? AND version=? AND deleted_at IS NULL`, req.WorkspaceID, req.SourceID, req.SourceVersion).Scan(&source.ID, &source.Version, &sourceScope, &source.Scope.ID, &source.Scope.PrincipalID, &source.Kind, &source.Reference, &source.ContentHash, &recorded, &source.Producer)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, ErrInvalidSource
	}
	if err != nil {
		return Proposal{}, err
	}
	source.Scope.Kind = ScopeKind(sourceScope)
	source.RecordedAt, _ = time.Parse(time.RFC3339Nano, recorded)
	if !sourceContains(source.Scope, req.Scope) {
		return Proposal{}, featureError(http.StatusForbidden, "scope_escalation", "proposal cannot broaden the source scope", nil)
	}
	var baselineID sql.NullString
	var baselineRevision sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT id,revision FROM memory_claims WHERE workspace_id=? AND scope_kind=? AND scope_id=? AND principal_id=? AND subject=? AND claim_key=? AND status IN ('active','conflicted') AND deleted_at IS NULL ORDER BY updated_at DESC LIMIT 1`, req.WorkspaceID, req.Scope.Kind, req.Scope.ID, req.Scope.PrincipalID, req.Subject, req.Key).Scan(&baselineID, &baselineRevision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, err
	}
	var prior Proposal
	err = tx.QueryRowContext(ctx, `SELECT id,version,workspace_id,source_id,source_version,extractor_version,proposed_scope_kind,proposed_scope_id,proposed_principal_id,subject,claim_key,content,status,coalesce(baseline_claim_id,''),coalesce(baseline_revision,0),created_at FROM memory_proposals WHERE workspace_id=? AND source_id=? AND source_version=? AND extractor_version=? AND subject=? AND claim_key=?`, req.WorkspaceID, req.SourceID, req.SourceVersion, req.ExtractorVersion, strings.TrimSpace(req.Subject), strings.TrimSpace(req.Key)).Scan(&prior.ID, &prior.Version, &prior.WorkspaceID, &prior.SourceID, &prior.SourceVersion, &prior.ExtractorVersion, &prior.Scope.Kind, &prior.Scope.ID, &prior.Scope.PrincipalID, &prior.Subject, &prior.Key, &prior.Content, &prior.Status, &prior.BaselineClaimID, &prior.BaselineRevision, &recorded)
	if err == nil {
		prior.CreatedAt, _ = time.Parse(time.RFC3339Nano, recorded)
		return prior, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, err
	}
	proposal := Proposal{ID: uuid.NewString(), Version: 1, WorkspaceID: req.WorkspaceID, SourceID: req.SourceID, SourceVersion: req.SourceVersion, ExtractorVersion: req.ExtractorVersion, Scope: req.Scope, Subject: strings.TrimSpace(req.Subject), Key: strings.TrimSpace(req.Key), Content: strings.TrimSpace(req.Content), Status: "pending", BaselineClaimID: baselineID.String, CreatedAt: time.Now().UTC()}
	if baselineRevision.Valid {
		proposal.BaselineRevision = baselineRevision.Int64
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_proposals(id,workspace_id,version,source_id,source_version,extractor_version,proposed_scope_kind,proposed_scope_id,proposed_principal_id,subject,claim_key,content,status,baseline_claim_id,baseline_revision,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, proposal.ID, proposal.WorkspaceID, proposal.Version, proposal.SourceID, proposal.SourceVersion, proposal.ExtractorVersion, proposal.Scope.Kind, proposal.Scope.ID, proposal.Scope.PrincipalID, proposal.Subject, proposal.Key, proposal.Content, proposal.Status, nullString(proposal.BaselineClaimID), nullInt64(proposal.BaselineRevision), proposal.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Proposal{}, err
	}
	return proposal, nil
}

func (r *Repository) ApproveProposalTx(ctx context.Context, tx *sql.Tx, principal contracts.Principal, commandID, proposalID string, expected int64, acceptedAt time.Time) (Claim, error) {
	if err := r.replayPendingDeletionsTx(ctx, tx); err != nil {
		return Claim{}, err
	}
	if expected < 1 {
		return Claim{}, featureError(http.StatusBadRequest, "invalid_request", "expected_version is required for proposal approval", nil)
	}
	var proposal Proposal
	var scopeKind, recorded string
	var baselineID sql.NullString
	var baselineRevision sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT id,version,workspace_id,source_id,source_version,extractor_version,proposed_scope_kind,proposed_scope_id,proposed_principal_id,subject,claim_key,content,status,baseline_claim_id,baseline_revision,created_at FROM memory_proposals WHERE id=? AND workspace_id=?`, proposalID, principal.WorkspaceID).Scan(&proposal.ID, &proposal.Version, &proposal.WorkspaceID, &proposal.SourceID, &proposal.SourceVersion, &proposal.ExtractorVersion, &scopeKind, &proposal.Scope.ID, &proposal.Scope.PrincipalID, &proposal.Subject, &proposal.Key, &proposal.Content, &proposal.Status, &baselineID, &baselineRevision, &recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, ErrNotFound
	}
	if err != nil {
		return Claim{}, err
	}
	proposal.Scope.Kind = ScopeKind(scopeKind)
	proposal.CreatedAt, _ = time.Parse(time.RFC3339Nano, recorded)
	if proposal.Status != "pending" || proposal.Version != expected {
		return Claim{}, ErrVersionConflict
	}
	allowed, err := mayWriteAccessScope(ctx, tx, principal, proposal.Scope)
	if err != nil {
		return Claim{}, err
	}
	if !mayWriteScope(principal, proposal.Scope) || !allowed {
		return Claim{}, ErrForbidden
	}
	if baselineID.Valid {
		var current int64
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM memory_claims WHERE workspace_id=? AND id=? AND deleted_at IS NULL`, principal.WorkspaceID, baselineID.String).Scan(&current, &status); err != nil || current != baselineRevision.Int64 || status != "active" && status != "conflicted" {
			return Claim{}, ErrVersionConflict
		}
	} else {
		var found int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_claims WHERE workspace_id=? AND scope_kind=? AND scope_id=? AND principal_id=? AND subject=? AND claim_key=? AND status IN ('active','conflicted') AND deleted_at IS NULL`, principal.WorkspaceID, proposal.Scope.Kind, proposal.Scope.ID, proposal.Scope.PrincipalID, proposal.Subject, proposal.Key).Scan(&found); err != nil {
			return Claim{}, err
		} else if found > 0 {
			return Claim{}, ErrVersionConflict
		}
	}
	source, err := resolveCommandSource(ctx, tx, principal, proposal.SourceID, proposal.SourceVersion)
	if err != nil {
		return Claim{}, err
	}
	confirmation := Source{ID: uuid.NewString(), Version: 1, Scope: proposal.Scope, Kind: "user_note", Reference: "command:" + commandID, RecordedAt: acceptedAt.UTC(), Producer: "user"}
	if err := insertSourceTx(ctx, tx, principal.WorkspaceID, confirmation); err != nil {
		return Claim{}, err
	}
	claim := Claim{WorkspaceID: principal.WorkspaceID, Scope: proposal.Scope, Subject: proposal.Subject, Key: proposal.Key, Content: proposal.Content, Authority: AuthorityUserConfirmed, Status: ClaimActive, Revision: 1, Sources: []Source{source, confirmation}}
	write := WriteInput{WorkspaceID: principal.WorkspaceID, ActorID: principal.SubjectID, Claim: claim, Source: source, AdditionalSources: []Source{confirmation}}
	if baselineID.Valid {
		write.Expected = baselineRevision.Int64
	}
	accepted, err := r.PutTx(ctx, tx, write)
	if err != nil {
		return Claim{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE memory_proposals SET status='accepted',version=version+1,decided_at=? WHERE id=? AND workspace_id=? AND version=? AND status='pending'`, acceptedAt.UTC().Format(time.RFC3339Nano), proposal.ID, principal.WorkspaceID, expected)
	if err != nil {
		return Claim{}, err
	}
	return accepted, nil
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func runnerHasWorkspace(p contracts.RunnerPrincipal, workspace string) bool {
	for _, id := range p.WorkspaceIDs {
		if id == workspace || id == "*" {
			return true
		}
	}
	return false
}
