package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
	"github.com/ronigooja/Zoe/internal/profiles"
)

var ErrNoClaim = errors.New("no runner claim available")
var ErrOutcomePending = errors.New("external outcome remains unresolved and fenced")

type WorkIsolation interface {
	Check(context.Context, contracts.JobClaim) error
}
type RunnerAPI interface {
	Claim(context.Context) (*contracts.JobClaim, error)
	Current(context.Context) ([]contracts.JobClaim, error)
	Control(context.Context, string) (contracts.RunControl, error)
	Observe(context.Context, string, contracts.RunnerObservation) error
}
type rotationContextControl interface {
	RotationContext(context.Context, string, string, uint64) (contracts.RotationContext, error)
}
type runBudgetControl interface {
	ReserveBudget(context.Context, string, contracts.RunBudgetReserveRequest) (contracts.RunBudgetReceipt, error)
	SettleBudget(context.Context, string, string, contracts.RunBudgetSettleRequest) (contracts.RunBudgetReceipt, error)
}
type dispatchAuthorizationControl interface {
	CheckDispatchAuthorization(context.Context, string) (bool, string, error)
}
type runtimeAuthorizationControl interface {
	RuntimeAuthorization(context.Context, string, string, uint64) (*contracts.RuntimeAuthorization, error)
}
type Route struct {
	ProfileID       string `json:"profile_id"`
	RuntimeRouteID  string `json:"runtime_route_id"`
	ModelRouteID    string `json:"model_route_id"`
	IsolationPolicy string `json:"isolation_policy"`
	Harness         string `json:"harness"`
	Model           string `json:"model"`
	Effort          string `json:"effort,omitempty"`
	ApprovalMode    string `json:"approval_mode"`
}
type Engine struct {
	Control                      RunnerAPI
	Instance                     InstanceAuthority
	Runtime                      Runtime
	Journal                      *Journal
	Routes                       map[string]Route
	Isolation                    WorkIsolation
	PollInterval, ObserveTimeout time.Duration
	WaitLimit                    time.Duration
	Now                          func() time.Time
	ContextTokenBudget           int64
	ContextWindowRatio           float64
	MaxRotations                 int
	RotationSummary              bool
	RotationSummaryTimeout       time.Duration
	windowMu                     sync.Mutex
	windowCache                  map[string]int64
	windowUnavailable            map[string]bool
}

func (e *Engine) RunOnce(ctx context.Context) error {
	if e.Control == nil || e.Journal == nil {
		return errors.New("runner engine requires control client and journal")
	}
	c, err := e.Control.Claim(ctx)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNoClaim
	}
	return e.Process(ctx, *c)
}

func (e *Engine) Process(ctx context.Context, claim contracts.JobClaim) (err error) {
	if err := validateClaim(claim); err != nil {
		return err
	}
	if e.Runtime == nil || e.Journal == nil {
		return errors.New("runner runtime and journal are required")
	}
	entry, ok, err := e.Journal.Entry(ctx, claim.RunID)
	if err != nil {
		return err
	}
	if ok {
		// Repair the control-plane binding before any recovery handler can emit
		// an observation. This is required for prompt_sent and waiting phases,
		// whose recovery path does not pass through flush.
		if err := e.rebindFromJournal(ctx, claim.RunID); err != nil {
			return err
		}
		defer func() {
			if err == nil {
				err = e.flushPending(ctx, claim.RunID)
			}
		}()
		switch entry.Phase {
		case "create_attempted", "effect_unknown":
			return e.reportUnknownCreate(ctx, claim)
		case "create_rejected", "no_effect_terminal":
			return e.flush(ctx, claim.RunID)
		case "bound":
			if entry.Session == "" {
				return fmt.Errorf("%w: bound runtime recovery identity incomplete", ErrOutcomePending)
			}
			return e.sendPrompt(ctx, claim, entry.Spec, RuntimeBinding{SessionID: entry.Session})
		case "prompt_attempted":
			return e.markUnknown(ctx, claim)
		case "prompt_sent":
			if entry.Session == "" {
				return fmt.Errorf("%w: prompt-sent runtime recovery identity incomplete", ErrOutcomePending)
			}
			return e.monitor(ctx, claim, RuntimeBinding{SessionID: entry.Session})
		case "rotating", "rotation_create_attempted", "rotation_created":
			return e.recoverRotation(ctx, claim, entry)
		case "waiting_permission", "waiting_question":
			if entry.Session == "" {
				return fmt.Errorf("%w: waiting runtime identity incomplete", ErrOutcomePending)
			}
			return e.monitor(ctx, claim, RuntimeBinding{SessionID: entry.Session})
		}
	}
	route, exists := e.Routes[claim.ProfileID]
	if !exists || route.Harness != "opencode" || route.IsolationPolicy != OpenCodeWorkspaceWritePolicy {
		return e.reportNoEffect(ctx, claim, "runtime_unavailable", "OpenCode route unavailable")
	}
	if e.Isolation != nil {
		if err := e.Isolation.Check(ctx, claim); err != nil {
			return e.reportNoEffect(ctx, claim, "isolation_denied", err.Error())
		}
	}
	if c, ok := e.Control.(dispatchAuthorizationControl); ok {
		allowed, reason, err := c.CheckDispatchAuthorization(ctx, claim.RunID)
		if err != nil {
			return err
		}
		if !allowed {
			return e.reportNoEffect(ctx, claim, reason, "dispatch authorization denied")
		}
	}
	prompt := buildPrompt(claim)
	if claim.ResumeQuestion != "" {
		prompt = buildResumePrompt(claim.Goal, claim.ResumeQuestion, claim.ResumeAnswer)
	}
	spec := RuntimeSpec{RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: fmt.Sprint(claim.Fence), Model: route.Model, ModelRouteID: claim.ModelRouteID, Agent: claim.Agent, Permissions: claim.PermissionRules, Prompt: prompt, Workspace: claim.WorkspaceID}
	entry, err = e.Journal.SaveClaim(ctx, claim, spec, "claimed")
	if err != nil {
		return err
	}
	if err := e.checkInstance(ctx, claim, true); err != nil {
		return err
	}
	if bc, ok := e.Control.(runBudgetControl); ok && claim.Budget.MaxModelCalls > 0 {
		_, err = bc.ReserveBudget(ctx, claim.RunID, contracts.RunBudgetReserveRequest{AttemptID: claim.AttemptID, Fence: claim.Fence, ReservationID: "runtime-initial-prompt", Usage: contracts.RunBudgetUsage{ModelCalls: 1}})
		if err != nil {
			return err
		}
	}
	if err = e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "dispatch_pending"); err != nil {
		return err
	}
	if err = e.emit(ctx, claim, "dispatch_started", "runtime", "runtime dispatch started", nil, nil); err != nil {
		return err
	}
	if err = e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "create_ready"); err != nil {
		return err
	}
	if err = e.checkInstance(ctx, claim, true); err != nil {
		return err
	}
	if err = e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "create_attempted"); err != nil {
		return err
	}
	binding, err := e.Runtime.Create(ctx, spec)
	if err != nil {
		var rejected RuntimeCreateRejectedError
		if errors.As(err, &rejected) && rejected.Valid() {
			return e.reportRejected(ctx, claim, rejected.Code)
		}
		log.Printf("runtime create failed for run %s: %v", safeRuntimeID(claim.RunID), err)
		return e.markUnknown(ctx, claim)
	}
	if binding.SessionID == "" {
		return e.markUnknown(ctx, claim)
	}
	if err = e.Journal.SetSession(ctx, claim.RunID, claim.AttemptID, claim.Fence, binding.SessionID, "bound"); err != nil {
		return err
	}
	if err = e.emit(ctx, claim, "dispatch_bound", "runtime", "runtime binding established", nil, []contracts.ExternalEffect{{ID: claim.RunID, Kind: "runtime_create", Status: "created", Reference: binding.SessionID}}); err != nil {
		return err
	}
	return e.sendPrompt(ctx, claim, spec, binding)
}

func (e *Engine) sendPrompt(ctx context.Context, claim contracts.JobClaim, spec RuntimeSpec, binding RuntimeBinding) error {
	if err := e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "prompt_attempted"); err != nil {
		return err
	}
	if err := e.Runtime.Prompt(ctx, binding, spec.Prompt); err != nil {
		// The session exists, but a failed request does not prove whether the
		// prompt reached it. Fence the run rather than risking a duplicate.
		log.Printf("runtime prompt failed for run %s (error type %T)", safeRuntimeID(claim.RunID), err)
		return e.markUnknown(ctx, claim)
	}
	if err := e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "prompt_sent"); err != nil {
		return err
	}
	return e.monitor(ctx, claim, binding)
}

func (e *Engine) monitor(ctx context.Context, claim contracts.JobClaim, b RuntimeBinding) error {
	steps := int64(0)
	events, errs := e.Runtime.Events(ctx, b)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				return e.markUnknown(ctx, claim)
			}
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if ev.Type == "message.part.updated" || ev.Type == "message.updated" {
				steps++
			}
			if claim.Budget.MaxToolSteps > 0 && steps > claim.Budget.MaxToolSteps {
				_ = e.Runtime.Abort(ctx, b)
				return e.terminate(ctx, claim, b, "tool_step_limit", false)
			}
		default:
		}
		control, err := e.Control.Control(ctx, claim.RunID)
		if err == nil && control.DesiredControl == "stop" {
			_ = e.Runtime.Abort(ctx, b)
			return e.terminate(ctx, claim, b, "stop_confirmed", true)
		}
		entry, _, _ := e.Journal.Entry(ctx, claim.RunID)
		waiting := entry.Phase == "waiting_permission" || entry.Phase == "waiting_question"
		if waiting && e.WaitLimit > 0 && !entry.Updated.IsZero() && e.now().After(entry.Updated.Add(e.WaitLimit)) {
			_ = e.Runtime.Abort(ctx, b)
			return e.terminate(ctx, claim, b, "wait_limit_exceeded", false)
		}
		status, err := e.Runtime.Status(ctx, b)
		if err != nil {
			time.Sleep(e.interval())
			continue
		}
		if pending, ok := e.Runtime.(PendingRequests); ok {
			if requests, reqErr := pending.PendingPermissions(ctx, b); reqErr == nil && len(requests) > 0 {
				r := requests[0]
				entry, _, _ := e.Journal.Entry(ctx, claim.RunID)
				if authc, ok := e.Control.(runtimeAuthorizationControl); ok {
					if authorization, authErr := authc.RuntimeAuthorization(ctx, claim.RunID, r.ID, claim.Fence); authErr == nil && authorization != nil {
						decision := opencode.PermissionDecision(authorization.Decision)
						if decision != opencode.PermissionOnce && decision != opencode.PermissionAlways && decision != opencode.PermissionReject {
							return errors.New("invalid runtime authorization decision")
						}
						if reply, ok := e.Runtime.(PermissionReply); ok {
							if err := reply.ReplyPermission(ctx, b, r.ID, decision, authorization.Message); err != nil {
								return err
							}
							_ = e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "prompt_sent")
							continue
						}
					}
				}
				if entry.Phase != "waiting_permission" {
					if err := e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "waiting_permission"); err != nil {
						return err
					}
					if err := e.emitPending(ctx, claim, "permission_requested", map[string]any{"request_id": r.ID, "action": r.Action, "resources": r.Resources, "metadata": r.Metadata}); err != nil {
						return err
					}
				}
				time.Sleep(e.interval())
				continue
			}
			if forms, reqErr := pending.PendingForms(ctx, b); reqErr == nil && len(forms) > 0 {
				f := forms[0]
				entry, _, _ := e.Journal.Entry(ctx, claim.RunID)
				if authc, ok := e.Control.(runtimeAuthorizationControl); ok {
					if authorization, authErr := authc.RuntimeAuthorization(ctx, claim.RunID, f.ID, claim.Fence); authErr == nil && authorization != nil {
						if authorization.Decision != "answer" {
							return errors.New("invalid runtime authorization decision")
						}
						reply, ok := e.Runtime.(FormReply)
						if !ok {
							return errors.New("runtime does not support same-session form replies")
						}
						key := ""
						if len(f.Fields) > 0 {
							key, _ = f.Fields[0]["key"].(string)
						}
						if key == "" {
							return errors.New("runtime question form has no field key")
						}
						if err := reply.ReplyForm(ctx, b, f.ID, map[string]any{key: authorization.Message}); err != nil {
							return err
						}
						if err := e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "prompt_sent"); err != nil {
							return err
						}
						continue
					}
				}
				question := f.Title
				for _, field := range f.Fields {
					if q, ok := field["description"].(string); ok && q != "" {
						question = q
						break
					}
				}
				if len(question) > 4000 {
					question = question[:4000]
				}
				if entry.Phase != "waiting_question" {
					if err := e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "waiting_question"); err != nil {
						return err
					}
					if err := e.emitPending(ctx, claim, "worker_question", map[string]any{"request_id": f.ID, "question": question, "title": f.Title}); err != nil {
						return err
					}
				}
				time.Sleep(e.interval())
				continue
			}
		}
		entry, _, _ = e.Journal.Entry(ctx, claim.RunID)
		waiting = entry.Phase == "waiting_permission" || entry.Phase == "waiting_question"
		if !waiting && !claim.Budget.HardDeadline.IsZero() && e.now().After(claim.Budget.HardDeadline) {
			_ = e.Runtime.Abort(ctx, b)
			return e.terminate(ctx, claim, b, "run_deadline_exceeded", false)
		}
		msgs, err := e.Runtime.Messages(ctx, b)
		if err != nil {
			time.Sleep(e.interval())
			continue
		}
		if !waiting && e.contextExceeded(ctx, claim, msgs) {
			if entry.RotationCount > 0 && e.rotationHasNoRoom(ctx, claim, msgs) {
				_ = e.Runtime.Abort(ctx, b)
				return e.terminate(ctx, claim, b, "context_threshold_below_baseline", false)
			}
			return e.rotate(ctx, claim, b, msgs)
		}
		_ = status
		m, done, term := opencode.Completion(msgs)
		if !done {
			time.Sleep(e.interval())
			continue
		}
		if term != nil {
			_ = e.emit(ctx, claim, "failure", "runtime", "runtime reported failure", term, nil)
			return e.terminate(ctx, claim, b, "runtime_error", false)
		}
		if err := e.emitMessage(ctx, claim, "result", "runtime", "runtime result", m); err != nil {
			return err
		}
		return e.terminate(ctx, claim, b, "completed", false)
	}
}

func (e *Engine) rotationHasNoRoom(ctx context.Context, c contracts.JobClaim, msgs []opencode.Message) bool {
	window, ok := e.contextWindow(ctx, c)
	if !ok {
		return false
	}
	ratio := e.ContextWindowRatio
	if ratio <= 0 {
		ratio = 0.5
	}
	limit := int64(float64(window) * ratio)
	if e.ContextTokenBudget > 0 && e.ContextTokenBudget < limit {
		limit = e.ContextTokenBudget
	}
	if c.Budget.MaxContextTokens > 0 && (limit <= 0 || c.Budget.MaxContextTokens < limit) {
		limit = c.Budget.MaxContextTokens
	}
	if limit <= 0 {
		return false
	}
	var earliest opencode.Message
	for _, m := range msgs {
		if messageRole(m) != "assistant" || messageCreated(m) == 0 {
			continue
		}
		created, current := messageCreated(m), messageCreated(earliest)
		if current == 0 || created < current || (created == current && m.ID < earliest.ID) {
			earliest = m
		}
	}
	if messageRole(earliest) != "assistant" {
		return false
	}
	usage := earliest.Tokens.Input + earliest.Tokens.Output + earliest.Tokens.Cache.Read + earliest.Tokens.Cache.Write
	if usage == 0 {
		usage = earliest.Info.Tokens.Input + earliest.Info.Tokens.Output + earliest.Info.Tokens.Cache.Read + earliest.Info.Tokens.Cache.Write
	}
	return usage > 0 && usage >= limit
}

const (
	rotationPromptLimit  = 24000
	rotationSummaryLimit = 12000 // retained for legacy helper tests
)

func (e *Engine) contextExceeded(ctx context.Context, c contracts.JobClaim, msgs []opencode.Message) bool {
	window, ok := e.contextWindow(ctx, c)
	if !ok {
		return false
	}
	ratio := e.ContextWindowRatio
	if ratio <= 0 {
		ratio = 0.5
	}
	limit := int64(float64(window) * ratio)
	if e.ContextTokenBudget > 0 && e.ContextTokenBudget < limit {
		limit = e.ContextTokenBudget
	}
	if c.Budget.MaxContextTokens > 0 && (limit <= 0 || c.Budget.MaxContextTokens < limit) {
		limit = c.Budget.MaxContextTokens
	}
	if limit <= 0 {
		return false
	}
	var latest opencode.Message
	for _, m := range msgs {
		if messageRole(m) == "assistant" && (messageCreated(latest) == 0 || messageCreated(m) >= messageCreated(latest)) {
			latest = m
		}
	}
	if messageRole(latest) != "assistant" {
		return false
	}
	// Reasoning tokens are intentionally excluded: the historical budget counts input, output, and cache tokens.
	usage := latest.Tokens.Input + latest.Tokens.Output + latest.Tokens.Cache.Read + latest.Tokens.Cache.Write
	if usage == 0 {
		usage = latest.Info.Tokens.Input + latest.Info.Tokens.Output + latest.Info.Tokens.Cache.Read + latest.Info.Tokens.Cache.Write
	}
	return usage > limit
}

func (e *Engine) contextWindow(ctx context.Context, c contracts.JobClaim) (int64, bool) {
	route, ok := e.Routes[c.ProfileID]
	if !ok {
		return 0, false
	}
	key := route.Model
	e.windowMu.Lock()
	if e.windowCache != nil {
		if w, ok := e.windowCache[key]; ok {
			e.windowMu.Unlock()
			return w, true
		}
	}
	if e.windowUnavailable != nil && e.windowUnavailable[key] {
		e.windowMu.Unlock()
		return 0, false
	}
	e.windowMu.Unlock()
	p, ok := e.Runtime.(ContextWindowProvider)
	if !ok {
		return 0, false
	}
	w, err := p.ContextWindow(ctx, route.Model)
	if err != nil || w <= 0 {
		e.windowMu.Lock()
		if e.windowUnavailable == nil {
			e.windowUnavailable = map[string]bool{}
		}
		first := !e.windowUnavailable[key]
		e.windowUnavailable[key] = true
		e.windowMu.Unlock()
		if first {
			log.Printf("context rotation window unavailable for run %s; rotation disabled", safeRuntimeID(c.RunID))
		}
		return 0, false
	}
	e.windowMu.Lock()
	defer e.windowMu.Unlock()
	if e.windowCache == nil {
		e.windowCache = map[string]int64{}
	}
	e.windowCache[key] = w
	return w, true
}

func rotationSummary(msgs []opencode.Message) string {
	var b strings.Builder
	for i := len(msgs) - 1; i >= 0 && b.Len() < rotationSummaryLimit; i-- {
		m := msgs[i]
		if messageRole(m) != "assistant" && messageRole(m) != "user" {
			continue
		}
		if m.Text != "" {
			b.WriteString(m.Text)
			b.WriteByte('\n')
		}
		for _, p := range messageParts(m) {
			if text, ok := p["text"].(string); ok {
				b.WriteString(text)
				b.WriteByte('\n')
			}
		}
	}
	s := b.String()
	if len(s) > rotationSummaryLimit {
		s = s[:rotationSummaryLimit]
	}
	return s
}

func rotationRecentMessages(msgs []opencode.Message) string {
	const itemLimit = 300
	var b strings.Builder
	for _, m := range msgs {
		role := messageRole(m)
		if role == "" {
			continue
		}
		text := strings.TrimSpace(m.Text)
		for _, p := range messageParts(m) {
			kind, _ := p["type"].(string)
			if partText, ok := p["text"].(string); ok && partText != "" {
				if text != "" {
					text += "\n"
				}
				text += partText
			}
			if kind == "tool" || strings.Contains(kind, "tool") {
				name, _ := p["name"].(string)
				args, _ := json.Marshal(p["input"])
				if len(args) == 0 || string(args) == "null" {
					args, _ = json.Marshal(p["arguments"])
				}
				result, _ := json.Marshal(p["output"])
				if len(result) == 0 || string(result) == "null" {
					result, _ = json.Marshal(p["result"])
				}
				if text != "" {
					text += "\n"
				}
				text += "tool=" + name + " args=" + truncate(string(args), itemLimit) + " result=" + truncate(string(result), itemLimit)
			}
		}
		if text == "" {
			continue
		}
		b.WriteString(role + ": " + truncate(text, itemLimit) + "\n")
	}
	return b.String()
}

func rotationFiles(msgs []opencode.Message) string {
	seen := map[string]bool{}
	var files []string
	for _, m := range msgs {
		for _, p := range messageParts(m) {
			kind, _ := p["type"].(string)
			name, _ := p["name"].(string)
			if kind != "tool" && !strings.Contains(name, "edit") && !strings.Contains(name, "write") {
				continue
			}
			for _, key := range []string{"filePath", "file_path", "path", "filename"} {
				if path, ok := p[key].(string); ok && path != "" && !seen[path] {
					seen[path] = true
					files = append(files, path)
				}
			}
			for _, key := range []string{"input", "arguments"} {
				if raw, ok := p[key].(map[string]any); ok {
					for _, pathKey := range []string{"filePath", "file_path", "path", "filename"} {
						if path, ok := raw[pathKey].(string); ok && path != "" && !seen[path] {
							seen[path] = true
							files = append(files, path)
						}
					}
				}
			}
		}
	}
	return strings.Join(files, ", ")
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

func messageRole(m opencode.Message) string {
	if m.Type != "" {
		return m.Type
	}
	return m.Info.Role
}

func messageCreated(m opencode.Message) int64 {
	if m.Time.Created != 0 {
		return m.Time.Created
	}
	return m.Info.Time.Created
}

func messageParts(m opencode.Message) []map[string]any {
	if len(m.Parts) > 0 {
		return m.Parts
	}
	return m.Content
}

func (e *Engine) rotate(ctx context.Context, c contracts.JobClaim, old RuntimeBinding, msgs []opencode.Message) error {
	entry, _, err := e.Journal.Entry(ctx, c.RunID)
	if err != nil {
		return err
	}
	count := entry.RotationCount + 1
	max := e.MaxRotations
	if max <= 0 {
		max = 3
	}
	if count > max {
		_ = e.Runtime.Abort(ctx, old)
		return e.terminate(ctx, c, old, "context_rotation_limit_exceeded", false)
	}
	rotation := e.buildRotationPrompt(ctx, c, msgs)
	if err := e.Journal.BeginRotation(ctx, c.RunID, c.AttemptID, c.Fence, old.SessionID, rotation, count); err != nil {
		return err
	}
	if err := e.Journal.SetPhase(ctx, c.RunID, c.AttemptID, c.Fence, "rotation_create_attempted"); err != nil {
		return err
	}
	spec := entry.Spec
	spec.Prompt = rotation
	binding, err := e.Runtime.Create(ctx, spec)
	if err != nil {
		return e.markUnknown(ctx, c)
	}
	if err := e.Journal.SetRotationSession(ctx, c.RunID, c.AttemptID, c.Fence, binding.SessionID, "rotation_created"); err != nil {
		return err
	}
	if _, err := e.Runtime.Terminate(ctx, old); err != nil {
		return fmt.Errorf("%w: old session termination during rotation: %v", ErrOutcomePending, err)
	}
	if err := e.Runtime.Prompt(ctx, binding, spec.Prompt); err != nil {
		return e.markUnknown(ctx, c)
	}
	if err := e.Journal.SetPhase(ctx, c.RunID, c.AttemptID, c.Fence, "prompt_sent"); err != nil {
		return err
	}
	if err := e.emitSessionRotated(ctx, c, old.SessionID, binding.SessionID, count); err != nil {
		return err
	}
	return e.monitor(ctx, c, binding)
}

func (e *Engine) emitSessionRotated(ctx context.Context, c contracts.JobClaim, oldSession, newSession string, count int) error {
	raw, _ := json.Marshal(map[string]any{"event": "context_rotated", "rotation": count, "old_session_id": oldSession, "new_session_id": newSession})
	o := contracts.RunnerObservation{ObservationID: observationID(c.RunID, c.AttemptID, c.Fence, "session_rotated", string(raw)), RunID: c.RunID, AttemptID: c.AttemptID, Fence: c.Fence, Kind: "session_rotated", OccurredAt: e.now(), RuntimeSessionID: newSession, Result: raw}
	stored, err := e.Journal.QueueObservation(ctx, c.RunID, o)
	if err != nil {
		return err
	}
	return e.observeQueued(ctx, c.RunID, stored)
}

func (e *Engine) buildRotationPrompt(ctx context.Context, c contracts.JobClaim, msgs []opencode.Message) string {
	ctxData := contracts.RotationContext{MemoryUnavailable: true}
	if control, ok := e.Control.(rotationContextControl); ok {
		if refreshed, err := control.RotationContext(ctx, c.RunID, c.AttemptID, c.Fence); err == nil {
			ctxData = refreshed
		}
	}
	var b strings.Builder
	b.WriteString("Continue the assigned goal in a fresh session. Do not delegate or perform external irreversible changes.\n\nGoal:\n")
	b.WriteString(c.Goal + "\n\nUser instructions in order:\n")
	for _, instruction := range ctxData.Instructions {
		b.WriteString("- " + instruction + "\n")
	}
	if ctxData.MemoryUnavailable {
		b.WriteString("\nZoe memory is temporarily unavailable; do not assume any memory.\n")
	} else if len(ctxData.Memory.Items) > 0 {
		b.WriteString("\nRefreshed Zoe memory:\n")
		for _, item := range ctxData.Memory.Items {
			b.WriteString("[" + item.Subject + "/" + item.Key + "] " + item.Content + "\n")
		}
	}
	b.WriteString("\nFiles touched in the prior session: " + rotationFiles(msgs) + "\n\nRecent prior messages (chronological; verify against the workspace):\n" + rotationRecentMessages(msgs))
	if e.RotationSummary {
		if summarizer, ok := e.Runtime.(RotationSummarizer); ok {
			timeout := e.RotationSummaryTimeout
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			summaryCtx, cancel := context.WithTimeout(ctx, timeout)
			summary, err := summarizer.Summarize(summaryCtx, e.Routes[c.ProfileID].Model, rotationSummary(msgs))
			cancel()
			if err == nil && strings.TrimSpace(summary) != "" {
				b.WriteString("\n\nUnverified context; verify against the workspace:\n" + truncate(summary, 16000))
			} else if err != nil {
				log.Printf("rotation summary unavailable: %v", err)
			}
		}
	}
	standardClaim := c
	standardClaim.MemoryContext = contracts.MemoryContext{}
	b.WriteString("\n\nStandard constraints:\n" + buildPrompt(standardClaim))
	return truncate(b.String(), rotationPromptLimit)
}

func (e *Engine) recoverRotation(ctx context.Context, c contracts.JobClaim, entry JournalEntry) error {
	if entry.Phase == "rotating" && entry.RotationNewSession == "" {
		spec := entry.Spec
		spec.Prompt = entry.RotationSummary
		binding, err := e.Runtime.Create(ctx, spec)
		if err != nil {
			return e.markUnknown(ctx, c)
		}
		if err := e.Journal.SetRotationSession(ctx, c.RunID, c.AttemptID, c.Fence, binding.SessionID, "rotation_created"); err != nil {
			return err
		}
		entry.RotationNewSession = binding.SessionID
		entry.Phase = "rotation_created"
	}
	if entry.RotationNewSession == "" {
		recovery, ok := e.Runtime.(RotationRecovery)
		if !ok {
			return fmt.Errorf("%w: rotation recovery cannot discover replacement", ErrOutcomePending)
		}
		b, err := recovery.FindRotationSession(ctx, c.RunID, entry.RotationOldSession)
		if err != nil {
			return e.markUnknown(ctx, c)
		}
		if err := e.Journal.SetRotationSession(ctx, c.RunID, c.AttemptID, c.Fence, b.SessionID, "rotation_created"); err != nil {
			return err
		}
		entry.RotationNewSession = b.SessionID
	}
	b := RuntimeBinding{SessionID: entry.RotationNewSession}
	if entry.Phase == "rotation_created" {
		if entry.RotationOldSession != "" {
			if _, err := e.Runtime.Terminate(ctx, RuntimeBinding{SessionID: entry.RotationOldSession}); err != nil {
				return fmt.Errorf("%w: old rotation session termination: %v", ErrOutcomePending, err)
			}
		}
		if err := e.Runtime.Prompt(ctx, b, entry.RotationSummary); err != nil {
			return e.markUnknown(ctx, c)
		}
		if err := e.Journal.SetPhase(ctx, c.RunID, c.AttemptID, c.Fence, "prompt_sent"); err != nil {
			return err
		}
	}
	return e.monitor(ctx, c, b)
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) emitPending(ctx context.Context, c contracts.JobClaim, kind string, payload map[string]any) error {
	raw, _ := json.Marshal(payload)
	entry, _, _ := e.Journal.Entry(ctx, c.RunID)
	o := contracts.RunnerObservation{ObservationID: observationID(c.RunID, c.AttemptID, c.Fence, kind, string(raw)), RunID: c.RunID, AttemptID: c.AttemptID, Fence: c.Fence, Kind: kind, OccurredAt: e.now(), RuntimeSessionID: entry.Session, Result: raw}
	stored, err := e.Journal.QueueObservation(ctx, c.RunID, o)
	if err != nil {
		return err
	}
	return e.observeQueued(ctx, c.RunID, stored)
}

func (e *Engine) terminate(ctx context.Context, c contracts.JobClaim, b RuntimeBinding, reason string, stopped bool) error {
	if bc, ok := e.Control.(runBudgetControl); ok {
		_, _ = bc.SettleBudget(ctx, c.RunID, "runtime-initial-prompt", contracts.RunBudgetSettleRequest{AttemptID: c.AttemptID, Fence: c.Fence, SettlementID: "runtime-initial-prompt-settled", Outcome: "settled", Actual: contracts.RunBudgetUsage{ModelCalls: 1}})
	}
	id, err := e.Runtime.Terminate(ctx, b)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOutcomePending, err)
	}
	_ = id
	kind := "runtime_terminated"
	if stopped {
		kind = "stop_confirmed"
	}
	proof := &contracts.TerminationProof{Source: "runner_host_session", Method: "session_interrupted", RuntimeSessionID: b.SessionID, RuntimeContainerID: "", VerifiedAt: time.Now().UTC(), CleanupComplete: true}
	return e.emit(ctx, c, kind, "runtime", reason, proof, nil)
}

func safeRuntimeID(id string) string {
	if len(id) > 64 {
		return id[:64]
	}
	return id
}
func (e *Engine) markUnknown(ctx context.Context, c contracts.JobClaim) error {
	if bc, ok := e.Control.(runBudgetControl); ok {
		_, _ = bc.SettleBudget(ctx, c.RunID, "runtime-initial-prompt", contracts.RunBudgetSettleRequest{AttemptID: c.AttemptID, Fence: c.Fence, SettlementID: "runtime-initial-prompt-unknown", Outcome: "unknown"})
	}
	_ = e.Journal.SetPhase(ctx, c.RunID, c.AttemptID, c.Fence, "effect_unknown")
	_ = e.emit(ctx, c, "effect_unknown", "runtime", "runtime create outcome is unknown", nil, []contracts.ExternalEffect{{ID: c.RunID, Kind: "runtime_create", Status: "unknown"}})
	return ErrOutcomePending
}
func (e *Engine) reportUnknownCreate(ctx context.Context, c contracts.JobClaim) error {
	return e.markUnknown(ctx, c)
}
func (e *Engine) reportRejected(ctx context.Context, c contracts.JobClaim, code string) error {
	_ = e.Journal.SetPhase(ctx, c.RunID, c.AttemptID, c.Fence, "create_rejected")
	return e.emit(ctx, c, "create_rejected", "runtime", code, &contracts.CreateRejectionProof{Code: code}, nil)
}
func (e *Engine) reportNoEffect(ctx context.Context, c contracts.JobClaim, code, msg string) error {
	_ = e.Journal.SetPhase(ctx, c.RunID, c.AttemptID, c.Fence, "no_effect_terminal")
	return e.emit(ctx, c, "no_effect_terminal", "runner", msg, &contracts.NoEffectProof{JournalPhase: "claimed", Outcome: "failed"}, nil)
}
func (e *Engine) emit(ctx context.Context, c contracts.JobClaim, kind, source, summary string, proof any, effects []contracts.ExternalEffect) error {
	var result json.RawMessage
	if proof != nil {
		result, _ = json.Marshal(proof)
	}
	entry, _, _ := e.Journal.Entry(ctx, c.RunID)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s", c.RunID, c.AttemptID, kind, summary)))
	o := contracts.RunnerObservation{ObservationID: hex.EncodeToString(sum[:16]), RunID: c.RunID, AttemptID: c.AttemptID, Fence: c.Fence, Kind: kind, OccurredAt: e.now(), RuntimeSessionID: entry.Session, RuntimeContainerID: entry.Container, Result: result, ExternalEffects: effects}
	o.Error = summary
	if p, ok := proof.(*contracts.TerminationProof); ok {
		o.TerminationProof = p
	}
	if p, ok := proof.(*contracts.CreateRejectionProof); ok {
		o.CreateRejection = p
	}
	if p, ok := proof.(*contracts.NoEffectProof); ok {
		o.NoEffectProof = p
	}
	stored, err := e.Journal.QueueObservation(ctx, c.RunID, o)
	if err != nil {
		return err
	}
	if e.Control == nil {
		return nil
	}
	return e.observeQueued(ctx, c.RunID, stored)
}

func (e *Engine) observeQueued(ctx context.Context, run string, observation contracts.RunnerObservation) error {
	if err := e.Control.Observe(ctx, run, observation); err != nil {
		return err
	}
	return e.Journal.AcknowledgeObservation(ctx, observation.ObservationID)
}
func (e *Engine) emitMessage(ctx context.Context, c contracts.JobClaim, kind, source, summary string, m opencode.Message) error {
	out := ""
	for _, part := range m.Parts {
		if text, ok := part["text"].(string); ok {
			out += text
		}
	}
	raw, _ := json.Marshal(map[string]any{"assistant_output": out, "assistant_message": m})
	entry, _, _ := e.Journal.Entry(ctx, c.RunID)
	evidence := toolEvidence(entry.Session, m)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s", c.RunID, c.AttemptID, kind, summary)))
	o := contracts.RunnerObservation{ObservationID: hex.EncodeToString(sum[:16]), RunID: c.RunID, AttemptID: c.AttemptID, Fence: c.Fence, Kind: kind, OccurredAt: e.now(), RuntimeSessionID: entry.Session, RuntimeContainerID: entry.Container, Result: raw, ExternalEffects: []contracts.ExternalEffect{}, Evidence: evidence}
	stored, err := e.Journal.QueueObservation(ctx, c.RunID, o)
	if err != nil {
		return err
	}
	return e.Control.Observe(ctx, c.RunID, stored)
}
func (e *Engine) flush(ctx context.Context, run string) error {
	if err := e.rebindFromJournal(ctx, run); err != nil {
		return err
	}
	return e.flushPending(ctx, run)
}

func (e *Engine) flushPending(ctx context.Context, run string) error {
	pending, err := e.Journal.PendingObservations(ctx, run)
	if err != nil {
		return err
	}
	for _, q := range pending {
		if err := e.Control.Observe(ctx, run, q.Body); err != nil {
			return err
		}
		if err := e.Journal.AcknowledgeObservation(ctx, q.Body.ObservationID); err != nil {
			return err
		}
	}
	return nil
}

// FlushPendingObservations runs the durable recovery replay used by terminal
// claim recovery. It is exported so acceptance tests and recovery operators
// can exercise the same journal-to-control path without a runtime process.
func (e *Engine) FlushPendingObservations(ctx context.Context, run string) error {
	if e.Control == nil || e.Journal == nil {
		return errors.New("runner recovery requires control client and journal")
	}
	return e.flush(ctx, run)
}

// rebindFromJournal repairs the server binding before terminal observations
// are replayed. Releases before session_rotated was introduced recorded
// rotations as progress observations, so the server cannot infer the chain
// from those rows itself. Each repair is still submitted through the normal
// strict server validation path.
func (e *Engine) rebindFromJournal(ctx context.Context, run string) error {
	history, err := e.Journal.ObservationHistory(ctx, run)
	if err != nil {
		return err
	}
	bound := ""
	for _, q := range history {
		if q.Body.Kind == "dispatch_bound" && q.Body.RuntimeSessionID != "" {
			bound = q.Body.RuntimeSessionID
			break
		}
	}
	if bound == "" {
		return nil
	}

	type transition struct {
		old, new string
		count    int
	}
	existing := make(map[string]bool)
	var transitions []transition
	current := bound
	for _, q := range history {
		o := q.Body
		switch o.Kind {
		case "session_rotated":
			var payload struct {
				Old   string `json:"old_session_id"`
				New   string `json:"new_session_id"`
				Count int    `json:"rotation"`
			}
			if json.Unmarshal(o.Result, &payload) != nil || payload.Old == "" || payload.New == "" {
				continue
			}
			existing[payload.Old+"\x00"+payload.New] = true
			current = payload.New
		case "progress":
			var payload struct {
				Event string `json:"event"`
				Count int    `json:"rotation"`
				Old   string `json:"old_session_id"`
				New   string `json:"new_session_id"`
			}
			if o.RuntimeSessionID == "" || json.Unmarshal(o.Result, &payload) != nil || payload.Event != "context_rotated" {
				continue
			}
			newSession := payload.New
			if newSession == "" {
				newSession = o.RuntimeSessionID
			}
			oldSession := payload.Old
			if oldSession == "" {
				oldSession = current
			}
			if newSession == oldSession {
				continue
			}
			transitions = append(transitions, transition{old: oldSession, new: newSession, count: payload.Count})
			current = newSession
		}
	}
	for i, tr := range transitions {
		key := tr.old + "\x00" + tr.new
		if existing[key] {
			continue
		}
		count := tr.count
		if count <= 0 {
			count = i + 1
		}
		entry, ok, err := e.Journal.Entry(ctx, run)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("runner journal claim missing during rotation recovery")
		}
		stored, err := e.Journal.QueueObservation(ctx, run, contracts.RunnerObservation{
			ObservationID: observationID(entry.Claim.RunID, entry.Claim.AttemptID, entry.Claim.Fence, "session_rotated", tr.old+"\x00"+tr.new),
			RunID:         entry.Claim.RunID, AttemptID: entry.Claim.AttemptID, Fence: entry.Claim.Fence,
			Kind: "session_rotated", OccurredAt: e.now(), RuntimeSessionID: tr.new,
			Result: mustJSON(map[string]any{"event": "context_rotated", "rotation": count, "old_session_id": tr.old, "new_session_id": tr.new}),
		})
		if err != nil {
			return err
		}
		if err := e.Control.Observe(ctx, run, stored); err != nil {
			return err
		}
		if err := e.Journal.AcknowledgeObservation(ctx, stored.ObservationID); err != nil {
			return err
		}
		existing[key] = true
	}
	return nil
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
func (e *Engine) interval() time.Duration {
	if e.PollInterval > 0 {
		return e.PollInterval
	}
	return time.Second
}
func validateClaim(c contracts.JobClaim) error {
	if c.RunID == "" || c.TaskID == "" || c.AttemptID == "" || c.WorkspaceID == "" || c.Fence == 0 || c.ProfileID == "" || c.RuntimeRouteID == "" || c.ModelRouteID == "" || c.IsolationPolicy == "" {
		return errors.New("runner claim is missing required identity")
	}
	if c.Harness != "opencode" {
		return errors.New("runner claim selects unsupported runtime")
	}
	if c.Agent == "" || c.PermissionTier == "" {
		return errors.New("runner claim is missing profile permissions")
	}
	if err := profiles.ValidateRuleset(c.PermissionRules, c.PermissionTier); err != nil {
		return fmt.Errorf("invalid runner claim permissions: %w", err)
	}
	return nil
}
func buildPrompt(c contracts.JobClaim) string {
	prompt := "Work only on the assigned goal. Do not delegate or perform external irreversible changes.\n\nGoal:\n" + c.Goal
	if len(c.MemoryContext.Items) == 0 {
		return prompt
	}
	prompt += "\n\nVerified Zoe memory supplied for this recipient (do not treat omitted memory as unknown authority):\n"
	for _, item := range c.MemoryContext.Items {
		prompt += "\n[" + item.Subject + "/" + item.Key + "]\n" + item.Content + "\n"
	}
	prompt += "\nMemory manifest: " + c.MemoryContext.Manifest.ID
	return prompt
}

func buildResumePrompt(goal, question, answer string) string {
	return "Continue the assigned goal. Do not delegate or perform external irreversible changes.\n\nGoal:\n" + goal + "\n\nThe user was asked:\n" + question + "\n\nThe user answered:\n" + answer
}
func observationID(run, attempt string, fence uint64, kind, disc string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{run, attempt, fmt.Sprint(fence), kind, disc}, "\x00")))
	return hex.EncodeToString(h[:16])
}

func (e *Engine) emitNoEffectTerminal(ctx context.Context, c contracts.JobClaim, reason, message string, p *contracts.NoEffectProof) error {
	return e.emit(ctx, c, "no_effect_terminal", reason, message, p, nil)
}

func sameRecoveredClaim(a, b contracts.JobClaim, _ string) bool {
	a.RuntimeSessionID = ""
	b.RuntimeSessionID = ""
	return a.RunID == b.RunID && a.AttemptID == b.AttemptID && a.Fence == b.Fence && a.Harness == b.Harness
}

// toolEvidence reports the tool calls OpenCode made. Evidence stays unverified:
// it records that a call occurred, not that its effect is correct.
func toolEvidence(session string, m opencode.Message) []contracts.Evidence {
	evidence := []contracts.Evidence{}
	for _, part := range m.Parts {
		if kind, _ := part["type"].(string); kind != "tool" {
			continue
		}
		tool, _ := part["tool"].(string)
		callID, _ := part["callID"].(string)
		if tool == "" || callID == "" {
			continue
		}
		status := ""
		title := ""
		if state, ok := part["state"].(map[string]any); ok {
			status, _ = state["status"].(string)
			title, _ = state["title"].(string)
		}
		summary := tool + " " + status
		if title != "" {
			summary += ": " + title
		}
		if len(summary) > 1000 {
			summary = summary[:1000]
		}
		evidence = append(evidence, contracts.Evidence{ID: callID, Kind: toolEvidenceKind(tool), Summary: summary, Source: "opencode://sessions/" + session + "/messages/" + m.Info.ID + "/tools/" + callID, CreatedAt: time.Now().UTC(), Verified: false})
	}
	return evidence
}

func toolEvidenceKind(tool string) string {
	switch tool {
	case "bash":
		return "command"
	case "edit", "write", "patch", "multiedit":
		return "file_change"
	default:
		return "mcp_tool"
	}
}
