package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/storage"
)

type Config struct {
	RunnerToken        string
	RunnerID           string
	RunnerWorkspaceIDs []string
	RunnerScopes       []string
	InstanceScopes     []string
	DefaultProfileID   string
	RoleProfiles       map[string]string
	RuntimeRouteID     string
	ModelRouteID       string
	Profiles           map[string]RuntimeProfile
	MaxRequestBytes    int64
	PushSender         PushSender
}

type RuntimeProfile struct {
	ID              string      `json:"id"`
	Harness         string      `json:"harness,omitempty"`
	RuntimeRouteID  string      `json:"runtime_route_id"`
	ModelRouteID    string      `json:"model_route_id"`
	Role            string      `json:"role"`
	IsolationPolicy string      `json:"isolation_policy"`
	CapabilityIDs   []string    `json:"capability_ids"`
	Scopes          []string    `json:"scopes"`
	ChildPolicy     ChildPolicy `json:"child_policy,omitempty"`
}

// ChildPolicy is a profile's bounded authority to request descendants. A task
// receives a serialized snapshot at admission, so later config changes cannot
// enlarge an existing lineage.
func normalizeIsolationPolicy(policy string) string {
	return policy
}

type ChildPolicy struct {
	AllowedProfileIDs []string `json:"allowed_profile_ids"`
	CapabilityIDs     []string `json:"capability_ids"`
	MaxChildRuns      int      `json:"max_child_runs"`
	MaxDepth          int      `json:"max_depth"`
}

type CommandContext struct {
	CommandID      string
	Principal      contracts.Principal
	IdempotencyKey string
	AcceptedAt     time.Time
	TrustedSource  bool `json:"-"`
}

type CommandHandler func(context.Context, *sql.Tx, CommandContext, contracts.CommandRequest) (contracts.CommandResult, []contracts.Event, error)

type TaskSourceRequest struct {
	Title          string
	Goal           string
	InitialMessage string
	TrustedSource  bool `json:"-"`
	GrantSnapshot  json.RawMessage
}

// AdmissionContext is supplied by trusted server-side features to admit a task
// inside their existing transaction. Principal must come from a stored grant,
// never from request JSON. IdempotencyKey should be a stable, server-derived ID.
type AdmissionContext struct {
	Principal      contracts.Principal
	Source         string
	SourceRef      json.RawMessage
	IdempotencyKey string
	GrantSnapshot  json.RawMessage
	ProfileID      string
	ProjectID      string
	Title          string
	Goal           string
	InitialMessage string
	AcceptedAt     time.Time
	TrustedSource  bool `json:"-"`
}

type TaskAdmissionResult struct {
	Task   contracts.Task
	Events []contracts.Event
}

// AppendEventsTx persists feature-produced events and their notifications in
// the caller's transaction. It is intended to pair with AdmitTaskTx.
func (s *Server) AppendEventsTx(ctx context.Context, tx *sql.Tx, p contracts.Principal, events []contracts.Event) error {
	if tx == nil || p.WorkspaceID == "" || p.SubjectID == "" {
		return errors.New("transaction and trusted event principal are required")
	}
	for i := range events {
		e := &events[i]
		if e.AggregateType == "" || e.AggregateID == "" {
			return apiError(500, "invalid_event", "event is missing aggregate identity", nil)
		}
		if e.EventID == "" {
			id, err := randomID()
			if err != nil {
				return err
			}
			e.EventID = id
		}
		e.SchemaVersion = 1
		e.WorkspaceID = p.WorkspaceID
		e.SubjectID = p.SubjectID
		if e.OccurredAt.IsZero() {
			e.OccurredAt = time.Now().UTC()
		}
		var cursor int64
		if err := tx.QueryRowContext(ctx, `UPDATE event_counters SET current_cursor=current_cursor+1 WHERE workspace_id=? RETURNING current_cursor`, p.WorkspaceID).Scan(&cursor); err != nil {
			return err
		}
		e.Cursor = fmt.Sprint(cursor)
		payload, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,subject_id,aggregate_type,aggregate_id,aggregate_version,payload) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, e.EventID, p.WorkspaceID, cursor, e.SchemaVersion, e.Type, e.OccurredAt.Format(time.RFC3339Nano), p.SubjectID, e.AggregateType, e.AggregateID, e.AggregateVersion, payload); err != nil {
			return err
		}
		if err = enqueueEventNotification(ctx, tx, *e); err != nil {
			return err
		}
	}
	return nil
}

type TaskSourceAdmissionHandler func(context.Context, *sql.Tx, contracts.Principal, string, *TaskSourceRequest, json.RawMessage, time.Time) ([]contracts.Event, error)

// TaskDispatchGuard decides whether a trusted non-user task source remains
// authorized immediately before a runner creates a runtime. A non-empty
// reason code denies dispatch. Implementations may update their source-owned
// records inside tx so cancellation and the task transition commit together.
type TaskDispatchGuard func(context.Context, *sql.Tx, string, string, time.Time) (reasonCode string, err error)

// PrincipalCanAccessProject/Task are the authoritative row-level access checks
// for feature modules running within a device transaction.
func PrincipalCanAccessProject(ctx context.Context, tx *sql.Tx, p contracts.Principal, projectID string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM projects p WHERE p.id=? AND p.workspace_id=? AND p.status='active' AND (p.owner_subject_id=?)`, projectID, p.WorkspaceID, p.SubjectID).Scan(&count)
	return count == 1, err
}

func PrincipalCanWriteProject(ctx context.Context, tx *sql.Tx, p contracts.Principal, projectID string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM projects p WHERE p.id=? AND p.workspace_id=? AND p.status='active' AND (p.owner_subject_id=?)`, projectID, p.WorkspaceID, p.SubjectID).Scan(&count)
	return count == 1, err
}

func PrincipalCanAccessTask(ctx context.Context, tx *sql.Tx, p contracts.Principal, taskID string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks t WHERE t.id=? AND t.workspace_id=? AND (t.owner_subject_id=?)`, taskID, p.WorkspaceID, p.SubjectID).Scan(&count)
	return count == 1, err
}

func PrincipalCanWriteTask(ctx context.Context, tx *sql.Tx, p contracts.Principal, taskID string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks t WHERE t.id=? AND t.workspace_id=? AND (t.owner_subject_id=?)`, taskID, p.WorkspaceID, p.SubjectID).Scan(&count)
	return count == 1, err
}

type APIError struct {
	Status  int
	Code    string
	Message string
	Details any
	Cause   error
}

func (e *APIError) Error() string {
	if e.Cause != nil {
		return e.Code + ": " + e.Cause.Error()
	}
	return e.Code + ": " + e.Message
}
func (e *APIError) Unwrap() error { return e.Cause }

func apiError(status int, code, message string, details any) *APIError {
	return &APIError{Status: status, Code: code, Message: message, Details: details}
}

func NewAPIError(status int, code, message string, details any) *APIError {
	return apiError(status, code, message, details)
}

type Server struct {
	store              *storage.Store
	cfg                Config
	commands           map[string]CommandHandler
	taskSources        map[string]TaskSourceAdmissionHandler
	taskDispatchGuards map[string]TaskDispatchGuard
	public             *http.ServeMux
	device             *http.ServeMux
	runner             *http.ServeMux
	mu                 sync.RWMutex
	sealed             bool
	pushSender         PushSender
	shutdownHooks      []func()
}

// RegisterShutdownHook registers a feature-owned background worker stop hook.
func (s *Server) RegisterShutdownHook(hook func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdownHooks = append(s.shutdownHooks, hook)
}

// Shutdown stops feature-owned background workers. It is safe to call once.
func (s *Server) Shutdown() {
	s.mu.Lock()
	hooks := append([]func(){}, s.shutdownHooks...)
	s.shutdownHooks = nil
	s.mu.Unlock()
	for _, hook := range hooks {
		hook()
	}
}

func DefaultOwnerScopes() []string {
	return []string{
		"client:read", "client:command", "device:push", "capability:deployment:revoke",
		"export:read", "instance:admin", "client:admin", "capability:read", "capability:release:submit", "capability:deployment:request", "capability:deployment:authorize", "voice:read", "voice:open", "voice:pool:reconcile", "voice:turn:commit", "memory:context:any",
		"memory:read:personal", "memory:write:personal", "memory:delete:personal",
		"memory:read:workspace", "memory:write:workspace", "memory:delete:workspace",
	}
}

func NewServer(store *storage.Store, cfg Config) (*Server, error) {
	if store == nil || store.DB() == nil {
		return nil, errors.New("control server requires an open store")
	}

	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = 1 << 20
	}
	profiles := make(map[string]RuntimeProfile, len(cfg.Profiles))
	for id, profile := range cfg.Profiles {
		profile.IsolationPolicy = normalizeIsolationPolicy(profile.IsolationPolicy)
		profile.Harness = strings.TrimSpace(profile.Harness)
		if profile.Harness == "" {
			profile.Harness = "opencode"
		}
		profile.CapabilityIDs = append([]string(nil), profile.CapabilityIDs...)
		profile.Scopes = append([]string(nil), profile.Scopes...)
		profile.ChildPolicy.AllowedProfileIDs = append([]string(nil), profile.ChildPolicy.AllowedProfileIDs...)
		profile.ChildPolicy.CapabilityIDs = append([]string(nil), profile.ChildPolicy.CapabilityIDs...)
		profiles[id] = profile
	}
	cfg.Profiles = profiles
	roleProfiles := make(map[string]string, len(cfg.RoleProfiles))
	for role, profileID := range cfg.RoleProfiles {
		role, profileID = strings.TrimSpace(role), strings.TrimSpace(profileID)
		profile, ok := cfg.Profiles[profileID]
		if role == "" || !ok || profile.Role != role {
			return nil, fmt.Errorf("role profile mapping %q -> %q must reference a configured worker profile with the same role", role, profileID)
		}
		if _, exists := roleProfiles[role]; exists {
			return nil, fmt.Errorf("role profile mapping for normalized role %q is defined more than once", role)
		}
		roleProfiles[role] = profileID
	}
	cfg.RoleProfiles = roleProfiles
	if cfg.InstanceScopes == nil {
		cfg.InstanceScopes = DefaultOwnerScopes()
	} else {
		cfg.InstanceScopes = append([]string(nil), cfg.InstanceScopes...)
	}
	for _, scope := range cfg.InstanceScopes {
		if strings.TrimSpace(scope) == "" || scope == "*" {
			return nil, errors.New("instance scopes must be explicit non-empty grants without wildcard")
		}
	}
	for id, profile := range cfg.Profiles {
		if profile.Harness != "opencode" {
			return nil, fmt.Errorf("runtime profile %q selects unsupported harness %q", id, profile.Harness)
		}
		if profile.IsolationPolicy != "opencode-workspace-write-v1" {
			return nil, fmt.Errorf("runtime profile %q selects OpenCode with an incompatible isolation policy", id)
		}
		if id == "" || profile.ID != id || profile.RuntimeRouteID == "" || profile.ModelRouteID == "" || profile.Role == "" || profile.IsolationPolicy == "" {
			return nil, fmt.Errorf("runtime profile %q is incomplete or does not select worker execution", id)
		}
		policy := profile.ChildPolicy
		if policy.MaxChildRuns == 0 {
			policy.MaxChildRuns = 4
		}
		if policy.MaxDepth == 0 {
			policy.MaxDepth = 4
		}
		if policy.MaxChildRuns < 1 || policy.MaxChildRuns > 64 || policy.MaxDepth < 1 || policy.MaxDepth > 8 {
			return nil, fmt.Errorf("runtime profile %q child policy limits are outside supported bounds", id)
		}
		policy.AllowedProfileIDs = uniqueNonEmpty(policy.AllowedProfileIDs)
		policy.CapabilityIDs = uniqueNonEmpty(policy.CapabilityIDs)
		for _, allowed := range policy.AllowedProfileIDs {
			if _, ok := cfg.Profiles[allowed]; !ok {
				return nil, fmt.Errorf("runtime profile %q child policy references unknown profile %q", id, allowed)
			}
		}
		for _, capability := range policy.CapabilityIDs {
			if !containsString(profile.CapabilityIDs, capability) {
				return nil, fmt.Errorf("runtime profile %q child policy grants capability %q absent from its own grants", id, capability)
			}
		}
		profile.ChildPolicy = policy
		cfg.Profiles[id] = profile
	}
	if cfg.DefaultProfileID != "" {
		if _, ok := cfg.Profiles[cfg.DefaultProfileID]; !ok {
			return nil, errors.New("default profile is not present in configured profiles")
		}
	}
	s := &Server{store: store, cfg: cfg, commands: make(map[string]CommandHandler), taskSources: make(map[string]TaskSourceAdmissionHandler), taskDispatchGuards: make(map[string]TaskDispatchGuard), public: http.NewServeMux(), device: http.NewServeMux(), runner: http.NewServeMux(), pushSender: cfg.PushSender}
	s.registerCoreRoutes()
	s.registerDiagnosticsRoute()
	s.registerClientPolicyRoutes()
	return s, nil
}

func (s *Server) ProfileForRole(role string) (string, bool) {
	role = strings.TrimSpace(role)
	if role == "" {
		return "", false
	}
	if profileID, ok := s.cfg.RoleProfiles[role]; ok {
		return profileID, true
	}
	profileID := ""
	for id, profile := range s.cfg.Profiles {
		if profile.Role != role {
			continue
		}
		if profileID != "" {
			return "", false
		}
		profileID = id
	}
	return profileID, profileID != ""
}

// RegisterTaskSourceHandler admits non-user task sources inside the same SQL
// transaction as task creation, receipt, and outbox events.
func (s *Server) RegisterTaskSourceHandler(source string, handler TaskSourceAdmissionHandler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return errors.New("server registrations are sealed")
	}
	source = strings.TrimSpace(source)
	if source == "" || source == "user" || handler == nil {
		return errors.New("task source and admission handler are required; user source is built in")
	}
	if _, ok := s.taskSources[source]; ok {
		return fmt.Errorf("task source %q already registered", source)
	}
	s.taskSources[source] = handler
	return nil
}

func (s *Server) RegisterCommand(kind string, h CommandHandler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return errors.New("server registrations are sealed")
	}
	if strings.TrimSpace(kind) == "" || h == nil {
		return errors.New("command kind and handler are required")
	}
	if _, ok := s.commands[kind]; ok {
		return fmt.Errorf("command %q already registered", kind)
	}
	s.commands[kind] = h
	return nil
}

// RegisterAuthenticatedRoutes registers feature routes behind device bearer auth.
func (s *Server) RegisterAuthenticatedRoutes(register func(*http.ServeMux)) error {
	if register == nil {
		return errors.New("route registration callback is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return errors.New("server registrations are sealed")
	}
	register(s.device)
	return nil
}

// RegisterRunnerRoutes registers runner routes behind the independent runner credential.
func (s *Server) RegisterRunnerRoutes(register func(*http.ServeMux)) error {
	if register == nil {
		return errors.New("route registration callback is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return errors.New("server registrations are sealed")
	}
	register(s.runner)
	return nil
}

func HasScope(p contracts.Principal, scope string) bool {
	for _, value := range p.Scopes {
		if value == scope || value == "*" {
			return true
		}
	}
	return false
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func HasRunnerScope(p contracts.RunnerPrincipal, scope, workspaceID string) bool {
	allowed := false
	for _, value := range p.Scopes {
		if value == scope || value == "*" {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	for _, value := range p.WorkspaceIDs {
		if value == workspaceID || value == "*" {
			return true
		}
	}
	return false
}

// DeviceTransaction binds a read or write transaction to the authenticated principal in ctx.
// Mutating feature handlers should prefer CommandHandler so receipt and outbox remain atomic.
func (s *Server) DeviceTransaction(ctx context.Context, fn func(*sql.Tx, contracts.Principal) error) error {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil)
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = requireOwnerSessionTx(ctx, tx, p); err != nil {
		return err
	}

	if err = fn(tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

// RunnerTransaction binds a transaction to the runner identity, workspace allowlist, and scopes in ctx.
func (s *Server) RunnerTransaction(ctx context.Context, fn func(*sql.Tx, contracts.RunnerPrincipal) error) error {
	p, ok := RunnerPrincipalFromContext(ctx)
	if !ok {
		return apiError(http.StatusUnauthorized, "unauthenticated", "runner authentication required", nil)
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) Handler() http.Handler {
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock()
	root := http.NewServeMux()
	// Authentication is intentionally a single instance bearer. Password, device
	// login, pairing, approval, and logout endpoints are not registered.
	root.Handle("/v1/", s.instanceAuth(s.restoreDeletionGate(s.device)))
	root.Handle("/runner/v1/", s.runnerAuth(s.restoreDeletionGate(s.runner)))
	root.Handle("/healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	root.Handle("/readyz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var present int
		err := s.store.DB().QueryRowContext(r.Context(), `SELECT count(*) FROM instance_auth WHERE singleton=1`).Scan(&present)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "pending", "reason": "instance_key_not_initialized"})
			return
		}
		if present != 1 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "pending", "reason": "instance_key_not_initialized"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}))
	root.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, apiError(http.StatusNotFound, "not_found", "route not found", nil))
	}))
	limited := requestLimit(s.cfg.MaxRequestBytes, root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set this outside routing and authentication so failures, including
		// stale sessions and unsupported methods, cannot be cached either.
		if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/runner/v1/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		limited.ServeHTTP(w, r)
	})
}

func PrincipalFromContext(ctx context.Context) (contracts.Principal, bool) {
	p, ok := ctx.Value(devicePrincipalKey{}).(contracts.Principal)
	return p, ok
}

func RunnerPrincipalFromContext(ctx context.Context) (contracts.RunnerPrincipal, bool) {
	p, ok := ctx.Value(runnerPrincipalKey{}).(contracts.RunnerPrincipal)
	return p, ok
}

type devicePrincipalKey struct{}
type runnerPrincipalKey struct{}

func writeError(w http.ResponseWriter, err error) {
	var api *APIError
	if !errors.As(err, &api) {
		api = apiError(http.StatusInternalServerError, "internal", "internal server error", nil)
	}
	writeJSON(w, api.Status, contracts.ErrorEnvelope{Error: contracts.APIError{Code: api.Code, Message: api.Message, Details: api.Details}})
}

func WriteError(w http.ResponseWriter, err error) { writeError(w, err) }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apiError(http.StatusBadRequest, "invalid_request", "invalid JSON request", nil)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return apiError(http.StatusBadRequest, "invalid_request", "request must contain one JSON value", nil)
	}
	return nil
}

func requestLimit(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
