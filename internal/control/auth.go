package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/storage"
)

func tokenBytes(token string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != token {
		return nil, errors.New("invalid token encoding")
	}
	return b, nil
}

func tokenHash(token string) ([]byte, error) { return instanceKeyHash(token) }

func bearer(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if len(value) < 8 || !strings.EqualFold(value[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(value[7:])
}

// GenerateInstanceKey returns a 256-bit URL-safe bearer suitable for QR import.
func GenerateInstanceKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func instanceKeyHash(key string) ([]byte, error) {
	b, err := tokenBytes(key)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// InitializeInstanceAuth provisions or verifies a deployment-provided key.
// Key generation and secret export are reserved for explicit operator commands.
func InitializeInstanceAuth(ctx context.Context, store *storage.Store, configured string) (string, bool, error) {
	return initializeInstanceAuth(ctx, store, configured, false)
}

// InitializeInstanceAuthPending provisions or verifies a deployment-provided
// key, while allowing a fresh deployment to start before explicit key init.
func InitializeInstanceAuthPending(ctx context.Context, store *storage.Store, configured string) (string, bool, error) {
	return initializeInstanceAuth(ctx, store, configured, true)
}

func initializeInstanceAuth(ctx context.Context, store *storage.Store, configured string, allowPending bool) (string, bool, error) {
	if store == nil || store.DB() == nil {
		return "", false, errors.New("open control store is required")
	}
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if _, err := instanceKeyHash(configured); err != nil {
			return "", false, errors.New("instance key must be a 32-byte base64url credential")
		}
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var hash []byte
	err = tx.QueryRowContext(ctx, `SELECT key_hash FROM instance_auth WHERE singleton=1`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		key := configured
		generated := false
		if key == "" {
			if allowPending {
				return "", false, tx.Commit()
			}
			return "", false, errors.New("instance key is not initialized; run zoe key init with an explicit secure output destination")
		}
		digest, e := instanceKeyHash(key)
		if e != nil {
			return "", false, e
		}
		var workerCount int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM runner_credentials WHERE token_hash=?`, digest).Scan(&workerCount); e != nil {
			return "", false, e
		}
		if workerCount != 0 {
			return "", false, errors.New("owner key must be distinct from internal worker credentials")
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, e = tx.ExecContext(ctx, `INSERT INTO instance_auth(singleton,key_hash,created_at,rotated_at) VALUES(1,?,?,?)`, digest, now, now); e != nil {
			return "", false, e
		}
		if e = ensureInstanceActor(ctx, tx, now); e != nil {
			return "", false, e
		}
		if e = tx.Commit(); e != nil {
			return "", false, e
		}
		return key, generated, nil
	}
	if err != nil {
		return "", false, err
	}
	if configured != "" {
		digest, e := instanceKeyHash(configured)
		if e != nil {
			return "", false, e
		}
		if len(hash) != len(digest) || subtle.ConstantTimeCompare(hash, digest) != 1 {
			return "", false, errors.New("configured instance key does not match the stored instance credential")
		}
	}
	if err = ensureInstanceActor(ctx, tx, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", false, err
	}
	return "", false, tx.Commit()
}

func ensureInstanceActor(ctx context.Context, db *sql.Tx, now string) error {
	// Existing domain tables retain a synthetic owner row for foreign-key and
	// audit compatibility; it is never used for authentication or approval.
	_, err := db.ExecContext(ctx, `INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at,approved_at) VALUES('instance','default','owner','This instance','approved',?,?) ON CONFLICT(id) DO NOTHING`, now, now)
	return err
}

func (s *Server) instanceAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, err := instanceKeyHash(bearer(r))
		if err != nil {
			writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "valid instance bearer required", nil))
			return
		}
		var expected []byte
		var generation int64
		if err = s.store.DB().QueryRowContext(r.Context(), `SELECT key_hash,generation FROM instance_auth WHERE singleton=1`).Scan(&expected, &generation); err != nil || len(expected) != len(provided) || subtle.ConstantTimeCompare(expected, provided) != 1 {
			writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "instance credential is not recognized", nil))
			return
		}
		p := contracts.Principal{Generation: generation, InstallationID: r.Header.Get("X-Zoe-Installation-ID"), WorkspaceID: "default", SubjectID: "owner", DeviceID: "instance", Scopes: append([]string(nil), s.cfg.InstanceScopes...)}
		if required := requiredCoreDeviceScope(r); required != "" && !HasScope(p, required) {
			writeError(w, apiError(403, "forbidden", "instance lacks required scope", nil))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), devicePrincipalKey{}, p)))
	})
}

// Recheck within each mutation transaction. Instance credentials are rotated
// atomically by replacing the digest, so no device/session revocation lookup is
// needed here.
func requireOwnerSessionTx(ctx context.Context, tx *sql.Tx, p contracts.Principal) error {
	if p.WorkspaceID != "default" || p.SubjectID != "owner" || p.DeviceID != "instance" {
		return apiError(401, "unauthenticated", "instance session is invalid", nil)
	}
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM instance_auth WHERE singleton=1 AND generation=?`, p.Generation).Scan(&present); err != nil {
		return err
	}
	if present != 1 {
		return apiError(401, "unauthenticated", "instance credential is not initialized", nil)
	}
	return nil
}

func requiredCoreDeviceScope(r *http.Request) string {
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/tasks/") {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] == "entries" {
			return "client:read"
		}
	}
	switch r.URL.Path {
	case "/v1/snapshot", "/v1/screen", "/v1/events", "/v1/notifications":
		return "client:read"
	case "/v1/commands":
		if r.Method == http.MethodGet {
			return "client:read"
		}
		if r.Method == http.MethodPost {
			return "client:command"
		}
	}
	return ""
}

func (s *Server) runnerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash, err := tokenHash(bearer(r))
		if err != nil {
			writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "valid runner bearer required", nil))
			return
		}
		var p contracts.RunnerPrincipal
		var workspaceJSON, scopesJSON []byte
		var revoked sql.NullString
		err = s.store.DB().QueryRowContext(r.Context(), `SELECT runner_id,workspace_ids,scopes,revoked_at FROM runner_credentials WHERE token_hash=?`, hash).Scan(&p.RunnerID, &workspaceJSON, &scopesJSON, &revoked)
		if err == nil {
			if revoked.Valid {
				writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "runner credential is revoked", nil))
				return
			}
			if e := jsonUnmarshal(workspaceJSON, &p.WorkspaceIDs); e != nil {
				writeError(w, apiError(500, "internal", "runner credential record is invalid", nil))
				return
			}
			if e := jsonUnmarshal(scopesJSON, &p.Scopes); e != nil {
				writeError(w, apiError(500, "internal", "runner credential record is invalid", nil))
				return
			}
		} else {
			writeError(w, apiError(http.StatusUnauthorized, "unauthenticated", "runner credential is not recognized", nil))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), runnerPrincipalKey{}, p)))
	})
}

// RegisterRunnerCredential stores only a digest of a deployment-provisioned bearer.
func (s *Server) RegisterRunnerCredential(ctx context.Context, token, runnerID string, workspaceIDs, scopes []string) error {
	return RegisterRunnerCredentialIfMissing(ctx, s.store, token, runnerID, workspaceIDs, scopes)
}

// RegisterRunnerCredentialIfMissing provisions the deployment credential once
// and preserves a later database revocation across server restarts.
func RegisterRunnerCredentialIfMissing(ctx context.Context, store interface{ DB() *sql.DB }, token, runnerID string, workspaceIDs, scopes []string) error {
	if store == nil || store.DB() == nil {
		return errors.New("open control store is required")
	}
	hash, err := tokenHash(token)
	if err != nil {
		return err
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ownerCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM instance_auth WHERE key_hash=?`, hash).Scan(&ownerCount); err != nil {
		return err
	}
	if ownerCount != 0 {
		return errors.New("internal worker credential must be distinct from the owner key")
	}
	workspaces, _ := jsonMarshal(workspaceIDs)
	scopeBytes, _ := jsonMarshal(scopes)
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO runner_credentials(token_hash,runner_id,workspace_ids,scopes) VALUES(?,?,?,?)`, hash, runnerID, workspaces, scopeBytes); err != nil {
		return err
	}
	return tx.Commit()

}

// RotateInstanceKey atomically replaces the owner key and increments its fence.
// Runners retain independent credentials and continue observing existing tasks.
func RotateInstanceKey(ctx context.Context, store *storage.Store, key string) error {
	digest, err := instanceKeyHash(key)
	if err != nil {
		return err
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var workerCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM runner_credentials WHERE token_hash=?`, digest).Scan(&workerCount); err != nil {
		return err
	}
	if workerCount != 0 {
		return errors.New("owner key must be distinct from internal worker credentials")
	}
	result, err := tx.ExecContext(ctx, `UPDATE instance_auth SET key_hash=?,generation=generation+1,rotated_at=? WHERE singleton=1`, digest, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("instance credential is not initialized")
	}
	return tx.Commit()
}
