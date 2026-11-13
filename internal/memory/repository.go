package memory

import (
	"database/sql"
	"errors"

	"github.com/ronigooja/Zoe/internal/storage"
)

// Repository reads ingestion inputs and tracks successful worker boundaries.
type Repository struct{ db *sql.DB }

var ErrVersionConflict = errors.New("ingestion boundary changed concurrently")

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

func NewRepositoryWithStore(store *storage.Store) *Repository {
	return NewRepository(store.DB())
}
