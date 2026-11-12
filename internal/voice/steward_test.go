package voice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/steward"
)

type fakeSteward struct {
	decision steward.Decision
	err      error
	calls    atomic.Int32
}

func (f *fakeSteward) Choose(context.Context, string, string, string, []steward.Candidate) (steward.Choice, error) {
	f.calls.Add(1)
	return steward.Choice{Action: steward.ActionNew}, f.err
}

func TestStewardOrdinaryTaskSentencesDispatch(t *testing.T) {
	for i, text := range []string{"is it done", "is it done yet", "how is it going", "any progress", "what did we discuss yesterday", "recall last week's conversation", "what did I ask before", "remind me what we said"} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			ts, store, key := handoffHTTPServer(t)
			r := commitAndHandoff(t, ts, key, fmt.Sprint(i), text)
			var status string
			if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE id=?`, r.ResourceID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "queued" {
				t.Fatalf("ordinary sentence %q parked: status=%s, want queued", text, status)
			}
		})
	}
}

func TestStewardLegacyAskNoLongerHoldsOrdinaryTask(t *testing.T) {
	f := &fakeSteward{decision: steward.Ask}
	var posts atomic.Int32
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(200) }))
	defer gateway.Close()
	cfg := Config{Steward: f, GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client(), Now: time.Now}
	ts, store, key := handoffHTTPServerWithConfig(t, cfg)
	_, _ = store.DB().Exec(`UPDATE voice_sessions SET gateway_session_id='gateway-handoff' WHERE id='handoff-session'`)
	r := commitAndHandoff(t, ts, key, "ask", "please handle this")
	var status, interaction, question string
	if err := store.DB().QueryRow(`SELECT status,interaction_state,interaction_question FROM tasks WHERE id=?`, r.ResourceID).Scan(&status, &interaction, &question); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || interaction != "none" || question != "" {
		t.Fatalf("status=%s interaction=%s question=%s", status, interaction, question)
	}
	svc := newService(store.DB(), cfg)
	svc.dispatchFastReplies(context.Background())
	svc.dispatchFastReplies(context.Background())
	if posts.Load() != 0 {
		t.Fatalf("question delivered %d times", posts.Load())
	}
}

func TestStewardHighImpactIsOrdinaryQueuedWork(t *testing.T) {
	f := &fakeSteward{decision: steward.Dispatch}
	ts, store, key := handoffHTTPServerWithConfig(t, Config{Steward: f})
	r := commitAndHandoff(t, ts, key, "high-steward", "delete the old project")
	var status, interaction string
	_ = store.DB().QueryRow(`SELECT status,interaction_state FROM tasks WHERE id=?`, r.ResourceID).Scan(&status, &interaction)
	if status != "queued" || interaction != "none" || f.calls.Load() != 1 {
		t.Fatalf("status=%s model calls=%d", status, f.calls.Load())
	}
}

func TestStewardUnavailableCreatesNoTask(t *testing.T) {
	for _, err := range []error{errors.New("provider unavailable"), context.DeadlineExceeded} {
		ts, store, key := handoffHTTPServerWithConfig(t, Config{Steward: &fakeSteward{decision: steward.Ask, err: err}})
		commitAndHandoff(t, ts, key, "failed-steward", "what did we discuss last week")
		var tasks int
		if e := store.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks); e != nil {
			t.Fatal(e)
		}
		if tasks != 0 {
			t.Fatalf("error %v created %d tasks", err, tasks)
		}
	}
}
