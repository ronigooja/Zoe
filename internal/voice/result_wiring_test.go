package voice

import (
	"context"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func TestResultWiringDisabledAndOpeningAvailability(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "Full authoritative result, not a summary.")
	r.run(t)
	status, _, _ := r.row(t, "entry-1")
	if status != "pending" || r.posts.Load() != 0 {
		t.Fatalf("default sent status=%s posts=%d", status, r.posts.Load())
	}
	r.seedCall(t, "opening", "user-1", 1)
	r.svc.upstreamRoute = "avas"
	r.svc.openingBudget = 10000
	principal := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1"}
	opening := r.svc.buildAVASOpeningContext(context.Background(), principal)
	if len(opening.PendingResults) != 1 || opening.PendingResults[0].EntryID != "entry-1" {
		t.Fatalf("pending=%+v", opening.PendingResults)
	}
	receipt := &openingContextResult{Applied: true, Silent: true, Version: opening.Version, ContentHash: "wrong"}
	if err := r.svc.acknowledgeOpeningResults(context.Background(), principal, "opening", 1, opening, receipt); err != nil {
		t.Fatal(err)
	}
	var n int
	r.store.DB().QueryRow(`SELECT count(*) FROM voice_result_availability`).Scan(&n)
	if n != 0 {
		t.Fatal("wrong receipt accepted")
	}
	receipt.ContentHash = openingContextHash(opening)
	if err := r.svc.acknowledgeOpeningResults(context.Background(), principal, "opening", 1, opening, receipt); err != nil {
		t.Fatal(err)
	}
	r.store.DB().QueryRow(`SELECT count(*) FROM voice_result_availability WHERE entry_id='entry-1' AND session_id='opening' AND generation=1`).Scan(&n)
	if n != 1 {
		t.Fatal("availability missing")
	}
	status, _, _ = r.row(t, "entry-1")
	if status != "pending" {
		t.Fatal("availability marked delivered")
	}
	if err := r.svc.acknowledgeOpeningResults(context.Background(), principal, "opening", 2, opening, receipt); err == nil {
		t.Fatal("wrong session generation accepted")
	}
}
