// Package memory defines Zoe's authoritative, source-linked memory records.
// External extractors may propose records, but they never own these records.
package memory

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type ScopeKind string

const (
	ScopePersonal  ScopeKind = "personal"
	ScopeWorkspace ScopeKind = "workspace"
	ScopeProject   ScopeKind = "project"
	ScopeTask      ScopeKind = "task"
)

type AuthorityKind string

const (
	AuthorityUserConfirmed     AuthorityKind = "user_confirmed"
	AuthorityExecutionObserved AuthorityKind = "execution_observed"
	AuthorityImported          AuthorityKind = "imported_authoritative"
	AuthorityModelInferred     AuthorityKind = "model_inferred"
)

type ClaimStatus string

const (
	ClaimActive     ClaimStatus = "active"
	ClaimCandidate  ClaimStatus = "candidate"
	ClaimConflicted ClaimStatus = "conflicted"
	ClaimSuperseded ClaimStatus = "superseded"
	ClaimRetracted  ClaimStatus = "retracted"
	ClaimTombstoned ClaimStatus = "tombstoned"
)

// Scope is always workspace-bound. Personal claims additionally belong to a
// principal; project/task IDs are opaque Zoe IDs, never host paths.
type Scope struct {
	Kind        ScopeKind `json:"kind"`
	ID          string    `json:"id,omitempty"`
	PrincipalID string    `json:"principal_id,omitempty"`
}

func (s Scope) Validate() error {
	switch s.Kind {
	case ScopePersonal:
		if s.ID != "" || strings.TrimSpace(s.PrincipalID) == "" {
			return errors.New("personal scope requires a principal and no scope ID")
		}
	case ScopeWorkspace:
		if s.ID != "" || s.PrincipalID != "" {
			return errors.New("workspace scope must not name a narrower scope or principal")
		}
	case ScopeProject, ScopeTask:
		if strings.TrimSpace(s.ID) == "" || s.PrincipalID != "" {
			return errors.New("project/task scope requires an opaque scope ID and no principal")
		}
	default:
		return fmt.Errorf("unsupported memory scope %q", s.Kind)
	}
	return nil
}

type Source struct {
	ID             string    `json:"id"`
	Version        int64     `json:"version"`
	Scope          Scope     `json:"scope"`
	Kind           string    `json:"kind"`
	Reference      string    `json:"reference"`
	ContentHash    string    `json:"content_hash,omitempty"`
	RecordedAt     time.Time `json:"recorded_at"`
	Producer       string    `json:"producer"`
	RetentionClass string    `json:"retention_class,omitempty"`
}

func (s Source) Validate() error {
	if strings.TrimSpace(s.ID) == "" || s.Version < 1 || strings.TrimSpace(s.Kind) == "" || strings.TrimSpace(s.Reference) == "" {
		return errors.New("source requires ID, positive version, kind, and reference")
	}
	if err := s.Scope.Validate(); err != nil {
		return fmt.Errorf("source scope: %w", err)
	}
	if strings.TrimSpace(s.Producer) == "" {
		return errors.New("source requires a producer")
	}
	return nil
}

type Claim struct {
	ID          string        `json:"id"`
	WorkspaceID string        `json:"workspace_id"`
	Scope       Scope         `json:"scope"`
	Subject     string        `json:"subject"`
	Key         string        `json:"key"`
	Content     string        `json:"content"`
	Authority   AuthorityKind `json:"authority_kind"`
	Status      ClaimStatus   `json:"status"`
	Revision    int64         `json:"revision"`
	ValidFrom   *time.Time    `json:"valid_from,omitempty"`
	ValidTo     *time.Time    `json:"valid_to,omitempty"`
	Supersedes  string        `json:"supersedes,omitempty"`
	Sources     []Source      `json:"sources"`
	DeletedAt   *time.Time    `json:"deleted_at,omitempty"`
}

// ValidateForWrite rejects authority escalation. Model output can only enter
// the candidate lane; it cannot assert a user confirmation or execution fact.
func (c Claim) ValidateForWrite() error {
	return c.validateForWrite(false)
}

// validateForWrite permits active model output only for repository-owned batches.
func (c Claim) validateForWrite(batchTrusted bool) error {
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.WorkspaceID) == "" || strings.TrimSpace(c.Subject) == "" || strings.TrimSpace(c.Key) == "" || strings.TrimSpace(c.Content) == "" {
		return errors.New("claim requires ID, workspace, subject, key, and content")
	}
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if c.Revision < 1 {
		return errors.New("claim revision must be positive")
	}
	if c.DeletedAt != nil || c.Status == ClaimTombstoned {
		return errors.New("tombstones are created only by the deletion operation")
	}
	if c.Authority == AuthorityModelInferred && c.Status != ClaimCandidate && !(batchTrusted && c.Status == ClaimActive) {
		return errors.New("model-inferred claims must remain candidates")
	}
	switch c.Authority {
	case AuthorityUserConfirmed, AuthorityExecutionObserved, AuthorityImported, AuthorityModelInferred:
	default:
		return fmt.Errorf("unsupported authority kind %q", c.Authority)
	}
	if c.Status != ClaimActive && c.Status != ClaimCandidate && c.Status != ClaimConflicted && c.Status != ClaimSuperseded && c.Status != ClaimRetracted {
		return fmt.Errorf("unsupported claim status %q", c.Status)
	}
	if len(c.Sources) == 0 {
		return errors.New("claim requires at least one source")
	}
	for _, source := range c.Sources {
		if err := source.Validate(); err != nil {
			return err
		}
	}
	if c.ValidFrom != nil && c.ValidTo != nil && c.ValidTo.Before(*c.ValidFrom) {
		return errors.New("valid_to must not precede valid_from")
	}
	return nil
}

type ContextEntry struct {
	ClaimID     string        `json:"claim_id"`
	Revision    int64         `json:"revision"`
	Authority   AuthorityKind `json:"authority_kind"`
	SourceIDs   []string      `json:"source_ids"`
	ContentHash string        `json:"content_hash"`
}

type ContextManifest struct {
	ID          string         `json:"id"`
	WorkspaceID string         `json:"workspace_id"`
	TaskID      string         `json:"task_id,omitempty"`
	RecipientID string         `json:"recipient_id"`
	Scopes      []Scope        `json:"scopes"`
	Entries     []ContextEntry `json:"entries"`
	Budget      int            `json:"budget"`
	Missing     []string       `json:"missing,omitempty"`
	Conflicts   []string       `json:"conflicts,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}
