package opencode

import "testing"

func TestPermissionRequestAndFormAreTypedPendingItems(t *testing.T) {
	if PermissionOnce != "once" || PermissionReject != "reject" {
		t.Fatal("unexpected permission decisions")
	}
	if (PermissionRequest{ID: "p", Action: "write"}).Action != "write" {
		t.Fatal("permission shape")
	}
	if (FormRequest{ID: "f", Title: "Questions"}).Title != "Questions" {
		t.Fatal("form shape")
	}
}
