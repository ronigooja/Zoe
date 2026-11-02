package profiles

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ronigooja/Zoe/contracts"
)

const maxExternalDirectories = 8

var deniedExternalRoots = map[string]bool{
	"/": true, "/etc": true, "/root": true, "/var/lib/zoe": true, "/opt/zoe": true,
}

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
	allScopes := append(append([]string{}, taskScopes...), extras...)
	for _, s := range allScopes {
		if s == TierExternalPaths {
			tier = TierExternalPaths
		}
	}
	externalDirs, err := externalDirectories(allScopes)
	if err != nil {
		return nil, "", err
	}
	rules := []contracts.PermissionRule{{Action: "*", Resource: "*", Effect: "deny"}}
	add := func(actions ...string) {
		for _, action := range actions {
			rules = append(rules, contracts.PermissionRule{Action: action, Resource: "*", Effect: "allow"})
		}
	}
	add("read", "grep", "glob", "webfetch", "skill", "opencode_list_mcp_resources", "opencode_read_mcp_resource")
	// MCP actions are server_tool names. Read-only research gets HTTP/browser
	// fetch, extraction, screenshots, and session lifecycle only. Workspace
	// write and external tiers additionally get page actions/evaluation and
	// browser state persistence; other MCP servers remain denied.
	for _, action := range []string{"scrapling_make_request", "scrapling_browser_fetch_once", "scrapling_browser_open", "scrapling_open_request_session", "scrapling_close_session", "scrapling_list_sessions", "scrapling_browser_fetch", "scrapling_session_make_request", "scrapling_browser_screenshot", "scrapling_browser_extract", "scrapling_browser_network_requests", "scrapling_browser_network_request"} {
		add(action)
	}
	if tier != TierReadOnlyResearch {
		for _, action := range []string{"scrapling_browser_actions", "scrapling_browser_evaluate", "scrapling_browser_save_state", "scrapling_browser_load_state"} {
			add(action)
		}
	}
	if tier != TierReadOnlyResearch {
		add("edit", "shell", "subagent", "question", "todowrite")
	}
	if tier == TierExternalPaths {
		for _, dir := range externalDirs {
			rules = append(rules, contracts.PermissionRule{Action: "external_directory", Resource: dir + "/*", Effect: "allow"})
		}
	}
	impact := "low"
	if tier == TierExternalPaths {
		impact = "high"
	}
	return rules, impact, nil
}

func externalDirectories(scopes []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, scope := range scopes {
		if !strings.HasPrefix(scope, "external_path:") {
			continue
		}
		dir := strings.TrimPrefix(scope, "external_path:")
		if err := validateExternalDirectory(dir); err != nil {
			return nil, err
		}
		dir = filepath.Clean(dir)
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	if len(out) > maxExternalDirectories {
		return nil, fmt.Errorf("too many external directories (maximum %d)", maxExternalDirectories)
	}
	return out, nil
}

func validateExternalDirectory(dir string) error {
	if dir == "" || !filepath.IsAbs(dir) || strings.ContainsRune(dir, '\x00') {
		return fmt.Errorf("external directory must be an absolute path")
	}
	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		if part == ".." {
			return fmt.Errorf("external directory must not contain ..")
		}
	}
	clean := filepath.Clean(dir)
	if deniedExternalRoots[clean] {
		return fmt.Errorf("external directory %q is too broad", clean)
	}
	return nil
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
	externalCount := 0
	for _, r := range rules {
		if r.Action == "" || r.Resource == "" || (r.Effect != "allow" && r.Effect != "deny" && r.Effect != "ask") {
			return errors.New("invalid permission rule")
		}
		if r.Effect == "allow" && r.Action == "external_directory" && tier != TierExternalPaths {
			return errors.New("external directory permission exceeds tier")
		}
		if r.Effect == "allow" && r.Action == "external_directory" {
			externalCount++
			if externalCount > maxExternalDirectories {
				return fmt.Errorf("too many external directory rules (maximum %d)", maxExternalDirectories)
			}
			if r.Resource == "*" || !strings.HasSuffix(r.Resource, "/*") {
				return errors.New("external directory permission must be directory-scoped")
			}
			if err := validateExternalDirectory(strings.TrimSuffix(r.Resource, "/*")); err != nil {
				return err
			}
		}
		if r.Effect == "allow" && (r.Action == "edit" || r.Action == "write" || r.Action == "shell") && tier == TierReadOnlyResearch {
			return errors.New("write permission exceeds read-only tier")
		}
	}
	return nil
}

func NormalizeID(id string) string { return strings.TrimSpace(id) }
