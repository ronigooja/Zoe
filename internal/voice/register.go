package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/steward"
)

type RegisterOwner struct{ WorkspaceID, OwnerSubjectID string }

// RegisterModelConfig supplies model settings directly; no process environment is read.
type RegisterModelConfig struct {
	BaseURL, Model, APIKey, ReasoningEffort, Thinking string
	Timeout                                           time.Duration
}
type RegisterConfig struct {
	RegisterInstruction    string
	CompactInstruction     string
	TaskSummaryInstruction string
	Model                  RegisterModelConfig
	CompactModel           RegisterModelConfig
	RawWindow              time.Duration
	MemoryBoundary         func(context.Context, RegisterOwner) (time.Time, error)
	CompactCharacters      int
	Interval               time.Duration
}
type ContextRegister struct {
	registerInstruction    string
	compactInstruction     string
	taskSummaryInstruction string
	db                     *sql.DB
	retry                  *backoff.Store
	requestPublish         func()
	model                  *modelclient.Client
	compactModel           *modelclient.Client
	compactTimeout         time.Duration
	rawWindow              time.Duration
	boundary               func(context.Context, RegisterOwner) (time.Time, error)
	limit                  int
	interval               time.Duration
	mu                     sync.Mutex
	cancel                 context.CancelFunc
	done                   chan struct{}
	retryJitter            func() float64
	retrySleep             func(context.Context, time.Duration) error
}

func NewRegister(db *sql.DB, cfg RegisterConfig) (*ContextRegister, error) {
	if db == nil || cfg.CompactCharacters <= 0 || cfg.Interval < 0 || cfg.RawWindow < 0 {
		return nil, errors.New("invalid register configuration")
	}
	if cfg.Interval == 0 {
		cfg.Interval = time.Hour
	}
	if cfg.RawWindow == 0 {
		cfg.RawWindow = time.Hour
	}
	build := func(prefix string, model RegisterModelConfig, def, min, max int) (*modelclient.Client, error) {
		if strings.TrimSpace(model.BaseURL) == "" {
			return nil, fmt.Errorf("%s_BASE_URL is required", prefix)
		}
		values := map[string]string{prefix + "_BASE_URL": model.BaseURL, prefix + "_MODEL": model.Model, prefix + "_API_KEY_FILE": "configured", prefix + "_REASONING_EFFORT": model.ReasoningEffort, prefix + "_THINKING": model.Thinking}
		if model.Timeout != 0 {
			values[prefix+"_TIMEOUT_MS"] = fmt.Sprint(model.Timeout.Milliseconds())
		}
		return modelclient.FromEnv(prefix, func(k string) string { return values[k] }, func(string) ([]byte, error) { return []byte(model.APIKey), nil }, def, min, max)
	}
	client, err := build("ZOE_REGISTER", cfg.Model, 30000, 500, 60000)
	if err != nil {
		return nil, err
	}
	compact, err := build("ZOE_COMPACT", cfg.CompactModel, 600000, 5000, 600000)
	if err != nil {
		return nil, err
	}
	if cfg.CompactModel.Timeout == 0 {
		cfg.CompactModel.Timeout = 10 * time.Minute
	}
	return &ContextRegister{registerInstruction: cfg.RegisterInstruction, compactInstruction: cfg.CompactInstruction, taskSummaryInstruction: cfg.TaskSummaryInstruction, db: db, model: client, compactModel: compact, compactTimeout: cfg.CompactModel.Timeout, rawWindow: cfg.RawWindow, boundary: cfg.MemoryBoundary, limit: cfg.CompactCharacters, interval: cfg.Interval}, nil
}

type RouteResult struct {
	Routes  []string `json:"routes"`
	Request *string  `json:"request"`
}

func (r RouteResult) Has(name string) bool {
	for _, exit := range r.Routes {
		if exit == name {
			return true
		}
	}
	return false
}

func (r RouteResult) Exits() string {
	exits := append([]string(nil), r.Routes...)
	sort.Strings(exits)
	return strings.Join(exits, ",")
}

func (r *ContextRegister) Route(ctx context.Context, owner RegisterOwner, sentence string, now time.Time) (result RouteResult, resultErr error) {
	started := time.Now()
	kind := "db"
	defer func() {
		status, route := "ok", result.Exits()
		if resultErr != nil {
			status, route = "error", "none"
		} else {
			kind = "none"
		}
		logging.Infof("register_call status=%s route=%s elapsed_ms=%d error_kind=%s", status, route, time.Since(started).Milliseconds(), kind)
		if resultErr != nil {
			logging.Errorf("register status=route_failed error_kind=%s", kind)
		}
	}()
	if validRegisterOwner(owner) != nil {
		kind = "config"
	}
	layers, err := r.Layers(ctx, owner, now)
	if err != nil {
		return RouteResult{}, err
	}
	kind = "other"
	input := "Compact:\n" + layers.Compact + "\nRaw:\n" + layers.Raw + "\nCurrent sentence:\n" + sentence
	call := logging.NewModelCall("register", logging.UserHash(owner.OwnerSubjectID), 1, "")
	call.Input(r.registerInstruction, input)
	defer func() {
		if resultErr == nil {
			kind = "none"
		}
		call.Result(result, kind)
	}()
	output, err := steward.RetryComplete(ctx, func(ctx context.Context) (string, error) {
		return r.model.Complete(ctx, r.registerInstruction, input)
	}, r.retryJitter, r.retrySleep)
	call.Output(output)
	if err != nil {
		kind = registerModelErrorKind(ctx, r.model, err)
		return RouteResult{}, err
	}
	kind = "output_invalid"
	return parseRegisterRoute(output)
}

// registerModelErrorKind classifies model failures without logging error text or payloads.
func registerModelErrorKind(ctx context.Context, model *modelclient.Client, err error) string {
	if model == nil {
		return "config"
	}
	var httpErr *modelclient.HTTPError
	if errors.As(err, &httpErr) {
		return "model_http"
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "model_timeout"
	}
	switch err.Error() {
	case "invalid model response", "model response was incomplete":
		return "output_invalid"
	default:
		return "model_transport"
	}
}

func parseRegisterRoute(output string) (RouteResult, error) {
	bad := func() (RouteResult, error) { return RouteResult{}, errors.New("invalid register route") }
	d := json.NewDecoder(strings.NewReader(output))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return bad()
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return bad()
		}
		name, ok := token.(string)
		if !ok {
			return bad()
		}
		if _, exists := fields[name]; exists {
			return bad()
		}
		var value json.RawMessage
		if d.Decode(&value) != nil || len(value) == 0 {
			return bad()
		}
		fields[name] = value
	}
	if _, err = d.Token(); err != nil {
		return bad()
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return bad()
	}
	rawRoutes, hasRoute := fields["routes"]
	rawText, hasText := fields["request"]
	if len(fields) != 2 || !hasRoute || !hasText {
		return bad()
	}
	var routes []string
	var text *string
	if len(rawRoutes) == 0 || rawRoutes[0] != '[' || json.Unmarshal(rawRoutes, &routes) != nil || len(routes) == 0 || json.Unmarshal(rawText, &text) != nil {
		return bad()
	}
	seen := map[string]bool{}
	for _, route := range routes {
		if seen[route] {
			return bad()
		}
		seen[route] = true
		switch route {
		case "chat", "memory_write", "recall":
			if len(routes) != 1 {
				return bad()
			}
		case "task", "ui":
		default:
			return bad()
		}
	}
	if seen["chat"] {
		if text != nil {
			return bad()
		}
	} else {
		if text == nil || strings.TrimSpace(*text) == "" || strings.EqualFold(strings.TrimSpace(*text), "none") || !utf8.ValidString(*text) {
			return bad()
		}
	}
	// Semantic completeness and fidelity require model-quality acceptance.
	return RouteResult{Routes: routes, Request: text}, nil
}

type RegisterLayers struct {
	MemoryBoundary, CompactFrom, CompactThrough, RawFrom, BuiltAt time.Time
	Compact, Raw                                                  string
	Version                                                       int64
}
type registerTurn struct {
	at         time.Time
	received   time.Time
	eligible   bool
	session    string
	seq        int64
	role, text string
}
type registerState struct {
	RegisterLayers
	hash        string
	turns       []registerTurn
	sourceCount int
}

const registerEpoch = "1970-01-01T00:00:00Z"

func validRegisterOwner(o RegisterOwner) error {
	if o.WorkspaceID == "" || o.OwnerSubjectID == "" {
		return errors.New("register owner is required")
	}
	return nil
}

func (r *ContextRegister) snapshot(ctx context.Context, o RegisterOwner, now time.Time) (registerState, error) {
	var s registerState
	if err := validRegisterOwner(o); err != nil {
		return s, err
	}
	boundary, err := r.memoryBoundary(ctx, o)
	if err != nil {
		return s, err
	}
	s.MemoryBoundary = boundary
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s, err
	}
	defer tx.Rollback()
	edge, built, used := time.Time{}.Format(time.RFC3339Nano), registerEpoch, ""
	err = tx.QueryRowContext(ctx, `SELECT compact,compact_through,built_at,version,source_hash,source_memory_boundary FROM voice_context_registers WHERE workspace_id=? AND owner_subject_id=?`, o.WorkspaceID, o.OwnerSubjectID).Scan(&s.Compact, &edge, &built, &s.Version, &s.hash, &used)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	s.CompactThrough, err = time.Parse(time.RFC3339Nano, edge)
	if err != nil {
		return s, err
	}
	s.BuiltAt, err = time.Parse(time.RFC3339Nano, built)
	if err != nil {
		return s, err
	}
	// M is current ingestion state, not permission to discard retained coverage.
	s.CompactFrom = s.MemoryBoundary
	if s.Version > 0 {
		s.CompactFrom, err = time.Parse(time.RFC3339Nano, used)
		if err != nil {
			return s, err
		}
	} else {
		s.CompactThrough = s.MemoryBoundary
	}
	s.RawFrom = s.CompactThrough
	consumed := map[string]bool{}
	var membership bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='memory_ingest_members')`).Scan(&membership); err != nil {
		return s, err
	}
	if membership {
		members, err := tx.QueryContext(ctx, `SELECT m.source_id FROM memory_ingest_members m JOIN memory_ingest_batches b ON b.id=m.batch_id AND b.workspace_id=m.workspace_id AND b.principal_id=m.principal_id WHERE m.workspace_id=? AND m.principal_id=? AND b.until_ns<=?`, o.WorkspaceID, o.OwnerSubjectID, s.MemoryBoundary.UnixNano())
		if err != nil {
			return s, err
		}
		for members.Next() {
			var id string
			if err := members.Scan(&id); err != nil {
				members.Close()
				return s, err
			}
			consumed[id] = true
		}
		err = members.Err()
		members.Close()
		if err != nil {
			return s, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.at,t.received_at,t.session_id,t.seq,t.role,t.text FROM voice_transcript_turns t JOIN voice_sessions v ON v.id=t.session_id AND v.workspace_id=t.workspace_id WHERE v.workspace_id=? AND v.owner_id=?`, o.WorkspaceID, o.OwnerSubjectID)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		s.sourceCount++
		var t registerTurn
		var at, received string
		if err = rows.Scan(&at, &received, &t.session, &t.seq, &t.role, &t.text); err != nil {
			rows.Close()
			return s, err
		}
		t.at, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			rows.Close()
			return s, err
		}
		t.received, err = time.Parse(time.RFC3339Nano, received)
		if err != nil {
			rows.Close()
			return s, err
		}
		// Keep all raw coverage after the retained edge, and late un-ingested
		// records even when their semantic timestamp precedes M.
		unseen := membership && !consumed[fmt.Sprintf("turn:%q:%d", t.session, t.seq)]
		t.eligible = !t.at.Before(s.MemoryBoundary) || unseen
		if (!t.at.Before(s.MemoryBoundary) || !t.at.Before(s.RawFrom) || unseen) && !t.at.After(now) {
			s.turns = append(s.turns, t)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s, err
	}
	if err = tx.Commit(); err != nil {
		return s, err
	}
	sort.Slice(s.turns, func(i, j int) bool {
		a, b := s.turns[i], s.turns[j]
		if !a.at.Equal(b.at) {
			return a.at.Before(b.at)
		}
		if a.session != b.session {
			return a.session < b.session
		}
		return a.seq < b.seq
	})
	for _, t := range s.turns {
		if !t.at.Before(s.RawFrom) || t.received.After(s.BuiltAt) || (s.Version == 0 && t.at.Before(s.MemoryBoundary)) {
			s.Raw += registerLine(t)
		}
	}
	return s, nil
}
func registerLine(t registerTurn) string {
	return fmt.Sprintf("[%s] %s: %s\n", t.at.UTC().Format(time.RFC3339Nano), t.role, t.text)
}

func (r *ContextRegister) Layers(ctx context.Context, owner RegisterOwner, now time.Time) (RegisterLayers, error) {
	s, err := r.snapshot(ctx, owner, now)
	return s.RegisterLayers, err
}

// AssembleContext returns at most budget Unicode characters, retaining the newest raw text first.
func (r *ContextRegister) AssembleContext(ctx context.Context, owner RegisterOwner, now time.Time, budget int) (string, error) {
	if budget < 0 {
		return "", errors.New("negative register context budget")
	}
	s, err := r.Layers(ctx, owner, now)
	if err != nil {
		return "", err
	}
	text := s.Compact
	if text != "" && s.Raw != "" {
		text += "\n"
	}
	text += s.Raw
	if utf8.RuneCountInString(text) > budget {
		runes := []rune(text)
		text = string(runes[len(runes)-budget:])
	}
	return text, nil
}

func (r *ContextRegister) memoryBoundary(ctx context.Context, owner RegisterOwner) (time.Time, error) {
	if r.boundary == nil {
		return time.Time{}, nil
	}
	return r.boundary(ctx, owner)
}
