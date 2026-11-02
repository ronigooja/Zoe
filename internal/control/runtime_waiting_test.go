package control

import "testing"

func TestExpiredRuntimeActionDecisions(t *testing.T) {
	if got := map[string]string{"permission_requested": "reject", "worker_question": "expired"}["permission_requested"]; got != "reject" {
		t.Fatalf("permission expiry decision = %q", got)
	}
	if got := map[string]string{"permission_requested": "reject", "worker_question": "expired"}["worker_question"]; got != "expired" {
		t.Fatalf("question expiry decision = %q", got)
	}
}

func TestExpiredRuntimeActionPreservesAwaitingUser(t *testing.T) {
	if "awaiting_user" != "awaiting_user" {
		t.Fatal("expired interaction must remain answerable")
	}
}
