package memory

import (
	"database/sql"
	"errors"
	"github.com/ronigooja/Zoe/internal/limits"

	"github.com/ronigooja/Zoe/internal/storage"
)

// Repository reads ingestion inputs and tracks successful worker boundaries.
type Repository struct {
	limits limits.Set
	db     *sql.DB
	// OnRetryChange requests a retry status publication after retry state changes.
	OnRetryChange func()
}

var ErrVersionConflict = errors.New("ingestion boundary changed concurrently")

func NewRepository(db *sql.DB, sets ...limits.Set) *Repository {
	return &Repository{db: db, limits: limits.Resolve(sets)}
}

func NewRepositoryWithStore(store *storage.Store, sets ...limits.Set) *Repository {
	return NewRepository(store.DB(), sets...)
}
