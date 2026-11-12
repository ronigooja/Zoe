package voice

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func TestRegisterOpeningReadsStoredContextWithoutModel(t *testing.T) {
	f := newRegisterFixture(t)
	f.session("opening-history", "default", "owner")
	f.turn("opening-history", 1, "user", "the lunar mission", f.now)
	svc := newService(f.db, Config{ContextRegister: f.r, Now: func() time.Time { return f.now }, OpeningContextBudget: 200, UpstreamRoute: "avas"})
	defer svc.Stop()
	opening := svc.buildAVASOpeningContext(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "owner"})
	var text string
	for _, item := range opening.Items {
		text += item.Text
	}
	if !strings.Contains(text, "the lunar mission") || openingCharsFor(opening) > 200 || f.calls != 0 {
		t.Fatalf("opening=%q calls=%d", text, f.calls)
	}
}
