package voice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/steward"
)

type classificationStewardFunc func(context.Context, string, string, string, []steward.Candidate) (steward.Choice, error)

func (f classificationStewardFunc) Choose(ctx context.Context, text, restatement, now string, candidates []steward.Candidate) (steward.Choice, error) {
	return f(ctx, text, restatement, now, candidates)
}

type classificationRig struct {
	*deliveryRig
	routes  atomic.Int32
	choices atomic.Int32
}

func newClassificationRig(t *testing.T, routeHook func()) *classificationRig {
	t.Helper()
	r := &classificationRig{deliveryRig: newDeliveryRig(t)}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.routes.Add(1)
		if routeHook != nil {
			routeHook()
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"output_text": `{"route":"task","restatement":"none"}`})
	}))
	t.Cleanup(model.Close)
	register, err := NewRegister(r.store.DB(), RegisterConfig{
		Model:             RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"},
		CompactModel:      RegisterModelConfig{BaseURL: model.URL, Model: "fixture", APIKey: "fixture"},
		CompactCharacters: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.svc = newService(r.store.DB(), Config{ContextRegister: register, Now: r.now,
		Steward: classificationStewardFunc(func(context.Context, string, string, string, []steward.Candidate) (steward.Choice, error) {
			r.choices.Add(1)
			return steward.Choice{Action: "new"}, nil
		}),
	})
	r.seedCall(t, "classification-session", "user-1", 1)
	return r
}

func (r *classificationRig) seedTurn(t *testing.T, id string) {
	t.Helper()
	r.exec(t, `INSERT INTO voice_turns(id,workspace_id,session_id,generation,turn_id,revision,content,content_hash,confirmed_at,command_id) VALUES(?,'default','classification-session',1,?,1,'PRIVATE_SENTENCE',X'01',?,'cmd')`, id, id, r.now().Format(time.RFC3339Nano))
}

func (r *classificationRig) assertResult(t *testing.T, id, route, action string) {
	t.Helper()
	var gotRoute, gotAction string
	if err := r.store.DB().QueryRow(`SELECT triage_category,steward_action FROM voice_turns WHERE id=?`, id).Scan(&gotRoute, &gotAction); err != nil {
		t.Fatal(err)
	}
	if gotRoute != route || gotAction != action {
		t.Fatalf("result=(%q,%q), want (%q,%q)", gotRoute, gotAction, route, action)
	}
}

func waitClassification(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("classification did not reach synchronization point")
	}
}

func classifyAsync(s *Service, id string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.classifyCommittedTurn(id)
	}()
	return done
}

func TestClassificationSingleFlightSameRow(t *testing.T) {
	started, release := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	r := newClassificationRig(t, func() { started <- struct{}{}; <-release })
	defer unblock()
	r.seedTurn(t, "same-row")
	first := classifyAsync(r.svc, "same-row")
	waitClassification(t, started)
	// The overlapping caller must return while the first model call is blocked.
	waitClassification(t, classifyAsync(r.svc, "same-row"))
	unblock()
	waitClassification(t, first)
	r.svc.classifyCommittedTurn("same-row")
	if r.routes.Load() != 1 || r.choices.Load() != 1 {
		t.Fatalf("Route=%d Choose=%d, want one each", r.routes.Load(), r.choices.Load())
	}
	r.assertResult(t, "same-row", "task", "new")
}

func TestClassificationSingleFlightDifferentRows(t *testing.T) {
	started, release := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	r := newClassificationRig(t, nil)
	defer unblock()
	// Block inside Route before the model client's own request mutex, so this
	// tests classification concurrency rather than model-client serialization.
	r.svc.register.keys = func(context.Context, RegisterOwner) ([]MemoryMappingKey, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	}
	r.seedTurn(t, "first-row")
	r.seedTurn(t, "second-row")
	first := classifyAsync(r.svc, "first-row")
	waitClassification(t, started)
	second := classifyAsync(r.svc, "second-row")
	waitClassification(t, started)
	unblock()
	waitClassification(t, first)
	waitClassification(t, second)
	if r.routes.Load() != 2 || r.choices.Load() != 2 {
		t.Fatalf("Route=%d Choose=%d, want two each", r.routes.Load(), r.choices.Load())
	}
	r.assertResult(t, "first-row", "task", "new")
	r.assertResult(t, "second-row", "task", "new")
}

func TestClassificationSingleFlightRetryAfterFailure(t *testing.T) {
	r := newClassificationRig(t, nil)
	r.seedTurn(t, "retry-row")
	var attempts int
	r.svc.register.keys = func(context.Context, RegisterOwner) ([]MemoryMappingKey, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("fixture register failure")
		}
		return nil, nil
	}
	// Routing errors normally persist an error sentinel. A storage failure leaves
	// the row eligible for a retry without changing that fail-closed behavior.
	r.exec(t, `CREATE TRIGGER classification_write_failure BEFORE UPDATE ON voice_turns BEGIN SELECT RAISE(FAIL,'fixture storage failure'); END`)
	r.svc.classifyCommittedTurn("retry-row")
	if _, loaded := r.svc.classifying.Load("retry-row"); loaded {
		t.Fatal("failed classification retained registry entry")
	}
	r.exec(t, `DROP TRIGGER classification_write_failure`)
	r.svc.classifyCommittedTurn("retry-row")
	if attempts != 2 || r.routes.Load() != 1 || r.choices.Load() != 1 {
		t.Fatalf("attempts=%d Route=%d Choose=%d", attempts, r.routes.Load(), r.choices.Load())
	}
	r.assertResult(t, "retry-row", "task", "new")
}

func TestClassificationSingleFlightReleasesOnPanicAndEarlyReturn(t *testing.T) {
	r := newClassificationRig(t, nil)
	r.svc.classifyCommittedTurn("missing-row")
	if _, loaded := r.svc.classifying.Load("missing-row"); loaded {
		t.Fatal("early return retained registry entry")
	}
	r.seedTurn(t, "missing-row")
	r.svc.classifyCommittedTurn("missing-row")
	r.assertResult(t, "missing-row", "task", "new")
	r.seedTurn(t, "panic-row")
	r.svc.register.keys = func(context.Context, RegisterOwner) ([]MemoryMappingKey, error) { panic("fixture panic") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected register panic")
			}
		}()
		r.svc.classifyCommittedTurn("panic-row")
	}()
	if _, loaded := r.svc.classifying.Load("panic-row"); loaded {
		t.Fatal("panic retained registry entry")
	}
	r.svc.register.keys = nil
	r.svc.classifyCommittedTurn("panic-row")
	r.assertResult(t, "panic-row", "task", "new")
}

func TestClassificationSingleFlightDiscardedLogs(t *testing.T) {
	r := newClassificationRig(t, nil)
	logs := captureRegisterLogs(t)
	r.seedTurn(t, "discard-row")
	r.svc.steward = classificationStewardFunc(func(context.Context, string, string, string, []steward.Candidate) (steward.Choice, error) {
		// Simulate another process persisting after our SELECT but before UPDATE.
		r.exec(t, `UPDATE voice_turns SET triage_category='chat',steward_action='' WHERE id='discard-row'`)
		return steward.Choice{Action: "new"}, nil
	})
	r.svc.classifyCommittedTurn("discard-row")
	text := logs.String()
	if !strings.Contains(text, "level=warn voice_routing_discarded id=discard-row reason=already_classified") {
		t.Fatalf("missing discarded WARN: %s", text)
	}
	if strings.Contains(text, "voice_routing id=discard-row") || strings.Contains(text, "voice_routing_failure id=discard-row") || strings.Contains(text, "PRIVATE_SENTENCE") {
		t.Fatalf("misleading or private discarded log: %s", text)
	}
	r.assertResult(t, "discard-row", "chat", "")
	logs.Reset()
	r.seedTurn(t, "winning-row")
	r.svc.steward = classificationStewardFunc(func(context.Context, string, string, string, []steward.Candidate) (steward.Choice, error) {
		return steward.Choice{Action: "new"}, nil
	})
	r.svc.classifyCommittedTurn("winning-row")
	if !strings.Contains(logs.String(), "level=info voice_routing id=winning-row failed=false storage_error=false") {
		t.Fatalf("winning success log changed: %s", logs.String())
	}
	logs.Reset()
	r.seedTurn(t, "failed-row")
	r.svc.register.keys = func(context.Context, RegisterOwner) ([]MemoryMappingKey, error) { return nil, errors.New("fixture") }
	r.svc.classifyCommittedTurn("failed-row")
	if !strings.Contains(logs.String(), "voice_routing id=failed-row failed=true storage_error=false") || !strings.Contains(logs.String(), "level=error voice_routing_failure id=failed-row error_kind=routing_or_storage") {
		t.Fatalf("winning error logs changed: %s", logs.String())
	}
	r.assertResult(t, "failed-row", "error", "")
}
