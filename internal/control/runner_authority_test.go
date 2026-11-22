package control

import (
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func TestRunnerAuthorityUsesExplicitInstanceScopes(t *testing.T) {
	for _, test := range []struct {
		name   string
		scopes []string
		want   bool
	}{
		{name: "missing scope"},
		{name: "other scope", scopes: []string{"runner:observe"}},
		{name: "voice only", scopes: []string{"voice:turn:ingest"}},
		{name: "claim", scopes: []string{"runner:claim"}, want: true},
		{name: "wildcard", scopes: []string{"*"}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := contracts.RunnerPrincipal{RunnerID: "runner", Scopes: test.scopes}
			if got := HasRunnerScope(p, "runner:claim"); got != test.want {
				t.Fatalf("claim authorization=%v, want %v", got, test.want)
			}
		})
	}
}
