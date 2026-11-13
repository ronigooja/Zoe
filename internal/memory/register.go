package memory

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
)

func Register(s *control.Server, store *storage.Store) error {
	if s == nil || store == nil {
		return errorsf("memory registration requires control server and storage")
	}
	if err := RegisterMigrations(store); err != nil {
		return err
	}
	repository := NewRepositoryWithStore(store)
	service := &Service{repo: repository, db: store.DB()}
	if err := s.RegisterContextAssembler(func(ctx context.Context, p contracts.Principal, taskID, recipientID, query string, budget int) (contracts.MemoryContext, error) {
		manifest, claims, err := repository.AssembleContext(ctx, p, taskID, recipientID, query, budget)
		if err != nil {
			return contracts.MemoryContext{}, err
		}
		out := contracts.MemoryContext{Manifest: contracts.MemoryContextManifest{ID: manifest.ID, WorkspaceID: manifest.WorkspaceID, TaskID: manifest.TaskID, RecipientID: manifest.RecipientID, Budget: manifest.Budget, Missing: manifest.Missing, Conflicts: manifest.Conflicts}}
		out.Manifest.Entries = make([]contracts.MemoryContextEntry, 0, len(manifest.Entries))
		for _, entry := range manifest.Entries {
			out.Manifest.Entries = append(out.Manifest.Entries, contracts.MemoryContextEntry{ClaimID: entry.ClaimID, Revision: entry.Revision, SourceIDs: entry.SourceIDs, ContentHash: entry.ContentHash})
		}
		out.Items = make([]contracts.MemoryContextItem, 0, len(claims))
		for _, claim := range claims {
			out.Items = append(out.Items, contracts.MemoryContextItem{Subject: claim.Subject, Key: claim.Key, Content: claim.Content})
		}
		return out, nil
	}); err != nil {
		return err
	}
	for kind, handler := range map[string]control.CommandHandler{
		"memory.remember":         service.commandRemember,
		"memory.delete":           service.commandDelete,
		"memory.proposal.approve": service.commandApproveProposal,
	} {
		if err := s.RegisterCommand(kind, handler); err != nil {
			return err
		}
	}
	if err := s.RegisterAuthenticatedRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/memory", service.handleSearch)
		mux.HandleFunc("GET /v1/memory/context", service.handleContext)
	}); err != nil {
		return err
	}
	if err := s.RegisterRunnerRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("POST /runner/v1/memory/proposals", service.handleRunnerProposal)
	}); err != nil {
		return err
	}
	return nil
}

type Service struct {
	repo *Repository
	db   *sql.DB
}

func errorsf(message string) error { return &registrationError{message: message} }

type registrationError struct{ message string }

func (e *registrationError) Error() string { return e.message }
