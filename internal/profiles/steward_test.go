package profiles

import "testing"

func TestStewardExecutorHasWorkerTools(t *testing.T) {
	p, ok := ByID("zoe-steward")
	if !ok || p.PermissionTier != TierExternalPaths {
		t.Fatalf("worker profile=%+v", p)
	}
	rules, _, err := EffectivePermissionSet(p, p.ScopeDefaults, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"edit", "shell", "subagent"} {
		found := false
		for _, r := range rules {
			if r.Action == tool && r.Effect == "allow" {
				found = true
			}
		}
		if !found {
			t.Errorf("worker tool %s is not allowed", tool)
		}
	}
}
