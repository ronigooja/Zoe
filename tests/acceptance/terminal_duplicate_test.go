package acceptance

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/runner"
)

func terminalDuplicateFixture(t *testing.T) (*rotationRecoveryFixture, contracts.RunnerObservation, contracts.RunnerObservation) {
	t.Helper()
	f := newRotationRecoveryFixture(t, true, "")
	term := terminationObservation(f.claim, "terminal-termination", "S3")
	failure := contracts.RunnerObservation{ObservationID: "terminal-failure", RunID: f.claim.RunID, AttemptID: f.claim.AttemptID, Fence: f.claim.Fence, Kind: "failure", RuntimeSessionID: "S3", Error: "runtime failed"}
	if err := f.client.Observe(t.Context(), f.claim.RunID, failure); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Observe(t.Context(), f.claim.RunID, term); err != nil {
		t.Fatal(err)
	}
	pending, err := f.journal.PendingObservations(t.Context(), f.claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, queued := range pending {
		if err := f.journal.AcknowledgeObservation(t.Context(), queued.Body.ObservationID); err != nil {
			t.Fatal(err)
		}
	}
	return f, term, failure
}

func queueTerminalDuplicates(t *testing.T, f *rotationRecoveryFixture, observations ...contracts.RunnerObservation) {
	t.Helper()
	for _, o := range observations {
		if _, err := f.journal.QueueObservation(t.Context(), f.claim.RunID, o); err != nil {
			t.Fatal(err)
		}
	}
}

func terminalObservationCount(t *testing.T, f *rotationRecoveryFixture) int {
	t.Helper()
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM runner_observations WHERE run_id=?`, f.claim.RunID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestTerminalRunStaleDuplicatesWithKnownIDsAreAcknowledged(t *testing.T) {
	f, term, failure := terminalDuplicateFixture(t)
	queueTerminalDuplicates(t, f, term, failure)
	before := terminalObservationCount(t, f)
	if err := (&runner.Engine{Control: f.client, Journal: f.journal}).FlushPendingObservations(t.Context(), f.claim.RunID); err != nil {
		t.Fatal(err)
	}
	if got := journalPendingCount(t, f); got != 0 {
		t.Fatalf("pending=%d, want 0", got)
	}
	if got := terminalObservationCount(t, f); got != before {
		t.Fatalf("server observations=%d, want unchanged %d", got, before)
	}
}

func TestTerminalRunUnknownObservationStaysPending(t *testing.T) {
	f, _, _ := terminalDuplicateFixture(t)
	unknown := terminationObservation(f.claim, "terminal-unknown", "S3")
	queueTerminalDuplicates(t, f, unknown)
	before := terminalObservationCount(t, f)
	err := (&runner.Engine{Control: f.client, Journal: f.journal}).FlushPendingObservations(t.Context(), f.claim.RunID)
	if err == nil || !strings.Contains(err.Error(), "state_conflict") {
		t.Fatalf("flush error=%v, want state_conflict", err)
	}
	if got := journalPendingCount(t, f); got != 1 {
		t.Fatalf("pending=%d, want 1", got)
	}
	if got := terminalObservationCount(t, f); got != before {
		t.Fatalf("server observations=%d, want unchanged %d", got, before)
	}
}

func TestTerminalRunSameIDDifferentKindIsRejected(t *testing.T) {
	f, term, _ := terminalDuplicateFixture(t)
	different := term
	different.Kind = "failure"
	err := f.client.Observe(t.Context(), f.claim.RunID, different)
	if err == nil || !strings.Contains(err.Error(), "state_conflict") {
		t.Fatalf("observe error=%v, want state_conflict", err)
	}
}

func TestTerminalRunDuplicateRouteAcknowledgesKnownObservationOnly(t *testing.T) {
	f, term, _ := terminalDuplicateFixture(t)
	before := terminalObservationCount(t, f)
	post := func(o contracts.RunnerObservation) (int, string) {
		raw, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/runner/v1/runs/"+f.claim.RunID+"/observations", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("Idempotency-Key", o.ObservationID)
		response, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body)
	}
	if status, body := post(term); status != http.StatusOK {
		t.Fatalf("known duplicate status=%d body=%s", status, body)
	}
	if status, body := post(term); status != http.StatusOK {
		t.Fatalf("second known duplicate status=%d body=%s", status, body)
	}
	unknown := term
	unknown.ObservationID = "route-never-seen"
	if status, body := post(unknown); status != http.StatusConflict || !strings.Contains(body, "state_conflict") {
		t.Fatalf("unknown duplicate status=%d body=%s, want 409 state_conflict", status, body)
	}
	if got := terminalObservationCount(t, f); got != before {
		t.Fatalf("server observations=%d, want unchanged %d", got, before)
	}
}
