package control

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
)

const defaultClientLogLevel = "error"

type ClientPolicy struct {
	LogLevel            string `json:"log_level"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
	Revision            int64  `json:"revision"`
}

func ensureClientPolicy(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) error {
	_, err := exec.ExecContext(ctx, `INSERT OR IGNORE INTO client_policy(singleton,log_level,poll_interval_seconds,revision,updated_at) VALUES(1,'error',60,1,?)`, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Server) ReadClientPolicy(ctx context.Context) (ClientPolicy, error) {
	if err := EnsureClientPolicy(ctx, s.store.DB()); err != nil {
		return ClientPolicy{}, err
	}
	var p ClientPolicy
	err := s.store.DB().QueryRowContext(ctx, `SELECT log_level,poll_interval_seconds,revision FROM client_policy WHERE singleton=1`).Scan(&p.LogLevel, &p.PollIntervalSeconds, &p.Revision)
	return p, err
}

func clampPollInterval(value int) int {
	if value == 0 {
		return 60
	}
	if value < 30 {
		return 30
	}
	if value > 600 {
		return 600
	}
	return value
}

func validClientLogLevel(level string) bool {
	return level == "error" || level == "warn" || level == "info" || level == "debug"
}

func (s *Server) UpdateClientPolicy(ctx context.Context, level string, interval int) (ClientPolicy, error) {
	return UpdateClientPolicy(ctx, s.store.DB(), level, interval)
}

func EnsureClientPolicy(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) error {
	return ensureClientPolicy(ctx, exec)
}

func ReadClientPolicy(ctx context.Context, db *sql.DB) (ClientPolicy, error) {
	if err := ensureClientPolicy(ctx, db); err != nil {
		return ClientPolicy{}, err
	}
	var p ClientPolicy
	err := db.QueryRowContext(ctx, `SELECT log_level,poll_interval_seconds,revision FROM client_policy WHERE singleton=1`).Scan(&p.LogLevel, &p.PollIntervalSeconds, &p.Revision)
	return p, err
}

func UpdateClientPolicy(ctx context.Context, db *sql.DB, level string, interval int) (ClientPolicy, error) {
	level = strings.ToLower(strings.TrimSpace(level))
	if !validClientLogLevel(level) {
		return ClientPolicy{}, apiError(http.StatusBadRequest, "invalid_policy", "log level must be error, warn, info, or debug", nil)
	}
	interval = clampPollInterval(interval)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ClientPolicy{}, err
	}
	defer tx.Rollback()
	if err = ensureClientPolicy(ctx, tx); err != nil {
		return ClientPolicy{}, err
	}
	var revision int64
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM client_policy WHERE singleton=1`).Scan(&revision); err != nil {
		return ClientPolicy{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE client_policy SET log_level=?,poll_interval_seconds=?,revision=?,updated_at=? WHERE singleton=1`, level, interval, revision+1, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return ClientPolicy{}, err
	}
	var p ClientPolicy
	if err = tx.QueryRowContext(ctx, `SELECT log_level,poll_interval_seconds,revision FROM client_policy WHERE singleton=1`).Scan(&p.LogLevel, &p.PollIntervalSeconds, &p.Revision); err != nil {
		return ClientPolicy{}, err
	}
	return p, tx.Commit()
}

func (s *Server) registerClientPolicyRoutes() {
	s.device.HandleFunc("GET /v1/client/policy", s.handleClientPolicy)
}

func (s *Server) handleClientPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "device authentication required", nil))
		return
	}
	if !HasScope(p, "client:read") && !HasScope(p, "client:command") {
		writeError(w, apiError(http.StatusForbidden, "forbidden", "client scope is required", nil))
		return
	}
	policy, err := s.ReadClientPolicy(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, policy)
}

func diagnosticLevelRank(level string) int {
	switch level {
	case "debug":
		return 3
	case "info":
		return 2
	case "warn":
		return 1
	default:
		return 0
	}
}
