package runner

import (
	"testing"

	"github.com/ronigooja/Zoe/internal/limits"
)

func withLimit(t *testing.T, base limits.Set, key string, value int64) limits.Set {
	t.Helper()
	updated, err := base.With(key, value)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}
