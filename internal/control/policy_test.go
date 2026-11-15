package control

import "testing"

func TestClientPolicyLevelsAndRanks(t *testing.T) {
	store, server, _ := instanceAuthFixture(t)
	policy, err := server.ReadClientPolicy(t.Context())
	if err != nil || policy.LogLevel != "error" {
		t.Fatalf("default policy=%+v err=%v", policy, err)
	}
	for rank, level := range []string{"error", "warn", "info", "debug"} {
		if !validClientLogLevel(level) || diagnosticLevelRank(level) != rank {
			t.Fatalf("level=%s rank=%d want=%d", level, diagnosticLevelRank(level), rank)
		}
		policy, err = UpdateClientPolicy(t.Context(), store.DB(), level, 60)
		if err != nil || policy.LogLevel != level {
			t.Fatalf("policy=%+v err=%v", policy, err)
		}
	}
	if _, err := UpdateClientPolicy(t.Context(), store.DB(), "verbose", 60); err == nil {
		t.Fatal("invalid level accepted")
	}
}
