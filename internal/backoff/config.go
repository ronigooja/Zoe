package backoff

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// Fingerprint separates credential hashing from the outer configuration digest.
func Fingerprint(endpoint, model, effort, thinking string, credential []byte) string {
	key := sha256.New()
	_, _ = key.Write([]byte("zoe-model-credential-v1\x00"))
	_, _ = key.Write(credential)
	encoded, _ := json.Marshal([]string{"zoe-model-config-v1", endpoint, model, effort, thinking, hex.EncodeToString(key.Sum(nil))})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// ObserveConfig atomically clears authentication stops only on a known change.
func (s Store) ObserveConfig(ctx context.Context, consumer, fingerprint string) (bool, error) {
	if len(fingerprint) != 64 {
		return false, errors.New("invalid configuration fingerprint")
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return false, errors.New("invalid configuration fingerprint")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM model_config_fingerprints WHERE consumer=?`, consumer).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	changed := err == nil && old != fingerprint
	if changed {
		if _, err = tx.ExecContext(ctx, `DELETE FROM model_retry_state WHERE consumer=? AND auth_failed=1`, consumer); err != nil {
			return false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO model_config_fingerprints(consumer,fingerprint) VALUES(?,?) ON CONFLICT(consumer) DO UPDATE SET fingerprint=excluded.fingerprint`, consumer, fingerprint); err != nil {
		return false, err
	}
	return changed, tx.Commit()
}
