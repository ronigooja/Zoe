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

func TestOpenCodeToolActionsAreAllowedByTierAndUnknownActionsDenied(t *testing.T) {
	// Action table copied from OpenCode packages/core/src/tool/plugin/*.ts:
	// write.ts, patch.ts, edit.ts assert edit; read.ts asserts skill and read;
	// the remaining entries assert their exported tool name, except mcp-resource.ts.
	table := []struct {
		name, action, tier string
	}{
		{"read", "read", TierReadOnlyResearch}, {"grep", "grep", TierReadOnlyResearch}, {"glob", "glob", TierReadOnlyResearch},
		{"webfetch", "webfetch", TierReadOnlyResearch}, {"skill", "skill", TierReadOnlyResearch},
		{"mcp-resource-list", "opencode_list_mcp_resources", TierReadOnlyResearch}, {"mcp-resource-read", "opencode_read_mcp_resource", TierReadOnlyResearch},
		{"write", "edit", TierWorkspaceReadWrite}, {"patch", "edit", TierWorkspaceReadWrite}, {"edit", "edit", TierWorkspaceReadWrite},
		{"shell", "shell", TierWorkspaceReadWrite}, {"subagent", "subagent", TierWorkspaceReadWrite}, {"question", "question", TierWorkspaceReadWrite},
		{"todowrite", "todowrite", TierWorkspaceReadWrite},
	}
	for _, item := range table {
		rules, _, err := EffectivePermissionSet(Profile{PermissionTier: item.tier}, nil, nil)
		if err != nil || evaluate(rules, item.action, "*") != "allow" {
			t.Fatalf("tool %s action %s tier %s rules=%v err=%v", item.name, item.action, item.tier, rules, err)
		}
		if evaluate(rules, "unlisted_action", "*") != "deny" {
			t.Fatalf("unlisted action was not denied for %s", item.name)
		}
	}
}

func TestScraplingOnlyMCPToolsAreAllowed(t *testing.T) {
	readOnly, _, err := EffectivePermissionSet(Profile{PermissionTier: TierReadOnlyResearch}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	write, _, err := EffectivePermissionSet(Profile{PermissionTier: TierWorkspaceReadWrite}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"scrapling_make_request", "scrapling_browser_fetch_once", "scrapling_browser_open", "scrapling_open_request_session", "scrapling_close_session", "scrapling_list_sessions", "scrapling_browser_fetch", "scrapling_session_make_request", "scrapling_browser_screenshot", "scrapling_browser_extract", "scrapling_browser_network_requests", "scrapling_browser_network_request"} {
		if evaluate(readOnly, action, "*") != "allow" || evaluate(write, action, "*") != "allow" {
			t.Fatalf("fetch action %s not allowed", action)
		}
	}
	for _, action := range []string{"scrapling_browser_actions", "scrapling_browser_evaluate", "scrapling_browser_save_state", "scrapling_browser_load_state"} {
		if evaluate(readOnly, action, "*") != "deny" || evaluate(write, action, "*") != "allow" {
			t.Fatalf("action scope %s read=%s write=%s", action, evaluate(readOnly, action, "*"), evaluate(write, action, "*"))
		}
	}
	if evaluate(readOnly, "other_mcp_tool", "*") != "deny" || evaluate(readOnly, "websearch", "*") != "deny" {
		t.Fatalf("rules=%v", readOnly)
	}
}

func TestExternalDirectoriesAreExplicitAndValidated(t *testing.T) {
	rules, _, err := EffectivePermissionSet(Profile{PermissionTier: TierExternalPaths}, []string{"external_paths"}, []string{"external_path:/tmp/project", "external_path:/srv/data"})
	if err != nil || evaluate(rules, "external_directory", "/tmp/project/*") != "allow" || evaluate(rules, "external_directory", "*") == "allow" {
		t.Fatalf("rules=%v err=%v", rules, err)
	}
	rules, _, err = EffectivePermissionSet(Profile{PermissionTier: TierExternalPaths}, nil, nil)
	if err != nil || evaluate(rules, "external_directory", "/tmp/project/*") != "deny" {
		t.Fatalf("ungranted external path rules=%v err=%v", rules, err)
	}
	for _, path := range []string{"relative", "/", "/etc", "/root", "/var/lib/zoe", "/opt/zoe", "/tmp/../etc"} {
		if _, _, err := EffectivePermissionSet(Profile{PermissionTier: TierExternalPaths}, nil, []string{"external_path:" + path}); err == nil {
			t.Fatalf("broad or invalid path %q accepted", path)
		}
	}
}
