package voice

import (
	"context"
	"database/sql"
)

type rosterQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}
