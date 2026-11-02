package runner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ronigooja/Zoe/contracts"
)

type Journal struct {
	db   *sql.DB
	lock *os.File
}

// DeploymentHostLock is a host-wide singleton lock for all runner processes
// with Docker access. Every runner on the same Docker host must use the same
// lock path, independent of its journal path.
type DeploymentHostLock struct{ file *os.File }

func OpenDeploymentHostLock(filename string) (*DeploymentHostLock, error) {
	if strings.TrimSpace(filename) == "" {
		return nil, errors.New("deployment host lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return nil, fmt.Errorf("create deployment lock directory: %w", err)
	}
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("resolve deployment lock path: %w", err)
	}
	if info, err := os.Lstat(absolute); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("deployment host lock path must not be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect deployment host lock: %w", err)
	}
	f, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open deployment host lock: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("protect deployment host lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another runner owns the Docker deployment host lock: %w", err)
	}
	return &DeploymentHostLock{file: f}, nil
}

func (l *DeploymentHostLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

type JournalEntry struct {
	Claim              contracts.JobClaim
	Spec               RuntimeSpec
	Session            string
	Container          string
	Password           string
	InstanceID         string
	Epoch              int64
	Phase              string
	Updated            time.Time
	RotationSummary    string
	RotationCount      int
	RotationOldSession string
	RotationNewSession string
}

type QueuedObservation struct {
	RunID string
	Body  contracts.RunnerObservation
}

type DeploymentJournalEntry struct {
	Claim        contracts.DeploymentClaim
	Observation  *contracts.DeploymentObservation
	Acknowledged bool
}

func OpenJournal(ctx context.Context, filename string) (*Journal, error) {
	if filename == "" || filename == ":memory:" {
		return nil, errors.New("runner journal requires a persistent file path")
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return nil, fmt.Errorf("create runner journal directory: %w", err)
	}
	// Resolve the containing directory so relative paths and symlinked parent
	// directories converge on the same database file. The lock is taken on the
	// database inode itself, which also covers hard-link aliases.
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("resolve runner journal path: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("resolve runner journal directory: %w", err)
	}
	filename = filepath.Join(parent, filepath.Base(absolute))
	if _, err := os.Lstat(filename); err == nil {
		filename, err = filepath.EvalSymlinks(filename)
		if err != nil {
			return nil, fmt.Errorf("resolve runner journal file: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect runner journal file: %w", err)
	}
	lock, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runner journal lock file: %w", err)
	}
	if err := lock.Chmod(0o600); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("protect runner journal lock file: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("runner journal is already in use: %w", err)
	}
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, fmt.Errorf("open runner journal: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			return nil, fmt.Errorf("configure runner journal: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS journal_runs (
  run_id TEXT PRIMARY KEY,
  attempt_id TEXT NOT NULL,
  fence INTEGER NOT NULL,
  claim_json BLOB NOT NULL,
		  runtime_spec_json BLOB NOT NULL DEFAULT '{}',
		  runtime_session_id TEXT NOT NULL DEFAULT '',
		runtime_container_id TEXT NOT NULL DEFAULT '',
		runtime_password TEXT NOT NULL DEFAULT '',
  instance_id TEXT NOT NULL DEFAULT '',
  instance_epoch INTEGER NOT NULL DEFAULT 0,
  phase TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS journal_observations (
  observation_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  body BLOB NOT NULL,
  created_at TEXT NOT NULL,
  acknowledged_at TEXT
);
CREATE TABLE IF NOT EXISTS journal_reconciliations (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  actor TEXT NOT NULL,
  checked_at TEXT NOT NULL,
  result_json BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS journal_deployments (
  deployment_id TEXT PRIMARY KEY,
  attempt_id TEXT NOT NULL,
  fence INTEGER NOT NULL,
  claim_json BLOB NOT NULL,
  observation_json BLOB,
  acknowledged INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);
`); err != nil {
		_ = db.Close()
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, fmt.Errorf("create runner journal schema: %w", err)
	}
	for _, column := range []struct{ name, definition string }{
		{"runtime_spec_json", "BLOB NOT NULL DEFAULT '{}'"},
		{"runtime_session_id", "TEXT NOT NULL DEFAULT ''"},
		{"runtime_container_id", "TEXT NOT NULL DEFAULT ''"},
		{"runtime_password", "TEXT NOT NULL DEFAULT ''"},
		{"instance_id", "TEXT NOT NULL DEFAULT ''"},
		{"instance_epoch", "INTEGER NOT NULL DEFAULT 0"},
		{"rotation_summary", "TEXT NOT NULL DEFAULT ''"},
		{"rotation_count", "INTEGER NOT NULL DEFAULT 0"},
		{"rotation_old_session", "TEXT NOT NULL DEFAULT ''"},
		{"rotation_new_session", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureJournalColumn(ctx, db, column.name, column.definition); err != nil {
			_ = db.Close()
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			return nil, err
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE journal_runs SET runtime_spec_json='{}' WHERE runtime_spec_json IS NULL`); err != nil {
		_ = db.Close()
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, fmt.Errorf("backfill neutral runner journal columns: %w", err)
	}
	if err := protectSQLiteFiles(filename); err != nil {
		_ = db.Close()
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, err
	}
	return &Journal{db: db, lock: lock}, nil
}

func ensureJournalColumn(ctx context.Context, db *sql.DB, name, definition string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(journal_runs)`)
	if err != nil {
		return fmt.Errorf("inspect runner journal schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var column, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &column, &kind, &notnull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read runner journal schema: %w", err)
		}
		found = found || column == name
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read runner journal schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close runner journal schema: %w", err)
	}
	if found {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE journal_runs ADD COLUMN `+name+` `+definition); err != nil {
		return fmt.Errorf("upgrade runner journal schema: %w", err)
	}
	return nil
}

func (j *Journal) SaveDeploymentClaim(ctx context.Context, claim contracts.DeploymentClaim) error {
	claim.ClaimState = ""
	claim.DesiredControl = ""
	body, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var priorFence int64
	var prior []byte
	err = tx.QueryRowContext(ctx, `SELECT fence,claim_json FROM journal_deployments WHERE deployment_id=?`, claim.DeploymentID).Scan(&priorFence, &prior)
	if err == nil {
		if claim.Fence < priorFence {
			return errors.New("deployment journal claim fence is stale")
		}
		if claim.Fence == priorFence {
			if string(prior) != string(body) {
				return errors.New("deployment journal claim changed under the same fence")
			}
			return tx.Commit()
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_deployments(deployment_id,attempt_id,fence,claim_json,observation_json,acknowledged,updated_at) VALUES(?,?,?,?,NULL,0,?) ON CONFLICT(deployment_id) DO UPDATE SET attempt_id=excluded.attempt_id,fence=excluded.fence,claim_json=excluded.claim_json,observation_json=NULL,acknowledged=0,updated_at=excluded.updated_at`, claim.DeploymentID, claim.AttemptID, claim.Fence, body, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (j *Journal) DeploymentEntry(ctx context.Context, deploymentID string) (DeploymentJournalEntry, bool, error) {
	var entry DeploymentJournalEntry
	var claimBody, obsBody []byte
	var acknowledged int
	err := j.db.QueryRowContext(ctx, `SELECT claim_json,observation_json,acknowledged FROM journal_deployments WHERE deployment_id=?`, deploymentID).Scan(&claimBody, &obsBody, &acknowledged)
	if errors.Is(err, sql.ErrNoRows) {
		return DeploymentJournalEntry{}, false, nil
	}
	if err != nil {
		return DeploymentJournalEntry{}, false, err
	}
	if err := json.Unmarshal(claimBody, &entry.Claim); err != nil {
		return DeploymentJournalEntry{}, false, err
	}
	if len(obsBody) > 0 {
		var obs contracts.DeploymentObservation
		if err := json.Unmarshal(obsBody, &obs); err != nil {
			return DeploymentJournalEntry{}, false, err
		}
		entry.Observation = &obs
	}
	entry.Acknowledged = acknowledged != 0
	return entry, true, nil
}

func (j *Journal) SaveDeploymentObservation(ctx context.Context, observation contracts.DeploymentObservation) error {
	body, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	result, err := j.db.ExecContext(ctx, `UPDATE journal_deployments SET observation_json=?,acknowledged=0,updated_at=? WHERE deployment_id=? AND attempt_id=? AND fence=?`, body, time.Now().UTC().Format(time.RFC3339Nano), observation.DeploymentID, observation.AttemptID, observation.Fence)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("deployment observation has no matching durable claim")
	}
	return nil
}

func (j *Journal) AcknowledgeDeploymentObservation(ctx context.Context, deploymentID, observationID string) error {
	var body []byte
	if err := j.db.QueryRowContext(ctx, `SELECT observation_json FROM journal_deployments WHERE deployment_id=?`, deploymentID).Scan(&body); err != nil {
		return err
	}
	var observation contracts.DeploymentObservation
	if err := json.Unmarshal(body, &observation); err != nil {
		return err
	}
	if observation.ObservationID != observationID {
		return errors.New("deployment observation acknowledgement id mismatch")
	}
	result, err := j.db.ExecContext(ctx, `UPDATE journal_deployments SET acknowledged=1,updated_at=? WHERE deployment_id=? AND observation_json IS NOT NULL`, time.Now().UTC().Format(time.RFC3339Nano), deploymentID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("deployment observation acknowledgement has no durable observation")
	}
	return nil
}

func (j *Journal) Close() error {
	var err error
	if j.db != nil {
		err = j.db.Close()
		j.db = nil
	}
	if j.lock != nil {
		_ = syscall.Flock(int(j.lock.Fd()), syscall.LOCK_UN)
		if closeErr := j.lock.Close(); err == nil {
			err = closeErr
		}
		j.lock = nil
	}
	return err
}

// SaveClaim commits the exact runtime request before any runtime effect can occur.
func (j *Journal) SaveClaim(ctx context.Context, claim contracts.JobClaim, spec RuntimeSpec, phase string) (JournalEntry, error) {
	claimJSON, err := json.Marshal(claim)
	if err != nil {
		return JournalEntry{}, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return JournalEntry{}, err
	}
	now := time.Now().UTC()
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return JournalEntry{}, err
	}
	defer tx.Rollback()
	var existingAttempt string
	var existingFence int64
	var existingClaim, existingSpec []byte
	var session, persistedPhase, updated string
	var container, runtimeSession, runtimeContainer, instanceID string
	var epoch int64
	err = tx.QueryRowContext(ctx, `SELECT attempt_id,fence,claim_json,runtime_spec_json,runtime_session_id,runtime_container_id,instance_id,instance_epoch,phase,updated_at FROM journal_runs WHERE run_id=?`, claim.RunID).Scan(&existingAttempt, &existingFence, &existingClaim, &existingSpec, &session, &container, &instanceID, &epoch, &persistedPhase, &updated)
	if err == nil {
		claimMatches := bytes.Equal(existingClaim, claimJSON) || compatibleJournalClaimUpdate(existingClaim, existingSpec, claim, spec, session, claimJSON)
		if existingAttempt != claim.AttemptID || uint64(existingFence) != claim.Fence || !claimMatches || !bytes.Equal(existingSpec, specJSON) {
			return JournalEntry{}, errors.New("runner journal run identity or request changed under the same run id")
		}
		phase = persistedPhase
	} else if !errors.Is(err, sql.ErrNoRows) {
		return JournalEntry{}, err
	} else {
		if phase == "" {
			phase = "claimed"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO journal_runs(run_id,attempt_id,fence,claim_json,runtime_spec_json,phase,updated_at) VALUES(?,?,?,?,?,?,?)`, claim.RunID, claim.AttemptID, claim.Fence, claimJSON, specJSON, phase, now.Format(time.RFC3339Nano)); err != nil {
			return JournalEntry{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return JournalEntry{}, err
	}
	if session == "" {
		session = runtimeSession
	}
	if container == "" {
		container = runtimeContainer
	}
	return JournalEntry{Claim: claim, Spec: spec, Session: session, Container: container, InstanceID: instanceID, Epoch: epoch, Phase: phase, Updated: now}, nil
}

// compatibleJournalClaimUpdate allows only two narrowly bounded updates to a
// journal claim: filling an initially empty session ID with the
// identical session ID recorded in this journal row.
func compatibleJournalClaimUpdate(existingClaim, existingSpec []byte, claim contracts.JobClaim, spec RuntimeSpec, journalSession string, currentClaimJSON []byte) bool {
	var oldClaim contracts.JobClaim
	var oldSpec RuntimeSpec
	if json.Unmarshal(existingClaim, &oldClaim) != nil || json.Unmarshal(existingSpec, &oldSpec) != nil || oldClaim.Harness != "" && oldClaim.Harness != claim.Harness {
		return false
	}
	if oldClaim.RuntimeSessionID != claim.RuntimeSessionID {
		if oldClaim.RuntimeSessionID != "" || claim.RuntimeSessionID == "" || claim.RuntimeSessionID != journalSession {
			return false
		}
		oldClaim.RuntimeSessionID = claim.RuntimeSessionID
	}
	upgradedClaim, err := json.Marshal(oldClaim)
	return err == nil && bytes.Equal(upgradedClaim, currentClaimJSON)
}

func (j *Journal) Entry(ctx context.Context, runID string) (JournalEntry, bool, error) {
	var claimJSON, specJSON []byte
	var entry JournalEntry
	var updated string
	err := j.db.QueryRowContext(ctx, `SELECT claim_json,runtime_spec_json,runtime_session_id,runtime_container_id,runtime_password,instance_id,instance_epoch,phase,updated_at,rotation_summary,rotation_count,rotation_old_session,rotation_new_session FROM journal_runs WHERE run_id=?`, runID).Scan(&claimJSON, &specJSON, &entry.Session, &entry.Container, &entry.Password, &entry.InstanceID, &entry.Epoch, &entry.Phase, &updated, &entry.RotationSummary, &entry.RotationCount, &entry.RotationOldSession, &entry.RotationNewSession)
	if errors.Is(err, sql.ErrNoRows) {
		return JournalEntry{}, false, nil
	}
	if err != nil {
		return JournalEntry{}, false, err
	}
	if err := json.Unmarshal(claimJSON, &entry.Claim); err != nil {
		return JournalEntry{}, false, err
	}
	if err := json.Unmarshal(specJSON, &entry.Spec); err != nil {
		return JournalEntry{}, false, err
	}
	entry.Updated, _ = time.Parse(time.RFC3339Nano, updated)
	return entry, true, nil
}

// BeginRotation durably records the bounded summary before creating its replacement.
func (j *Journal) BeginRotation(ctx context.Context, runID, attemptID string, fence uint64, oldSession, summary string, count int) error {
	result, err := j.db.ExecContext(ctx, `UPDATE journal_runs SET phase='rotating',rotation_summary=?,rotation_count=?,rotation_old_session=?,rotation_new_session='',updated_at=? WHERE run_id=? AND attempt_id=? AND fence=?`, summary, count, oldSession, time.Now().UTC().Format(time.RFC3339Nano), runID, attemptID, fence)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("runner journal rotation fence mismatch")
	}
	return nil
}

func (j *Journal) SetRotationSession(ctx context.Context, runID, attemptID string, fence uint64, session, phase string) error {
	result, err := j.db.ExecContext(ctx, `UPDATE journal_runs SET phase=?,rotation_new_session=?,runtime_session_id=?,updated_at=? WHERE run_id=? AND attempt_id=? AND fence=?`, phase, session, session, time.Now().UTC().Format(time.RFC3339Nano), runID, attemptID, fence)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("runner journal rotation session fence mismatch")
	}
	return nil
}

func (j *Journal) SetContainer(ctx context.Context, runID, attemptID string, fence uint64, container string) error {
	if strings.TrimSpace(container) == "" {
		return errors.New("runtime container identity is required")
	}
	result, err := j.db.ExecContext(ctx, `UPDATE journal_runs SET runtime_container_id=?,updated_at=? WHERE run_id=? AND attempt_id=? AND fence=? AND (runtime_container_id='' OR runtime_container_id=?)`, container, time.Now().UTC().Format(time.RFC3339Nano), runID, attemptID, fence, container)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("runner journal container identity or fence mismatch")
	}
	return nil
}

func (j *Journal) SetInstance(ctx context.Context, runID, attemptID string, fence uint64, instanceID string, epoch int64) error {
	if strings.TrimSpace(instanceID) == "" || epoch < 1 {
		return errors.New("active instance identity and epoch are required")
	}
	result, err := j.db.ExecContext(ctx, `UPDATE journal_runs SET instance_id=?,instance_epoch=?,updated_at=? WHERE run_id=? AND attempt_id=? AND fence=? AND (instance_id='' OR (instance_id=? AND instance_epoch=?))`, instanceID, epoch, time.Now().UTC().Format(time.RFC3339Nano), runID, attemptID, fence, instanceID, epoch)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("runner journal active instance fence changed")
	}
	return nil
}

func (j *Journal) SetSession(ctx context.Context, runID, attemptID string, fence uint64, session, phase string) error {
	result, err := j.db.ExecContext(ctx, `UPDATE journal_runs SET runtime_session_id=?,phase=?,updated_at=? WHERE run_id=? AND attempt_id=? AND fence=?`, session, phase, time.Now().UTC().Format(time.RFC3339Nano), runID, attemptID, fence)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("runner journal fence mismatch")
	}
	return nil
}

func (j *Journal) SetRuntimePassword(ctx context.Context, runID, attemptID string, fence uint64, password string) error {
	if strings.TrimSpace(password) == "" {
		return errors.New("runtime password is required")
	}
	result, err := j.db.ExecContext(ctx, `UPDATE journal_runs SET runtime_password=?,updated_at=? WHERE run_id=? AND attempt_id=? AND fence=? AND (runtime_password='' OR runtime_password=?)`, password, time.Now().UTC().Format(time.RFC3339Nano), runID, attemptID, fence, password)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("runtime password identity or fence mismatch")
	}
	return nil
}

func (j *Journal) SetPhase(ctx context.Context, runID, attemptID string, fence uint64, phase string) error {
	result, err := j.db.ExecContext(ctx, `UPDATE journal_runs SET phase=?,updated_at=? WHERE run_id=? AND attempt_id=? AND fence=?`, phase, time.Now().UTC().Format(time.RFC3339Nano), runID, attemptID, fence)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("runner journal fence mismatch")
	}
	return nil
}

func (j *Journal) QueueObservation(ctx context.Context, runID string, body contracts.RunnerObservation) (contracts.RunnerObservation, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return contracts.RunnerObservation{}, err
	}
	_, err = j.db.ExecContext(ctx, `INSERT OR IGNORE INTO journal_observations(observation_id,run_id,body,created_at) VALUES(?,?,?,?)`, body.ObservationID, runID, encoded, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return contracts.RunnerObservation{}, err
	}
	var prior []byte
	if err := j.db.QueryRowContext(ctx, `SELECT body FROM journal_observations WHERE observation_id=?`, body.ObservationID).Scan(&prior); err != nil {
		return contracts.RunnerObservation{}, err
	}
	var stored contracts.RunnerObservation
	if err := json.Unmarshal(prior, &stored); err != nil {
		return contracts.RunnerObservation{}, err
	}
	if stored.ObservationID != body.ObservationID || stored.RunID != body.RunID || stored.AttemptID != body.AttemptID || stored.Fence != body.Fence || stored.Kind != body.Kind {
		return contracts.RunnerObservation{}, errors.New("runner observation id reused with different identity")
	}
	return stored, nil
}

func (j *Journal) ObservationExists(ctx context.Context, id string) (bool, bool, error) {
	var acknowledged sql.NullString
	err := j.db.QueryRowContext(ctx, `SELECT acknowledged_at FROM journal_observations WHERE observation_id=?`, id).Scan(&acknowledged)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, acknowledged.Valid, nil
}

func (j *Journal) PendingObservations(ctx context.Context, runID string) ([]QueuedObservation, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT run_id,body FROM journal_observations WHERE run_id=? AND acknowledged_at IS NULL ORDER BY created_at,observation_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedObservation
	for rows.Next() {
		var queued QueuedObservation
		var encoded []byte
		if err := rows.Scan(&queued.RunID, &encoded); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &queued.Body); err != nil {
			return nil, err
		}
		out = append(out, queued)
	}
	return out, rows.Err()
}

func (j *Journal) AcknowledgeObservation(ctx context.Context, id string) error {
	_, err := j.db.ExecContext(ctx, `UPDATE journal_observations SET acknowledged_at=? WHERE observation_id=?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func protectSQLiteFiles(filename string) error {
	for _, candidate := range []string{filename, filename + "-wal", filename + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("protect runner journal file %q: %w", filepath.Base(candidate), err)
		}
	}
	return nil
}

// NoEffectReceipt includes acknowledged receipts so operator retries reuse the
// original proof and observation identity even after an acknowledgement crash.
func (j *Journal) NoEffectReceipt(ctx context.Context, runID string) (*contracts.RunnerObservation, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT body FROM journal_observations WHERE run_id=? AND json_extract(body,'$.kind')='no_effect_terminal' ORDER BY created_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var body []byte
	if err := rows.Scan(&body); err != nil {
		return nil, err
	}
	var obs contracts.RunnerObservation
	if err := json.Unmarshal(body, &obs); err != nil {
		return nil, err
	}
	return &obs, nil
}

func (j *Journal) SaveReconciliationAudit(ctx context.Context, id, runID, actor string, at time.Time, result any) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = j.db.ExecContext(ctx, `INSERT INTO journal_reconciliations(id,run_id,actor,checked_at,result_json) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET result_json=excluded.result_json`, id, runID, actor, at.Format(time.RFC3339Nano), body)
	return err
}
