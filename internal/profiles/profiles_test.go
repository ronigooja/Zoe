package profiles

import (
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func evaluate(rules []contracts.PermissionRule, action, resource string) string {
	effect := "ask"
	for _, r := range rules {
		if (r.Action == "*" || r.Action == action) && (r.Resource == "*" || r.Resource == resource) {
			effect = r.Effect
		}
	}
	return effect
}

func TestEffectivePermissionSetTiersDenyFirstAndLastMatch(t *testing.T) {
	for _, tier := range []string{TierReadOnlyResearch, TierWorkspaceReadWrite, TierExternalPaths} {
		rules, impact, err := EffectivePermissionSet(Profile{PermissionTier: tier}, nil, nil)
		if err != nil || rules[0].Effect != "deny" || evaluate(rules, "read", "x") != "allow" {
			t.Fatalf("tier %s rules=%v impact=%s err=%v", tier, rules, impact, err)
		}
		if tier == TierReadOnlyResearch && evaluate(rules, "edit", "x") == "allow" {
			t.Fatal("read-only tier allowed edit")
		}
		if tier != TierExternalPaths && evaluate(rules, "external_directory", "/tmp/x") == "allow" {
			t.Fatal("external directory allowed without grant")
		}
		if (tier == TierExternalPaths) != (impact == "high") {
			t.Fatalf("impact=%q tier=%q", impact, tier)
		}
	}
}

func TestValidateRulesetRejectsMissingAndWidenedClaims(t *testing.T) {
	if err := ValidateRuleset(nil, TierWorkspaceReadWrite); err == nil {
		t.Fatal("missing ruleset accepted")
	}
	rules, _, _ := EffectivePermissionSet(Profile{PermissionTier: TierReadOnlyResearch}, nil, nil)
	rules = append(rules, contracts.PermissionRule{Action: "external_directory", Resource: "*", Effect: "allow"})
	if err := ValidateRuleset(rules, TierReadOnlyResearch); err == nil {
		t.Fatal("widened ruleset accepted")
	}
}
