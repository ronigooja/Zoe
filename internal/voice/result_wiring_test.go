package voice

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestResultWiringOpeningFencesVoiceDispatch(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "Full authoritative result, not a summary.")
	ctx := context.Background()
	r.svc.consumeResultEvents(ctx)
	r.seedCall(t, "opening", "user-1", 1)
	opening := reserveTestOpening(t, r, "opening")
	if len(opening.Items) != 1 {
		t.Fatal(opening.Items)
	}
	assertExcluded := func() {
		t.Helper()
		r.svc.dispatchResults(ctx)
		adapter := &checkpointAdapter{}
		r.checkpointSend(adapter)
		if status, _, attempts := r.row(t, "entry-1"); status != "pending" || attempts != 0 || r.posts.Load() != 0 || adapter.submits != 0 {
			t.Fatalf("status=%s attempts=%d HTTP posts=%d direct submits=%d", status, attempts, r.posts.Load(), adapter.submits)
		}
		if err := r.svc.selectPendingResultForTest(ctx, openingTestOwner, "entry-1", "opening", 1); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("explicit selection = %v", err)
		}
		pending, err := pendingResultsForContext(ctx, r.store.DB(), "default", "user-1")
		if err != nil || len(pending) != 0 {
			t.Fatalf("pending=%+v error=%v", pending, err)
		}
	}
	assertExcluded()
	if err := acknowledgeTestOpening(r, "opening", opening, appliedTestOpening(opening)); err != nil {
		t.Fatal(err)
	}
	assertExcluded()
	r.exec(t, `UPDATE voice_sessions SET state='ended' WHERE id='opening'`)
	assertExcluded()
}
