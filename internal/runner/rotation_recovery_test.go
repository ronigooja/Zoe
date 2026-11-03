package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

type rotationSummarizerFake struct {
	*runtimeFake
	result string
	err    error
	calls  int
	model  string
	input  string
	block  bool
}

func (f *rotationSummarizerFake) Summarize(ctx context.Context, model, input string) (string, error) {
	f.calls++
	f.model, f.input = model, input
	if f.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return f.result, f.err
}

func rotationEngine(t *testing.T, f *rotationSummarizerFake, control RunnerAPI, j *Journal) *Engine {
	t.Helper()
	e := testEngine(t, f.runtimeFake, &controlFake{}, j)
	e.Runtime = f
	e.Control = control
	e.Routes["profile"] = Route{ProfileID: "profile", Harness: "opencode", IsolationPolicy: OpenCodeWorkspaceWritePolicy, Model: "provider/configured"}
	e.ContextTokenBudget = 1
	return e
}

func TestRotationCrashBeforeCreateReusesStoredPromptWithoutRefetch(t *testing.T) {
	msgs := rotationPromptFixture(t)
	control := &rotationContextControlFake{context: contracts.RotationContext{Instructions: []string{"fresh instruction"}}}
	f := completingRuntimeFake()
	f.messages, f.rotationMessages = msgs, rotationReplacement(msgs)
	j := openTestJournal(t)
	c := rotationClaim()
	producer := rotationEngine(t, &rotationSummarizerFake{runtimeFake: f}, control, j)
	stored := producer.buildRotationPrompt(context.Background(), c, msgs)
	if _, err := j.SaveClaim(context.Background(), c, RuntimeSpec{RunID: c.RunID, AttemptID: c.AttemptID, Fence: "1", Prompt: buildPrompt(c), Workspace: c.WorkspaceID}, "claimed"); err != nil {
		t.Fatal(err)
	}
	if err := j.SetSession(context.Background(), c.RunID, c.AttemptID, c.Fence, "session", "prompt_sent"); err != nil {
		t.Fatal(err)
	}
	if err := j.BeginRotation(context.Background(), c.RunID, c.AttemptID, c.Fence, "session", stored, 1); err != nil {
		t.Fatal(err)
	}
	newControl := &rotationContextControlFake{context: contracts.RotationContext{Instructions: []string{"must not fetch"}}}
	newRuntime := completingRuntimeFake()
	newRuntime.live = map[string]bool{"session": true}
	newRuntime.creates = 1
	newRuntime.rotationMessages = rotationReplacement(msgs)
	e := rotationEngine(t, &rotationSummarizerFake{runtimeFake: newRuntime}, newControl, j)
	if err := e.Process(testContext(t), c); err != nil {
		t.Fatal(err)
	}
	if len(newRuntime.createdSpecs) != 1 || newRuntime.createdSpecs[0].Prompt != stored {
		t.Fatalf("recovery prompt=%q, stored=%q", newRuntime.createdSpecs[0].Prompt, stored)
	}
	if newControl.calls != 0 || len(newRuntime.liveAtBoundary) != 1 || newRuntime.liveAtBoundary[0] != "session-2" {
		t.Fatalf("context calls=%d live=%v boundary=%v", newControl.calls, liveSessions(newRuntime), newRuntime.liveAtBoundary)
	}
	entry, _, err := j.Entry(context.Background(), c.RunID)
	if err != nil || entry.RotationSummary != stored {
		t.Fatalf("stored rotation prompt=%q err=%v", entry.RotationSummary, err)
	}
}

func TestRotationSummarizerSuccessIsLabeledAndPersisted(t *testing.T) {
	f := completingRuntimeFake()
	msgs := rotationPromptFixture(t)
	f.messages, f.rotationMessages = msgs, rotationReplacement(msgs)
	s := &rotationSummarizerFake{runtimeFake: f, result: "summary text"}
	j := openTestJournal(t)
	e := rotationEngine(t, s, &rotationContextControlFake{}, j)
	e.RotationSummary = true
	if err := e.Process(testContext(t), rotationClaim()); err != nil {
		t.Fatal(err)
	}
	prompt := f.createdSpecs[1].Prompt
	if !strings.Contains(prompt, "Unverified context; verify against the workspace:\nsummary text") || s.calls != 1 || s.model != "provider/configured" || len(s.input) > rotationSummaryLimit {
		t.Fatalf("summary call=%d model=%q input=%d prompt=%q", s.calls, s.model, len(s.input), prompt)
	}
	entry, _, err := j.Entry(context.Background(), rotationClaim().RunID)
	if err != nil || entry.RotationSummary != prompt {
		t.Fatalf("persisted prompt=%q err=%v", entry.RotationSummary, err)
	}
}

func TestRotationSummarizerErrorFallsBack(t *testing.T) {
	rotationSummarizerFallbackTest(t, errors.New("summary failed"), false)
}

func TestRotationSummarizerContextDeadlineFallsBack(t *testing.T) {
	rotationSummarizerFallbackTest(t, nil, true)
}

func rotationSummarizerFallbackTest(t *testing.T, err error, block bool) {
	f := completingRuntimeFake()
	msgs := rotationPromptFixture(t)
	f.messages, f.rotationMessages = msgs, rotationReplacement(msgs)
	s := &rotationSummarizerFake{runtimeFake: f, err: err, block: block}
	j := openTestJournal(t)
	e := rotationEngine(t, s, &rotationContextControlFake{}, j)
	e.RotationSummary, e.RotationSummaryTimeout = true, time.Millisecond
	if processErr := e.Process(testContext(t), rotationClaim()); processErr != nil {
		t.Fatal(processErr)
	}
	if strings.Contains(f.createdSpecs[1].Prompt, "Unverified context; verify against the workspace:") {
		t.Fatal("failed summary was included")
	}
}

func TestRotationSummarizerOffNeverCalled(t *testing.T) {
	f := completingRuntimeFake()
	msgs := rotationPromptFixture(t)
	f.messages, f.rotationMessages = msgs, rotationReplacement(msgs)
	s := &rotationSummarizerFake{runtimeFake: f, result: "must not appear"}
	e := rotationEngine(t, s, &rotationContextControlFake{}, openTestJournal(t))
	if err := e.Process(testContext(t), rotationClaim()); err != nil {
		t.Fatal(err)
	}
	if s.calls != 0 {
		t.Fatalf("summarizer calls=%d", s.calls)
	}
}

func TestOpenCodeSummarizerSessionIsAllDenyAndTerminated(t *testing.T) {
	for _, tc := range []struct {
		name      string
		promptErr bool
		cancel    bool
	}{
		{"success", false, false}, {"prompt error", true, false}, {"context cancellation", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			terminated := false
			created := make(chan struct{})
			messageSeen := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/session" {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body["agent"] != "zoe-rotation-summary" {
						t.Fatalf("agent=%v", body["agent"])
					}
					permissions := body["permissions"].([]any)
					if len(permissions) != 1 || permissions[0].(map[string]any)["action"] != "*" || permissions[0].(map[string]any)["resource"] != "*" || permissions[0].(map[string]any)["effect"] != "deny" {
						t.Fatalf("permissions=%v", permissions)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": opencode.Session{ID: "summary"}})
					close(created)
					return
				}
				switch r.URL.Path {
				case "/api/session/summary/prompt":
					if tc.promptErr {
						http.Error(w, "bad prompt", http.StatusBadRequest)
						return
					}
				case "/api/session/summary/message":
					if tc.cancel {
						close(messageSeen)
						http.Error(w, "cancelled", http.StatusRequestTimeout)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "m", "type": "assistant", "text": "summary text"}, {"id": "idle", "type": "idle", "outcome": "succeeded"}}})
				case "/api/session/summary/interrupt":
					mu.Lock()
					terminated = true
					mu.Unlock()
				case "/api/session/active":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
				}
			}))
			defer server.Close()
			client, err := opencode.New(opencode.Config{BaseURL: server.URL, Password: "password"})
			if err != nil {
				t.Fatal(err)
			}
			r := OpenCodeRuntime{Client: client}
			ctx := context.Background()
			var cancel context.CancelFunc
			if tc.cancel {
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}
			result := make(chan struct{})
			go func() {
				_, _ = r.Summarize(ctx, "provider/model", "old content")
				close(result)
			}()
			if tc.cancel {
				<-created
				<-messageSeen
				cancel()
			}
			<-result
			mu.Lock()
			wasTerminated := terminated
			mu.Unlock()
			if !wasTerminated {
				t.Fatal("summary session was not terminated")
			}
		})
	}
}
