package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/agents/opencode"
)

const OpenCodeWorkspaceWritePolicy = "opencode-workspace-write-v1"

var ErrRuntimeOutcomeUnknown = errors.New("runtime operation outcome is unknown")

const runtimeRunLabel = "io.zoe.run"
const runtimeManagedLabel = "io.zoe.managed"
const runtimeLabel = "io.zoe.runtime"

func RuntimeIdentity(runID, attempt, fence string) (string, map[string]string, error) {
	if runID == "" || attempt == "" || fence == "" {
		return "", nil, errors.New("runtime identity is incomplete")
	}
	return "zoe-run-" + sanitizeRuntimePart(runID) + "-" + sanitizeRuntimePart(attempt) + "-" + sanitizeRuntimePart(fence), map[string]string{runtimeManagedLabel: "true", runtimeLabel: "opencode", runtimeRunLabel: runID}, nil
}
func sanitizeRuntimePart(s string) string { return s }

type RuntimeSpec struct {
	RunID, AttemptID, Fence                                                string
	Model, Prompt, Workspace                                               string
	ClientRequestID, ProjectID, Harness, DisplayName, ApprovalMode, Effort string
	Agent                                                                  string
	ModelRouteID                                                           string
	Permissions                                                            []contracts.PermissionRule
}
type RuntimeBinding struct{ SessionID string }
type RuntimeCreateRejectedError struct{ Code string }

func (e RuntimeCreateRejectedError) Error() string { return "runtime create rejected: " + e.Code }
func (e RuntimeCreateRejectedError) Valid() bool {
	return e.Code == "invalid_request" || e.Code == "session_create_rejected"
}

type Runtime interface {
	Create(context.Context, RuntimeSpec) (RuntimeBinding, error)
	Prompt(context.Context, RuntimeBinding, string) error
	Status(context.Context, RuntimeBinding) (map[string]opencode.Status, error)
	Messages(context.Context, RuntimeBinding) ([]opencode.Message, error)
	Events(context.Context, RuntimeBinding) (<-chan opencode.Event, <-chan error)
	Abort(context.Context, RuntimeBinding) error
	Terminate(context.Context, RuntimeBinding) (string, error)
}
type ContextWindowProvider interface {
	ContextWindow(context.Context, string) (int64, error)
}
type RotationRecovery interface {
	FindRotationSession(context.Context, string, string) (RuntimeBinding, error)
}
type PendingRequests interface {
	PendingPermissions(context.Context, RuntimeBinding) ([]opencode.PermissionRequest, error)
	PendingForms(context.Context, RuntimeBinding) ([]opencode.FormRequest, error)
}
type PermissionReply interface {
	ReplyPermission(context.Context, RuntimeBinding, string, opencode.PermissionDecision, string) error
}
type FormReply interface {
	ReplyForm(context.Context, RuntimeBinding, string, map[string]any) error
}

type OpenCodeRuntime struct {
	BaseURL, Username, Password, WorkspaceRoot, Model string
	Client                                            *opencode.Client
	HTTPClient                                        *httpClientFactory
}
type httpClientFactory struct{}

func (r OpenCodeRuntime) client() (*opencode.Client, error) {
	if r.Client != nil {
		return r.Client, nil
	}
	return opencode.New(opencode.Config{BaseURL: r.BaseURL, Username: r.Username, Password: r.Password})
}
func (r OpenCodeRuntime) Create(ctx context.Context, s RuntimeSpec) (RuntimeBinding, error) {
	c, err := r.client()
	if err != nil {
		return RuntimeBinding{}, err
	}
	dir := s.Workspace
	if r.WorkspaceRoot != "" {
		dir = filepath.Join(r.WorkspaceRoot, s.RunID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return RuntimeBinding{}, err
		}
	}
	permissions := make([]map[string]string, len(s.Permissions))
	for i, rule := range s.Permissions {
		permissions[i] = map[string]string{"action": rule.Action, "resource": rule.Resource, "effect": rule.Effect}
	}
	providerID, modelID, _ := strings.Cut(s.Model, "/")
	if providerID == "" {
		providerID, modelID = s.ModelRouteID, s.Model
	}
	session, err := c.CreateSession(ctx, "zoe-run-"+s.RunID, opencode.SessionOptions{Agent: s.Agent, Model: map[string]string{"id": modelID, "providerID": providerID}, Directory: dir, Permissions: permissions})
	if err != nil {
		var httpErr *opencode.HTTPError
		if errors.As(err, &httpErr) && httpErr.Status >= 400 && httpErr.Status < 500 {
			return RuntimeBinding{}, RuntimeCreateRejectedError{Code: "session_create_rejected"}
		}
		return RuntimeBinding{}, err
	}
	return RuntimeBinding{SessionID: session.ID}, nil
}
func (r OpenCodeRuntime) FindRotationSession(ctx context.Context, runID, oldSession string) (RuntimeBinding, error) {
	c, err := r.client()
	if err != nil {
		return RuntimeBinding{}, err
	}
	sessions, err := c.Sessions(ctx)
	if err != nil {
		return RuntimeBinding{}, err
	}
	title := "zoe-run-" + runID
	for _, s := range sessions {
		if s.Title == title && s.ID != oldSession {
			return RuntimeBinding{SessionID: s.ID}, nil
		}
	}
	return RuntimeBinding{}, errors.New("replacement OpenCode session not found")
}
func (r OpenCodeRuntime) ContextWindow(ctx context.Context, model string) (int64, error) {
	c, err := r.client()
	if err != nil {
		return 0, err
	}
	providerID, modelID, ok := strings.Cut(model, "/")
	if !ok || providerID == "" || modelID == "" {
		return 0, errors.New("model must be provider/model")
	}
	return c.ModelContextWindow(ctx, providerID, modelID)
}
func (r OpenCodeRuntime) Prompt(ctx context.Context, b RuntimeBinding, p string) error {
	c, e := r.client()
	if e != nil {
		return e
	}
	return c.Prompt(ctx, b.SessionID, p)
}
func (r OpenCodeRuntime) Status(ctx context.Context, b RuntimeBinding) (map[string]opencode.Status, error) {
	c, e := r.client()
	if e != nil {
		return nil, e
	}
	return c.Active(ctx)
}
func (r OpenCodeRuntime) Messages(ctx context.Context, b RuntimeBinding) ([]opencode.Message, error) {
	c, e := r.client()
	if e != nil {
		return nil, e
	}
	return c.Messages(ctx, b.SessionID)
}
func (r OpenCodeRuntime) PendingPermissions(ctx context.Context, b RuntimeBinding) ([]opencode.PermissionRequest, error) {
	c, e := r.client()
	if e != nil {
		return nil, e
	}
	return c.PendingPermissions(ctx, b.SessionID)
}
func (r OpenCodeRuntime) PendingForms(ctx context.Context, b RuntimeBinding) ([]opencode.FormRequest, error) {
	c, e := r.client()
	if e != nil {
		return nil, e
	}
	return c.PendingForms(ctx, b.SessionID)
}
func (r OpenCodeRuntime) ReplyPermission(ctx context.Context, b RuntimeBinding, requestID string, decision opencode.PermissionDecision, message string) error {
	c, e := r.client()
	if e != nil {
		return e
	}
	return c.ReplyPermission(ctx, b.SessionID, requestID, decision, message)
}
func (r OpenCodeRuntime) ReplyForm(ctx context.Context, b RuntimeBinding, formID string, answer map[string]any) error {
	c, e := r.client()
	if e != nil {
		return e
	}
	return c.ReplyForm(ctx, b.SessionID, formID, answer)
}
func (r OpenCodeRuntime) Events(context.Context, RuntimeBinding) (<-chan opencode.Event, <-chan error) {
	events := make(chan opencode.Event)
	errs := make(chan error)
	close(events)
	close(errs)
	return events, errs
}
func (r OpenCodeRuntime) Abort(ctx context.Context, b RuntimeBinding) error {
	c, e := r.client()
	if e != nil {
		return e
	}
	return c.Interrupt(ctx, b.SessionID)
}
func (r OpenCodeRuntime) Terminate(ctx context.Context, b RuntimeBinding) (string, error) {
	c, e := r.client()
	if e != nil {
		return "", e
	}
	if err := c.Interrupt(ctx, b.SessionID); err != nil {
		return "", err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		active, err := c.Active(ctx)
		if err == nil {
			if _, ok := active[b.SessionID]; !ok {
				return b.SessionID, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", errors.New("OpenCode session remains active after interrupt")
}

func waitRuntime(ctx context.Context, r Runtime, b RuntimeBinding, interval time.Duration, onStep func() error) (opencode.Message, error) {
	if interval <= 0 {
		interval = time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return opencode.Message{}, ctx.Err()
		case <-time.After(interval):
		}
		if onStep != nil {
			if err := onStep(); err != nil {
				return opencode.Message{}, err
			}
		}
		msgs, err := r.Messages(ctx, b)
		if err != nil {
			continue
		}
		m, done, e := opencode.Completion(msgs)
		if done {
			return m, e
		}
	}
}
