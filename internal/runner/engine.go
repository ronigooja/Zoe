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

func (e *Engine) Process(ctx context.Context, claim contracts.JobClaim) error {
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
		case "waiting_permission", "waiting_question":
			if entry.Session == "" {
				return fmt.Errorf("%w: waiting runtime identity incomplete", ErrOutcomePending)
			}
			if entry.Phase == "waiting_question" {
				if authc, ok := e.Control.(runtimeAuthorizationControl); ok {
					if auth, err := authc.RuntimeAuthorization(ctx, claim.RunID, "", claim.Fence); err == nil && auth != nil && auth.Decision == "answer" {
						if err := e.Runtime.Prompt(ctx, RuntimeBinding{SessionID: entry.Session}, auth.Message); err != nil {
							return err
						}
						_ = e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "prompt_sent")
					}
				}
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
	spec := RuntimeSpec{RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: fmt.Sprint(claim.Fence), Model: route.Model, ModelRouteID: claim.ModelRouteID, Agent: claim.Agent, Permissions: claim.PermissionRules, Prompt: buildPrompt(claim), Workspace: claim.WorkspaceID}
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
		if !claim.Budget.HardDeadline.IsZero() && time.Now().UTC().After(claim.Budget.HardDeadline) {
			_ = e.Runtime.Abort(ctx, b)
			return e.terminate(ctx, claim, b, "run_deadline_exceeded", false)
		}
		status, err := e.Runtime.Status(ctx, b)
		if err != nil {
			time.Sleep(e.interval())
			continue
		}
		if pending, ok := e.Runtime.(PendingRequests); ok {
			if requests, reqErr := pending.PendingPermissions(ctx, b); reqErr == nil && len(requests) > 0 {
				r := requests[0]
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
				if err := e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "waiting_permission"); err != nil {
					return err
				}
				return e.emitPending(ctx, claim, "permission_requested", map[string]any{"request_id": r.ID, "action": r.Action, "resources": r.Resources, "metadata": r.Metadata})
			}
			if forms, reqErr := pending.PendingForms(ctx, b); reqErr == nil && len(forms) > 0 {
				f := forms[0]
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
				if err := e.Journal.SetPhase(ctx, claim.RunID, claim.AttemptID, claim.Fence, "waiting_question"); err != nil {
					return err
				}
				return e.emitPending(ctx, claim, "worker_question", map[string]any{"request_id": f.ID, "question": question, "title": f.Title})
			}
		}
		msgs, err := e.Runtime.Messages(ctx, b)
		if err != nil {
			time.Sleep(e.interval())
			continue
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

func (e *Engine) emitPending(ctx context.Context, c contracts.JobClaim, kind string, payload map[string]any) error {
	raw, _ := json.Marshal(payload)
	entry, _, _ := e.Journal.Entry(ctx, c.RunID)
	o := contracts.RunnerObservation{ObservationID: observationID(c.RunID, c.AttemptID, c.Fence, kind, string(raw)), RunID: c.RunID, AttemptID: c.AttemptID, Fence: c.Fence, Kind: kind, OccurredAt: time.Now().UTC(), RuntimeSessionID: entry.Session, Result: raw}
	if err := e.Journal.QueueObservation(ctx, c.RunID, o); err != nil {
		return err
	}
	return e.Control.Observe(ctx, c.RunID, o)
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
	o := contracts.RunnerObservation{ObservationID: hex.EncodeToString(sum[:16]), RunID: c.RunID, AttemptID: c.AttemptID, Fence: c.Fence, Kind: kind, OccurredAt: time.Now().UTC(), RuntimeSessionID: entry.Session, RuntimeContainerID: entry.Container, Result: result, ExternalEffects: effects}
	if p, ok := proof.(*contracts.TerminationProof); ok {
		o.TerminationProof = p
	}
	if p, ok := proof.(*contracts.CreateRejectionProof); ok {
		o.CreateRejection = p
	}
	if p, ok := proof.(*contracts.NoEffectProof); ok {
		o.NoEffectProof = p
	}
	if err := e.Journal.QueueObservation(ctx, c.RunID, o); err != nil {
		return err
	}
	if e.Control == nil {
		return nil
	}
	return e.Control.Observe(ctx, c.RunID, o)
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
	o := contracts.RunnerObservation{ObservationID: hex.EncodeToString(sum[:16]), RunID: c.RunID, AttemptID: c.AttemptID, Fence: c.Fence, Kind: kind, OccurredAt: time.Now().UTC(), RuntimeSessionID: entry.Session, RuntimeContainerID: entry.Container, Result: raw, ExternalEffects: []contracts.ExternalEffect{}, Evidence: evidence}
	if err := e.Journal.QueueObservation(ctx, c.RunID, o); err != nil {
		return err
	}
	return e.Control.Observe(ctx, c.RunID, o)
}
func (e *Engine) flush(ctx context.Context, run string) error {
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
	return "Work only on the assigned goal. Do not delegate or perform external irreversible changes.\n\nGoal:\n" + c.Goal
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
