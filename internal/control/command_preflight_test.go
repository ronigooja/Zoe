package control

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestCommandPreflightRunsBeforeTransactionAndRejectsWithoutReceipt(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := storage.RegisterCoreMigrations(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := server.RegisterCommand("test.preflight", func(context.Context, *sql.Tx, CommandContext, contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
		called = true
		return contracts.CommandResult{}, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("preparation incomplete")
	if err := server.RegisterCommandPreflight("test.preflight", func(ctx context.Context, _ contracts.Principal, _ contracts.CommandRequest) error {
		var one int
		if err := store.DB().QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
			return err
		}
		return stop
	}); err != nil {
		t.Fatal(err)
	}
	_, err = server.ExecuteCommand(ctx, contracts.Principal{WorkspaceID: "default", SubjectID: "owner"}, "test-preflight", contracts.CommandRequest{Kind: "test.preflight"}, nil)
	if !errors.Is(err, stop) || called {
		t.Fatalf("err=%v handler_called=%t", err, called)
	}
	var receipts int
	if err := store.DB().QueryRow(`SELECT count(*) FROM command_receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatalf("rejected preparation wrote %d receipts", receipts)
	}
}
