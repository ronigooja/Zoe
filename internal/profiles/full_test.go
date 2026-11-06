package profiles

import (
	"testing"

	"github.com/ronigooja/Zoe/contracts"
)

func TestResidentWorkerFullPermissionsAndOtherProfilesPreserved(t *testing.T) {
	for _, id := range []string{"zoe-worker", "zoe-super"} {
		p, _ := ByID(id)
		if p.PermissionTier != TierExternalPaths {
			t.Fatalf("nonresident profile %s changed tier: %s", id, p.PermissionTier)
		}
		rules, _, err := EffectivePermissionSet(p, p.ScopeDefaults, nil)
		if err != nil || evaluate(rules, "external_directory", "*") != "deny" || evaluate(rules, "unknown_mcp_tool", "*") != "deny" {
			t.Fatalf("nonresident profile %s widened: %v %v", id, rules, err)
		}
	}
	p, _ := ByID("zoe-steward")
	rules, impact, err := EffectivePermissionSet(p, []string{"external_paths"}, nil)
	if err != nil || p.PermissionTier != "full" || impact != "high" {
		t.Fatalf("resident Worker tier=%s impact=%s err=%v", p.PermissionTier, impact, err)
	}
	for _, action := range []string{"edit", "shell", "bash", "webfetch", "external_directory", "mcp*", "scrapling_browser_evaluate", "tools.scrapling.browser_actions", "unknown_mcp_tool", "execute", "read", "subagent", "future_action"} {
		if got := evaluate(rules, action, "/etc/example.env"); got != "allow" {
			t.Errorf("resident Worker action %s = %s, want allow", action, got)
		}
	}
	if err := ValidateRuleset(rules, "full"); err != nil {
		t.Fatal(err)
	}
	rules = append(rules, contracts.PermissionRule{Action: "read", Resource: "*.env", Effect: "ask"})
	if err := ValidateRuleset(rules, "full"); err == nil {
		t.Fatal("full tier accepted a blocking prompt")
	}
}

func TestFullTierCannotBeSelectedByUnauthorizedProfile(t *testing.T) {
	for _, id := range []string{"", "zoe-worker", "zoe-super", "custom"} {
		if _, _, err := EffectivePermissionSet(Profile{ID: id, PermissionTier: "full"}, nil, nil); err == nil {
			t.Errorf("unauthorized profile %q selected full", id)
		}
	}
	if _, _, err := EffectivePermissionSet(Profile{ID: "zoe-steward", PermissionTier: "full"}, nil, nil); err != nil {
		t.Fatalf("authorized resident Worker rejected: %v", err)
	}
	for _, tier := range []string{TierReadOnlyResearch, TierWorkspaceReadWrite, TierExternalPaths} {
		rules, _, err := EffectivePermissionSet(Profile{ID: "custom", PermissionTier: tier}, []string{"full"}, nil)
		if err != nil || evaluate(rules, "unknown_mcp_tool", "*") != "deny" {
			t.Fatalf("scope escalated tier %s: %v %v", tier, rules, err)
		}
	}
}
