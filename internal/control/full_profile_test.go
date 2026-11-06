package control

import (
	"context"
	"testing"

	"github.com/ronigooja/Zoe/internal/storage"
)

func TestFullTierProfileAdmission(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []string{"zoe-super", "zoe-worker", "custom", "zoe-steward"} {
		p := RuntimeProfile{ID: id, Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "worker", Agent: id, PermissionTier: "full", IsolationPolicy: "opencode-workspace-write-v1"}
		_, err := NewServer(store, Config{Profiles: map[string]RuntimeProfile{id: p}})
		if id == "zoe-steward" {
			if err != nil {
				t.Fatalf("resident Worker rejected: %v", err)
			}
		} else if err == nil {
			t.Errorf("unauthorized profile %s admitted full tier", id)
		}
	}
	for _, mismatch := range []RuntimeProfile{
		{ID: "zoe-super", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "worker", Agent: "zoe-steward", PermissionTier: "external_paths", IsolationPolicy: "opencode-workspace-write-v1"},
		{ID: "zoe-steward", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "super", Agent: "zoe-steward", PermissionTier: "full", IsolationPolicy: "opencode-workspace-write-v1"},
		{ID: "zoe-steward", Harness: "opencode", RuntimeRouteID: "opencode", ModelRouteID: "default", Role: "worker", Agent: "zoe-super", PermissionTier: "full", IsolationPolicy: "opencode-workspace-write-v1"},
	} {
		if _, err := NewServer(store, Config{Profiles: map[string]RuntimeProfile{mismatch.ID: mismatch}}); err == nil {
			t.Errorf("full tier admitted mismatched identity: %+v", mismatch)
		}
	}
}
