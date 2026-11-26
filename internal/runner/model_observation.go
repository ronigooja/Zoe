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
type observationLease struct {
	Boot       string `json:"boot"`
	Generation uint64 `json:"generation"`
}
type observationItem struct {
	lease           observationLease
	typ, call, text string
	truncated       bool
}
type observationSender struct {
	lease   atomic.Pointer[observationLease]
	queue   chan observationItem
	secrets []string
	seen    map[string]bool
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
		s.lease.Store(nil)
		requestCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		r, err := c.request(requestCtx, http.MethodGet, path+"?attempt_id="+url.QueryEscape(claim.AttemptID)+"&fence="+fmt.Sprint(claim.Fence), nil, "")
		if err != nil {
			return
		}
		defer r.Body.Close()
		var lease observationLease
		if r.StatusCode == 200 && json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&lease) == nil && lease.Generation != 0 && lease.Boot != "" {
			s.lease.Store(&lease)
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
		case item := <-s.queue:
			body, err := json.Marshal(map[string]any{"boot": item.lease.Boot, "generation": item.lease.Generation, "attempt_id": claim.AttemptID, "fence": claim.Fence, "call_id": item.call, "type": item.typ, "text": item.text, "truncated": item.truncated})
			if err != nil {
				continue
			}
			requestCtx, cancel := context.WithTimeout(ctx, time.Second)
			r, err := c.request(requestCtx, http.MethodPost, path, body, "")
			if err == nil {
				r.Body.Close()
			}
			cancel()
		}
	}
}

func observeModel(ctx context.Context, session, typ, key, text string) {
	s, ok := ctx.Value(observationKey{}).(*observationSender)
	if !ok {
		return
	}
	// Polling is repetitive; bound memory and never replay observations to a later subscription.
	digest := sha256.Sum256([]byte(session + "\x00" + typ + "\x00" + key))
	id := hex.EncodeToString(digest[:])
	if s.seen[id] || len(s.seen) >= 4096 {
		return
	}
	s.seen[id] = true
	lease := s.lease.Load()
	if lease == nil {
		return
	}
	text, truncated := redactObservation(text, s.secrets)
	call := sha256.Sum256([]byte(session))
	select {
	case s.queue <- observationItem{lease: *lease, typ: typ, call: hex.EncodeToString(call[:]), text: text, truncated: truncated}:
	default:
	}
}

func observeMessages(ctx context.Context, session string, messages []opencode.Message) {
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
		if m.Text != "" {
			observeModel(ctx, session, "response", id+"\x00"+m.Text, m.Text)
		}
		for _, part := range m.Parts {
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
