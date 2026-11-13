package voice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memory"
	"github.com/ronigooja/Zoe/internal/storage"
)

func memoryScopeFixture(t *testing.T) (*memory.Repository, *Service) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	server, err := control.NewServer(store, control.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Register(server, store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := memory.NewRepository(store.DB())
	for _, owner := range []string{"owner", "other-owner"} {
		tx, err := store.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		scope := memory.Scope{Kind: memory.ScopePersonal, PrincipalID: owner}
		source := memory.Source{ID: "note-" + owner, Version: 1, Scope: scope, Kind: "user_note", Reference: "command:note-" + owner, RecordedAt: time.Now().UTC(), Producer: "user"}
		_, err = repo.PutTx(ctx, tx, memory.WriteInput{WorkspaceID: "default", ActorID: owner, Source: source, Claim: memory.Claim{WorkspaceID: "default", Scope: scope, Subject: "饮品", Key: "偏好", Content: "喜欢美式咖啡", Authority: memory.AuthorityUserConfirmed, Status: memory.ClaimActive, Revision: 1, Sources: []memory.Source{source}}})
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	assembler := server.ContextAssembler()
	svc := newService(store.DB(), Config{ContextAssembler: func(ctx context.Context, p contracts.Principal, task, owner, query string, budget int) (contracts.MemoryContext, error) {
		if len(p.Scopes) != 1 || p.Scopes[0] != "memory:read:personal" {
			t.Errorf("voice read scopes=%v", p.Scopes)
		}
		return assembler(ctx, p, task, owner, query, budget)
	}})
	t.Cleanup(svc.Stop)
	return repo, svc
}

func TestMemoryRecallDoesNotReadLegacyRepository(t *testing.T) {
	_, svc := memoryScopeFixture(t)
	text, count, status := svc.recallAnswer(context.Background(), "default", "owner", "饮品")
	if status != "worker_error" || count != 0 || text != stewardErrorSpeech {
		t.Fatalf("memory recall status=%s candidates=%d text=%q", status, count, text)
	}
}

func TestMemoryRepositoryStillRejectsMissingReadScope(t *testing.T) {
	repo, _ := memoryScopeFixture(t)
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "owner"}
	if _, err := repo.Search(context.Background(), p, "饮品", memory.SearchFilter{}); !errors.Is(err, memory.ErrForbidden) {
		t.Fatalf("scope-less search error=%v, want ErrForbidden", err)
	}
	if _, _, err := repo.AssembleContext(context.Background(), p, "", "owner", "饮品", contracts.MemoryContextDefaultBudgetTokens); !errors.Is(err, memory.ErrForbidden) {
		t.Fatalf("scope-less context error=%v, want ErrForbidden", err)
	}
}
