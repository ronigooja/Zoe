package voice

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/contracts"
)

func TestRegisterOpeningReadsStoredContextWithoutModel(t *testing.T) {
	f := newRegisterFixture(t)
	f.session("opening-history", "owner")
	f.turn("opening-history", 1, "user", "the lunar mission", f.now)
	svc := newService(f.db, Config{ContextRegister: f.r, Now: func() time.Time { return f.now }, Limits: mustLimits(t, "opening_context_runes", 200), UpstreamRoute: "avas"})
	defer svc.Stop()
	text := svc.openingRegisterText(context.Background(), contracts.Principal{SubjectID: "owner"})
	if text != "user: the lunar mission\n" || f.calls != 0 {
		t.Fatalf("opening=%q calls=%d", text, f.calls)
	}
}

func TestRegisterFirstSharedRuneBudget(t *testing.T) {
	r := checkpointRig(t)
	r.setup(t, "短答案")
	r.seedTask(t, "oversized", "user-1")
	r.seedEntry(t, "oversized", "entry-0", strings.Repeat("界", 200))
	r.seedEvent(t, 2, "oversized", "run-0", "entry-0")
	r.svc.consumeResultEvents(context.Background())
	r.seedCall(t, "opening", "user-1", 1)
	f := newRegisterFixture(t)
	f.session("history", "user-1")
	f.turn("history", 1, "user", "之前聊过月球", f.now)
	r.svc.register = f.r
	r.svc.now = func() time.Time { return f.now }
	register := r.svc.openingRegisterText(context.Background(), openingTestOwner)
	result := "Task result:\nGoal: goal\nStatus: awaiting_user\nResult: 短答案"
	budget := utf8.RuneCountInString(register + result)
	r.svc.limits = mustLimits(t, "opening_context_runes", int64(budget))
	opening := reserveTestOpening(t, r, "opening")
	if register == "" || len(opening.Items) != 2 || opening.Items[0].Text != register || opening.Items[1].Text != result || openingCharsFor(opening) != budget {
		t.Fatalf("opening=%+v budget=%d register=%q", opening, budget, register)
	}
	pending, err := pendingResultsForContext(context.Background(), r.store.DB(), "user-1")
	if err != nil || len(pending) != 1 || pending[0].EntryID != "entry-0" {
		t.Fatalf("oversized must remain unreserved and pending: %+v, %v", pending, err)
	}
}
