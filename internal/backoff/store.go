package backoff

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type State struct {
	Consumer, Scope string
	Failures        int
	NextAttemptAt   time.Time
	AuthFailed      bool
	LastError       Kind
	UpdatedAt       time.Time
}

type Store struct{ DB *sql.DB }

// Record atomically increments the failure count. Authentication stops are sticky
// until ClearAuth, even if an already-in-flight request subsequently fails.
func (s Store) Record(ctx context.Context, consumer, scope string, kind Kind, now time.Time, p Parameters, jitter func() float64) error {
	if kind != Auth && kind != Transient && kind != Permanent {
		return errors.New("invalid retry category")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	var failures int
	var stopped bool
	err = tx.QueryRowContext(ctx, `INSERT INTO model_retry_state(consumer,scope,failures,next_attempt_at,auth_failed,last_error,updated_at)
		VALUES(?,?,1,'',?,?,?) ON CONFLICT(consumer,scope) DO UPDATE SET
		failures=model_retry_state.failures+1, auth_failed=max(model_retry_state.auth_failed,excluded.auth_failed),
		last_error=CASE WHEN model_retry_state.auth_failed=1 THEN 'auth' ELSE excluded.last_error END,
		updated_at=excluded.updated_at RETURNING failures,auth_failed`, consumer, scope, kind == Auth, string(kind), stamp).Scan(&failures, &stopped)
	if err != nil {
		return err
	}
	next := ""
	if !stopped && kind == Transient {
		next = now.Add(Delay(failures, p.Base, p.Cap, jitter)).UTC().Format(time.RFC3339Nano)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE model_retry_state SET next_attempt_at=? WHERE consumer=? AND scope=?`, next, consumer, scope); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) Clear(ctx context.Context, consumer, scope string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM model_retry_state WHERE consumer=? AND scope=? AND auth_failed=0`, consumer, scope)
	return err
}

func (s Store) ClearAuth(ctx context.Context, consumer string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM model_retry_state WHERE consumer=? AND auth_failed=1`, consumer)
	return err
}

func (s Store) Due(ctx context.Context, consumer, scope string, now time.Time) (bool, error) {
	var stopped bool
	var next string
	err := s.DB.QueryRowContext(ctx, `SELECT auth_failed,next_attempt_at FROM model_retry_state WHERE consumer=? AND scope=?`, consumer, scope).Scan(&stopped, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if stopped {
		return false, nil
	}
	if next == "" {
		return true, nil
	}
	t, err := time.Parse(time.RFC3339Nano, next)
	if err != nil {
		return false, err
	}
	return !now.Before(t), nil
}

func (s Store) List(ctx context.Context) ([]State, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT consumer,scope,failures,next_attempt_at,auth_failed,last_error,updated_at FROM model_retry_state ORDER BY consumer,scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []State
	for rows.Next() {
		var item State
		var next, updated string
		if err := rows.Scan(&item.Consumer, &item.Scope, &item.Failures, &next, &item.AuthFailed, &item.LastError, &updated); err != nil {
			return nil, err
		}
		if next != "" {
			item.NextAttemptAt, err = time.Parse(time.RFC3339Nano, next)
			if err != nil {
				return nil, err
			}
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
