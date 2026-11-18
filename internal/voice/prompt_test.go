package voice

import (
	"testing"

	"github.com/ronigooja/Zoe/internal/prompts"
)

func testPrompts(t *testing.T) *prompts.Set {
	t.Helper()
	p, err := prompts.Load("../../deploy/prompts")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
