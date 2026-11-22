package voice

import (
	"context"
	"strings"
	"testing"
)

func appliedTestOpening(opening *openingContext) *openingContextResult {
	return &openingContextResult{Applied: true, Silent: true, Version: opening.Version, ContentHash: openingContextHash(opening)}
}

func acknowledgeTestOpening(r *deliveryRig, session string, opening *openingContext, receipt *openingContextResult) error {
	ctx := context.Background()
	tx, err := r.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.svc.acknowledgeOpeningResults(ctx, tx, openingTestOwner, session, opening, receipt); err != nil {
		return err
	}
	return tx.Commit()
}

func TestOpeningReceiptEventAtomicAndIdempotent(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "full")
	r.seedTask(t, "task-2", "user-1")
	r.seedEntry(t, "task-2", "entry-2", "second result")
	r.seedEvent(t, 2, "task-2", "run-2", "entry-2")
	r.exec(t, `UPDATE tasks SET summary_result='sent summary' WHERE id='task-1'`)
	r.svc.consumeResultEvents(context.Background())
	r.seedCall(t, "opening", "user-1", 1)
	opening := reserveTestOpening(t, r, "opening")
	r.exec(t, `UPDATE tasks SET summary_result='changed after send' WHERE id='task-1'`)
	r.exec(t, `CREATE TRIGGER fail_second BEFORE INSERT ON voice_context_events WHEN NEW.source_id='entry-2' BEGIN SELECT RAISE(ABORT, 'fixture'); END;`)
	receipt := appliedTestOpening(opening)
	if err := acknowledgeTestOpening(r, "opening", opening, receipt); err == nil {
		t.Fatal("event failure did not abort acknowledgement")
	}
	var reserved, events int
	if err := r.store.DB().QueryRow(`SELECT (SELECT count(*) FROM voice_result_opening WHERE state='reserved'),(SELECT count(*) FROM voice_context_events)`).Scan(&reserved, &events); err != nil || reserved != 2 || events != 0 {
		t.Fatalf("reserved=%d events=%d err=%v", reserved, events, err)
	}
	r.exec(t, `DROP TRIGGER fail_second`)
	for i := 0; i < 2; i++ {
		if err := acknowledgeTestOpening(r, "opening", opening, receipt); err != nil {
			t.Fatal(err)
		}
	}
	var consumed int
	var text string
	if err := r.store.DB().QueryRow(`SELECT (SELECT count(*) FROM voice_result_opening WHERE state='consumed' AND consumed_at IS NOT NULL),(SELECT count(*) FROM voice_context_events),(SELECT text FROM voice_context_events WHERE source_kind='task_result' AND source_id='entry-1' AND session_id='opening' AND role='background')`).Scan(&consumed, &events, &text); err != nil || consumed != 2 || events != 2 || text != opening.Items[0].Text {
		t.Fatalf("consumed=%d events=%d text=%q err=%v", consumed, events, text, err)
	}
}

func TestRejectLegacyVoiceRouteBeforeRegistration(t *testing.T) {
	for _, route := range []string{"wm", "bogus"} {
		err := Register(nil, nil, Config{UpstreamRoute: route})
		if err == nil || !strings.Contains(err.Error(), "AVAS only") {
			t.Fatalf("route %q: expected actionable AVAS-only error, got %v", route, err)
		}
	}
}
