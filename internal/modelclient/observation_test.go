package modelclient

import (
	"strings"
	"testing"
)

func TestObservationTextFiltersAndBounds(t *testing.T) {
	text, truncated := observationText(`configured-secret Bearer abc.def password=hello sk-abcdefghijk`, "configured-secret")
	if truncated {
		t.Fatal("unexpected truncation")
	}
	for _, secret := range []string{"configured-secret", "abc.def", "hello", "sk-abcdefghijk"} {
		if strings.Contains(text, secret) {
			t.Fatalf("secret %q was not redacted: %s", secret, text)
		}
	}
	text, truncated = observationText(strings.Repeat("x", observationTextLimit+1), "")
	if !truncated || strings.Contains(text, "xxx") {
		t.Fatal("oversized text was not omitted")
	}
}

func TestObservationComponents(t *testing.T) {
	for prefix, want := range map[string]string{"ZOE_REGISTER": "register", "ZOE_COMPACT": "compact", "ZOE_STEWARD": "steward", "ZOE_UI_CONTROLLER": "ui_controller", "PROBE": ""} {
		if got := observationComponent(prefix); got != want {
			t.Errorf("%s: got %q want %q", prefix, got, want)
		}
	}
}
