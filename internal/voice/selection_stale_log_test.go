package voice

import (
	"bytes"
	"context"
	"errors"
	"log"
	"testing"

	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/logging"
)

func TestStaleDecisionLog(t *testing.T) {
	var logs bytes.Buffer
	oldOutput, oldFlags, oldLevel := log.Writer(), log.Flags(), logging.CurrentLevel()
	log.SetOutput(&logs)
	log.SetFlags(0)
	logging.SetLevel(logging.Info)
	t.Cleanup(func() { log.SetOutput(oldOutput); log.SetFlags(oldFlags); logging.SetLevel(oldLevel) })

	s := &Service{}
	for _, tc := range []struct {
		versionChanged bool
		branch         string
	}{
		{false, "stale_decision"},
		{true, "version_changed"},
	} {
		logs.Reset()
		err := s.staleDecisionTx(context.Background(), nil, "", handoffPayload{ContentHash: "sentinel-private-hash"}, "turn-id", tc.versionChanged)
		var apiErr *control.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 409 || apiErr.Code != "selection_stale" {
			t.Fatalf("unexpected rejection: %v", err)
		}
		if got, want := logs.String(), "level=info selection_stale branch="+tc.branch+" turn=turn-id\n"; got != want {
			t.Fatalf("log = %q, want %q", got, want)
		}
	}
}

func TestClassificationInFlightLog(t *testing.T) {
	var logs bytes.Buffer
	oldOutput, oldFlags, oldLevel := log.Writer(), log.Flags(), logging.CurrentLevel()
	log.SetOutput(&logs)
	log.SetFlags(0)
	logging.SetLevel(logging.Info)
	t.Cleanup(func() { log.SetOutput(oldOutput); log.SetFlags(oldFlags); logging.SetLevel(oldLevel) })

	s := &Service{}
	s.classifying.Store("turn-id", struct{}{})
	s.classifyCommittedTurn("turn-id")
	if _, loaded := s.classifying.Load("turn-id"); !loaded {
		t.Fatal("in-flight classification guard was cleared")
	}
	if got, want := logs.String(), "level=info classify_in_flight turn=turn-id\n"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}
