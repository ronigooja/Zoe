package control

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestCommandRejectionLog(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewServer(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterCommand("voice.turn.handoff", func(context.Context, *sql.Tx, CommandContext, contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error) {
		return contracts.CommandResult{}, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var rejection error = apiError(http.StatusConflict, "voice_generation_stale", "sentinel-private-message", nil)
	if err := server.RegisterCommandPreflight("voice.turn.handoff", func(context.Context, contracts.Principal, contracts.CommandRequest) error {
		return rejection
	}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	oldOutput, oldFlags, oldLevel := log.Writer(), log.Flags(), logging.CurrentLevel()
	log.SetOutput(&logs)
	log.SetFlags(0)
	logging.SetLevel(logging.Info)
	t.Cleanup(func() { log.SetOutput(oldOutput); log.SetFlags(oldFlags); logging.SetLevel(oldLevel) })

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{rejection, http.StatusConflict, "voice_generation_stale"},
		{errors.New("sentinel-private-error"), http.StatusInternalServerError, "internal"},
	} {
		rejection = tc.err
		logs.Reset()
		r := httptest.NewRequest(http.MethodPost, "/v1/commands", strings.NewReader(`{"kind":"voice.turn.handoff","payload":{"text":"sentinel-private-payload"}}`))
		r.Header.Set("Idempotency-Key", "key")
		r = r.WithContext(context.WithValue(r.Context(), devicePrincipalKey{}, contracts.Principal{Scopes: []string{"client:command"}}))
		w := httptest.NewRecorder()
		server.handleCommand(w, r)
		if w.Code != tc.status {
			t.Fatalf("status = %d, want %d", w.Code, tc.status)
		}
		if got := logs.String(); got != fmt.Sprintf("level=info command_rejected kind=voice.turn.handoff status=%d code=%s\n", tc.status, tc.code) || strings.Contains(got, "sentinel") {
			t.Fatalf("unexpected log: %q", got)
		}
	}
}

func TestCommandLogSafe(t *testing.T) {
	if got := commandLogSafe("x \n=中" + strings.Repeat("a", 70)); got != "x____"+strings.Repeat("a", 59) {
		t.Fatalf("unexpected sanitized value %q", got)
	}
}
