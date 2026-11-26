package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

type observationKey struct{}
type rotationObservationKey struct{}
type observationLease struct {
	Boot       string `json:"boot"`
	Generation uint64 `json:"generation"`
	expires    time.Time
}
type observationItem struct {
	lease                   observationLease
	source, typ, call, text string
	truncated               bool
}
type observationSender struct {
	lease     atomic.Pointer[observationLease]
	queue     chan observationItem
	secrets   []string
	seen      map[string]bool
	gap       atomic.Pointer[observationLease]
	baselines map[string]pollBaseline
}

type pollBaseline struct {
	boot       string
	generation uint64
	since      time.Time
	blocked    map[string]bool
}

func sameLease(a, b *observationLease) bool {
	return a != nil && b != nil && a.Boot == b.Boot && a.Generation == b.Generation
}

var credentialPattern = regexp.MustCompile(`(?i)(bearer\s+|(?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret|authorization)\s*[:=]\s*["']?)[^\s"',;]+|\b(?:sk|ghp|github_pat)_[A-Za-z0-9_-]{8,}\b|\bsk-[A-Za-z0-9_-]{8,}\b`)

func redactObservation(s string, secrets []string) (string, bool) {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	s = credentialPattern.ReplaceAllString(s, "[redacted]")
	truncated := len(s) > 8192
	if truncated {
		s = s[:8192]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s, truncated
}

func startModelObservation(ctx context.Context, e *Engine, c contracts.JobClaim) context.Context {
	client, ok := e.Control.(*ControlClient)
	if !ok {
		return ctx
	}
	s := &observationSender{queue: make(chan observationItem, 32), secrets: []string{client.token}, seen: make(map[string]bool)}
	if runtime, ok := e.Runtime.(OpenCodeRuntime); ok {
		s.secrets = append(s.secrets, runtime.Password)
	}
	if runtime, ok := e.Runtime.(*OpenCodeRuntime); ok {
		s.secrets = append(s.secrets, runtime.Password)
	}
	go s.run(ctx, client, c)
	return context.WithValue(ctx, observationKey{}, s)
}

func (s *observationSender) run(ctx context.Context, c *ControlClient, claim contracts.JobClaim) {
	path := "/runner/v1/runs/" + url.PathEscape(claim.RunID) + "/model-observation"
	refresh := func() {
		requestCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		r, err := c.request(requestCtx, http.MethodGet, path+"?attempt_id="+url.QueryEscape(claim.AttemptID)+"&fence="+fmt.Sprint(claim.Fence), nil, "")
		if err != nil {
			s.lease.Store(nil)
			return
		}
		defer r.Body.Close()
		var lease observationLease
		if r.StatusCode == 200 && json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&lease) == nil && lease.Generation != 0 && lease.Boot != "" {
			lease.expires = time.Now().Add(3 * time.Second)
			s.lease.Store(&lease)
		} else {
			s.lease.Store(nil)
		}
	}
	refresh()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.lease.Store(nil)
			return
		case <-ticker.C:
			refresh()
			s.flushGap(ctx, c, claim, path)
		case item := <-s.queue:
			body, err := json.Marshal(map[string]any{"boot": item.lease.Boot, "generation": item.lease.Generation, "attempt_id": claim.AttemptID, "fence": claim.Fence, "call_id": item.call, "source": item.source, "type": item.typ, "text": item.text, "truncated": item.truncated})
			if err != nil {
				continue
			}
			requestCtx, cancel := context.WithTimeout(ctx, time.Second)
			r, err := c.request(requestCtx, http.MethodPost, path, body, "")
			if err == nil {
				r.Body.Close()
			}
			if err != nil || r.StatusCode != http.StatusNoContent {
				s.gap.Store(&item.lease)
			}
			cancel()
		}
	}
}

// Retry at most once per loop iteration, and only while the same checked lease is valid.
func (s *observationSender) flushGap(ctx context.Context, c *ControlClient, claim contracts.JobClaim, path string) {
	gap := s.gap.Load()
	if gap == nil {
		return
	}
	current := s.lease.Load()
	if !sameLease(gap, current) || time.Now().After(current.expires) {
		s.gap.CompareAndSwap(gap, nil)
		return
	}
	body, _ := json.Marshal(map[string]any{"boot": gap.Boot, "generation": gap.Generation, "attempt_id": claim.AttemptID, "fence": claim.Fence, "call_id": fmt.Sprintf("%064x", 0), "source": "worker", "type": "gap"})
	requestCtx, cancel := context.WithTimeout(ctx, time.Second)
	r, err := c.request(requestCtx, http.MethodPost, path, body, "")
	if err == nil {
		r.Body.Close()
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			s.gap.CompareAndSwap(gap, nil)
		}
	}
	cancel()
}

func observeModel(ctx context.Context, session, typ, key, text string) {
	source := "worker"
	if ctx.Value(rotationObservationKey{}) != nil {
		source = "worker_rotation"
	}
	s, ok := ctx.Value(observationKey{}).(*observationSender)
	if !ok {
		return
	}
	// Polling is repetitive; bound memory and never replay observations to a later subscription.
	digest := sha256.Sum256([]byte(source + "\x00" + session + "\x00" + typ + "\x00" + key))
	id := hex.EncodeToString(digest[:])
	if s.seen[id] || len(s.seen) >= 4096 {
		return
	}
	lease := s.lease.Load()
	if lease == nil || (!lease.expires.IsZero() && time.Now().After(lease.expires)) {
		return
	}
	text, truncated := redactObservation(text, s.secrets)
	call := sha256.Sum256([]byte(session))
	select {
	case s.queue <- observationItem{lease: *lease, source: source, typ: typ, call: hex.EncodeToString(call[:]), text: text, truncated: truncated}:
		s.seen[id] = true
	default:
		s.gap.Store(lease)
	}
}

func observeMessages(ctx context.Context, session string, messages []opencode.Message) {
	s, ok := ctx.Value(observationKey{}).(*observationSender)
	if !ok {
		return
	}
	lease := s.lease.Load()
	if lease == nil || (!lease.expires.IsZero() && time.Now().After(lease.expires)) {
		return
	}
	if s.baselines == nil {
		s.baselines = make(map[string]pollBaseline)
	}
	baseline, exists := s.baselines[session]
	if !exists || baseline.boot != lease.Boot || baseline.generation != lease.Generation {
		baseline = pollBaseline{boot: lease.Boot, generation: lease.Generation, since: time.Now(), blocked: make(map[string]bool)}
		for _, m := range messages {
			id := m.ID
			if id == "" {
				id = m.Info.ID
			}
			if id != "" {
				baseline.blocked[id] = true
			}
		}
		s.baselines[session] = baseline
		s.gap.Store(lease)
		return
	}
	for _, m := range messages {
		role := m.Type
		if role == "" {
			role = m.Info.Role
		}
		if role != "assistant" {
			continue
		}
		id := m.ID
		if id == "" {
			id = m.Info.ID
		}
		if id == "" {
			continue
		}
		created := m.Time.Created
		if created == 0 {
			created = m.Info.Time.Created
		}
		createdAt := time.UnixMilli(created)
		if baseline.blocked[id] || created <= 0 || !createdAt.After(baseline.since) || createdAt.After(time.Now().Add(time.Second)) {
			continue
		}
		if m.Text != "" {
			observeModel(ctx, session, "response", id+"\x00"+m.Text, m.Text)
		}
		parts := m.Parts
		if len(parts) == 0 {
			parts = m.Content
		}
		for _, part := range parts {
			typ, _ := part["type"].(string)
			if typ == "text" {
				if text, ok := part["text"].(string); ok && text != "" {
					observeModel(ctx, session, "response", id+"\x00"+text, text)
				}
			}
			if typ == "tool" { // Never forward tool arguments, output, title, or raw errors.
				name, _ := part["tool"].(string)
				if name != "" && len(name) <= 64 && regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`).MatchString(name) {
					observeModel(ctx, session, "tool_call", id+"\x00"+name, name)
				}
			}
		}
	}
}
