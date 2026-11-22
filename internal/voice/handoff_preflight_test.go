package voice

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/steward"
)

func (r *classificationRig) prepareTestHandoff() error {
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user-1", DeviceID: "device-1", Scopes: []string{"client:command"}}
	return r.svc.prepareHandoff(context.Background(), p, contracts.CommandRequest{Payload: resolvedRequestJSON(handoffPayload{
		SessionID: "classification-session", Generation: 1, TurnID: "handoff", Revision: 1, ContentHash: "01" + strings.Repeat("00", 31),
	})})
}

func newHandoffPreflightRig(t *testing.T) *classificationRig {
	t.Helper()
	r := newClassificationRig(t, nil)
	r.seedTurn(t, "handoff")
	hash := [32]byte{1}
	r.exec(t, `UPDATE voice_turns SET content_hash=? WHERE id='handoff'`, hash[:])
	return r
}

func assertHandoffPreflightError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiErr *control.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != status || apiErr.Code != code {
		t.Fatalf("preflight err=%v, want %d %s", err, status, code)
	}
}

func TestHandoffPreflightWaitsForSteward(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "ready"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			r := newHandoffPreflightRig(t)
			logs := captureRegisterLogs(t)
			if timeout {
				r.svc.recall.ClassificationTimeout = 200 * time.Millisecond
			}
			started, release := make(chan struct{}), make(chan struct{})
			r.svc.steward = classificationStewardFunc(func(context.Context, steward.Input) (steward.Choice, error) {
				if r.choices.Add(1) == 1 {
					close(started)
					<-release
				}
				return steward.Choice{Action: steward.ActionNew}, nil
			})
			classificationDone := classifyAsync(r.svc, "handoff")
			unblock := func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}
			defer func() { unblock(); waitClassification(t, classificationDone) }()
			waitClassification(t, started)
			r.assertResult(t, "handoff", "task", "")
			if timeout {
				assertHandoffPreflightError(t, r.prepareTestHandoff(), http.StatusServiceUnavailable, "routing_timeout")
			} else {
				done := make(chan error, 1)
				go func() { done <- r.prepareTestHandoff() }()
				select {
				case err := <-done:
					t.Fatalf("preflight returned while Steward was blocked: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
				unblock()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("preflight did not return after Steward completed")
				}
				waitClassification(t, classificationDone)
				r.assertResult(t, "handoff", "task", "new")
			}
			if r.routes.Load() != 1 || r.choices.Load() != 1 {
				t.Fatalf("Route=%d Choose=%d, want one each", r.routes.Load(), r.choices.Load())
			}
			if text := logs.String(); strings.Contains(text, "voice_selection_refresh") || strings.Contains(text, "refresh_exhausted") {
				t.Fatalf("waiting consumed a selection refresh: %s", text)
			}
		})
	}
}

func TestHandoffPreflightRefreshLimit(t *testing.T) {
	for _, name := range []string{"steward_error", "stale_facts"} {
		t.Run(name, func(t *testing.T) {
			r := newHandoffPreflightRig(t)
			logs := captureRegisterLogs(t)
			if name == "steward_error" {
				// A model failure normally stores an error snapshot. Block that write
				// to exercise the retry path for failures leaving the snapshot empty.
				r.exec(t, `CREATE TRIGGER reject_steward_error BEFORE UPDATE ON voice_turns WHEN NEW.steward_action='error' BEGIN SELECT RAISE(FAIL,'fixture storage failure'); END`)
				r.svc.steward = classificationStewardFunc(func(context.Context, steward.Input) (steward.Choice, error) {
					r.choices.Add(1)
					return steward.Choice{}, errors.New("fixture Steward failure")
				})
				r.svc.classifyCommittedTurn("handoff")
			} else {
				r.seedTask(t, "candidate", "user-1")
				r.svc.classifyCommittedTurn("handoff")
				// Invalidate the completed, non-empty snapshot, then invalidate
				// each replacement while Steward is selecting against it.
				r.exec(t, `UPDATE tasks SET version=version+1 WHERE id='candidate'`)
				r.svc.steward = classificationStewardFunc(func(context.Context, steward.Input) (steward.Choice, error) {
					r.choices.Add(1)
					r.exec(t, `UPDATE tasks SET version=version+1 WHERE id='candidate'`)
					return steward.Choice{Action: steward.ActionNew}, nil
				})
			}
			assertHandoffPreflightError(t, r.prepareTestHandoff(), http.StatusConflict, "selection_stale")
			if r.routes.Load() != 1 || r.choices.Load() != 4 {
				t.Fatalf("Route=%d Choose=%d, want one Register and one initial plus three Steward refreshes", r.routes.Load(), r.choices.Load())
			}
			text := logs.String()
			if strings.Count(text, "voice_selection_refresh ") != 3 || !strings.Contains(text, "selection_stale branch=refresh_exhausted turn=handoff refreshes=3") {
				t.Fatalf("missing three refreshes and exhaustion: %s", text)
			}
			if name == "stale_facts" && !strings.Contains(text, "voice_selection_refresh turn=handoff refreshes=1 accepted_empty=false equal=false") {
				t.Fatalf("missing refresh of the completed stale snapshot: %s", text)
			}
		})
	}
}

func TestHandoffPreflightKeepsCompletedStewardError(t *testing.T) {
	r := newHandoffPreflightRig(t)
	r.svc.steward = classificationStewardFunc(func(context.Context, steward.Input) (steward.Choice, error) {
		r.choices.Add(1)
		return steward.Choice{}, errors.New("fixture Steward failure")
	})
	r.svc.classifyCommittedTurn("handoff")
	if err := r.prepareTestHandoff(); err != nil {
		t.Fatal(err)
	}
	r.assertResult(t, "handoff", "task", "error")
	if r.choices.Load() != 1 {
		t.Fatalf("Choose=%d, want one", r.choices.Load())
	}
}
