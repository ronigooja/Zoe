package voice

import (
	"testing"

	"github.com/ronigooja/Zoe/internal/limits"
)

func mustLimits(t *testing.T, key string, value int64) limits.Set {
	t.Helper()
	result, err := limits.Defaults().With(key, value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
