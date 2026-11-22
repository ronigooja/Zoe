package voice

import (
	"bytes"
	"errors"
	"log"
	"testing"

	"github.com/ronigooja/Zoe/internal/logging"
)

func TestStaleDecisionLog(t *testing.T) {
	var logs bytes.Buffer
	oldOutput, oldFlags, oldLevel := log.Writer(), log.Flags(), logging.CurrentLevel()
	log.SetOutput(&logs)
	log.SetFlags(0)
	logging.SetLevel(logging.Info)
	t.Cleanup(func() { log.SetOutput(oldOutput); log.SetFlags(oldFlags); logging.SetLevel(oldLevel) })

	for _, branch := range []string{"snapshot_mismatch", "version_changed"} {
		logs.Reset()
		err := staleSelection(branch, "turn-id")
		if !errors.Is(err, errStaleSelection) {
			t.Fatalf("unexpected rejection: %v", err)
		}
		if got, want := logs.String(), "level=info selection_stale branch="+branch+" turn=turn-id\n"; got != want {
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
	if s.claimClassification("turn-id") {
		t.Fatal("claimed an in-flight turn")
	}
	if _, loaded := s.classifying.Load("turn-id"); !loaded {
		t.Fatal("in-flight classification guard was cleared")
	}
	if got, want := logs.String(), "level=info classify_in_flight turn=turn-id\n"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}
