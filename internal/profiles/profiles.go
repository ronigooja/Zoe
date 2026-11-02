package profiles

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ronigooja/Zoe/contracts"
)

const (
	TierReadOnlyResearch   = "read_only_research"
	TierWorkspaceReadWrite = "workspace_read_write"
	TierExternalPaths      = "external_paths"
)

type Profile struct {
	ID, SystemPrompt, ModelRouteKey string
	CapabilityIDs                   []string
	ScopeDefaults                   []string
	PermissionTier                  string
}

var Definitions = map[string]Profile{
	"zoe-worker":  {ID: "zoe-worker", SystemPrompt: "Act as Zoe's Worker. Complete the assigned workspace task carefully and report evidence.", ModelRouteKey: "default", CapabilityIDs: []string{"workspace.read", "workspace.write"}, ScopeDefaults: []string{"workspace"}, PermissionTier: TierWorkspaceReadWrite},
	"zoe-steward": {ID: "zoe-steward", SystemPrompt: "Act as Zoe's Steward. Understand the handoff, advise, and route work without changing files.", ModelRouteKey: "default", CapabilityIDs: []string{"research.read"}, ScopeDefaults: []string{"workspace"}, PermissionTier: TierReadOnlyResearch},
	"zoe-super":   {ID: "zoe-super", SystemPrompt: "Act as Zoe's Super planner. Decompose complex goals and coordinate bounded work.", ModelRouteKey: "default", CapabilityIDs: []string{"workspace.read", "workspace.write", "external.read"}, ScopeDefaults: []string{"workspace"}, PermissionTier: TierExternalPaths},
}

func ByID(id string) (Profile, bool) { p, ok := Definitions[id]; return p, ok }

func EffectivePermissionSet(p Profile, taskScopes, extras []string) ([]contracts.PermissionRule, string, error) {
	tier := p.PermissionTier
	if tier == "" {
		tier = TierWorkspaceReadWrite
	}
	if tier != TierReadOnlyResearch && tier != TierWorkspaceReadWrite && tier != TierExternalPaths {
		return nil, "", fmt.Errorf("unknown permission tier %q", tier)
	}
	for _, s := range append(append([]string{}, taskScopes...), extras...) {
		if s == TierExternalPaths {
			tier = TierExternalPaths
		}
	}
	rules := []contracts.PermissionRule{{Action: "*", Resource: "*", Effect: "deny"}}
	add := func(actions ...string) {
		for _, action := range actions {
			rules = append(rules, contracts.PermissionRule{Action: action, Resource: "*", Effect: "allow"})
		}
	}
	add("read", "grep", "glob", "webfetch")
	if tier != TierReadOnlyResearch {
		add("edit", "write", "shell")
	}
	if tier == TierExternalPaths {
		rules = append(rules, contracts.PermissionRule{Action: "external_directory", Resource: "*", Effect: "allow"})
	}
	impact := "low"
	if tier == TierExternalPaths {
		impact = "high"
	}
	return rules, impact, nil
}

func ValidateRuleset(rules []contracts.PermissionRule, tier string) error {
	if len(rules) == 0 || len(rules) > 64 {
		return errors.New("permission ruleset is missing or too large")
	}
	if rules[0] != (contracts.PermissionRule{Action: "*", Resource: "*", Effect: "deny"}) {
		return errors.New("permission ruleset must start with deny-all")
	}
	if tier != TierReadOnlyResearch && tier != TierWorkspaceReadWrite && tier != TierExternalPaths {
		return errors.New("invalid permission tier")
	}
	for _, r := range rules {
		if r.Action == "" || r.Resource == "" || (r.Effect != "allow" && r.Effect != "deny" && r.Effect != "ask") {
			return errors.New("invalid permission rule")
		}
		if r.Effect == "allow" && r.Action == "external_directory" && tier != TierExternalPaths {
			return errors.New("external directory permission exceeds tier")
		}
		if r.Effect == "allow" && (r.Action == "edit" || r.Action == "write" || r.Action == "shell") && tier == TierReadOnlyResearch {
			return errors.New("write permission exceeds read-only tier")
		}
	}
	return nil
}

func NormalizeID(id string) string { return strings.TrimSpace(id) }
