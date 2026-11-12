package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

type appendControl struct {
	controlFake
	entries []contracts.RunEntry
	failAck bool
}

func (c *appendControl) Control(context.Context, string) (contracts.RunControl, error) {
	return contracts.RunControl{DesiredControl: "run", Entries: append([]contracts.RunEntry(nil), c.entries...)}, nil
}
func (c *appendControl) AcknowledgeEntry(_ context.Context, _ string, d contracts.EntryDelivery) error {
	if c.failAck {
		c.failAck = false
		return errors.New("lost acknowledgement")
	}
	if len(c.entries) == 0 || d.EntryID != c.entries[0].ID {
		return errors.New("out of order")
	}
	c.entries = c.entries[1:]
	return nil
}

func TestAppendDeliverySteerRetriesAndRestart(t *testing.T) {
	// Unlike a runtime mock, this exercises the real OpenCode HTTP request body.
	var bodies []map[string]string
	accepted := map[string]bool{}
	var texts []string
	first := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/session/prompt" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		if first {
			first = false
			w.WriteHeader(503)
			return
		}
		if !accepted[body["id"]] {
			accepted[body["id"]] = true
			texts = append(texts, body["text"])
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	c := &appendControl{entries: []contracts.RunEntry{{ID: "a", Content: "  original\n用户 text  "}, {ID: "b", Content: "second"}}, failAck: true}
	j := openTestJournal(t)
	claim := seedRuntimePhase(t, j, "prompt_sent")
	e := &Engine{Control: c, Journal: j, Runtime: OpenCodeRuntime{BaseURL: server.URL, Password: "test-password"}}
	e.deliverEntries(context.Background(), claim, RuntimeBinding{SessionID: "session"}, c.entries)
	if len(c.entries) != 2 {
		t.Fatal("failed prompt acknowledged")
	}
	e.deliverEntries(context.Background(), claim, RuntimeBinding{SessionID: "session"}, c.entries)
	if len(c.entries) != 2 {
		t.Fatal("failed ack advanced cursor")
	}
	// A fresh engine has no in-memory delivery state; message identity must match.
	restarted := &Engine{Control: c, Journal: j, Runtime: e.Runtime}
	restarted.deliverEntries(context.Background(), claim, RuntimeBinding{SessionID: "session"}, c.entries)
	restarted.deliverEntries(context.Background(), claim, RuntimeBinding{SessionID: "session"}, c.entries)
	if !reflect.DeepEqual(texts, []string{"  original\n用户 text  ", "second"}) {
		t.Fatalf("admitted=%q", texts)
	}
	if len(bodies) != 4 {
		t.Fatalf("attempts=%d", len(bodies))
	}
	for i, b := range bodies {
		if b["delivery"] != "steer" || b["id"] == "" {
			t.Fatalf("options=%v", b)
		}
		if i < 3 && b["id"] != bodies[0]["id"] {
			t.Fatal("retry message id changed")
		}
	}
}

type appendRuntime struct {
	*runtimeFake
	delivered []contracts.RunEntry
}

func (r *appendRuntime) PromptEntry(_ context.Context, _ RuntimeBinding, e contracts.RunEntry) error {
	r.delivered = append(r.delivered, e)
	return nil
}

func TestAppendMonitorPausesPendingAndRotating(t *testing.T) {
	for _, kind := range []string{"permission", "question", "rotation", "stop"} {
		t.Run(kind, func(t *testing.T) {
			j := openTestJournal(t)
			claim := seedRuntimePhase(t, j, "prompt_sent")
			c := &appendControl{entries: []contracts.RunEntry{{ID: "a", Content: "followup"}}}
			f := &runtimeFake{}
			if kind == "permission" {
				f.pendingPermissions = []opencode.PermissionRequest{{ID: "p"}}
			}
			if kind == "question" {
				f.pendingForms = []opencode.FormRequest{{ID: "q", Title: "question"}}
			}
			if kind == "rotation" {
				f.messages = []opencode.Message{{Type: "assistant", Tokens: opencode.TokenUsage{Input: 100000}}}
			}
			r := &appendRuntime{runtimeFake: f}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			e := &Engine{Control: c, Runtime: r, Journal: j, PollInterval: time.Millisecond, ContextTokenBudget: 10, MaxRotations: 1}
			if kind == "rotation" {
				e.Routes = map[string]Route{claim.ProfileID: {Model: "provider/model"}}
			}
			if kind == "stop" {
				e.Control = &stoppingControl{}
			}
			_ = e.monitor(ctx, claim, RuntimeBinding{SessionID: "session"})
			if len(r.delivered) != 0 {
				t.Fatalf("delivered while %s: %v", kind, r.delivered)
			}
		})
	}
}
