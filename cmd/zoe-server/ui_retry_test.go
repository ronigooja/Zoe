package main

import (
	"strings"
	"testing"
)

func TestUIControllerMaxAttempts(t *testing.T) {
	for _, raw := range []string{"", "0", "1", "100", "-1", "private-invalid"} {
		n, err := uiControllerMaxAttempts(func(string) string { return raw })
		bad := raw == "-1" || raw == "private-invalid"
		if (err != nil) != bad || (!bad && n < 0) {
			t.Fatal(raw, n, err)
		}
		if bad && (!strings.Contains(err.Error(), "ZOE_UI_CONTROLLER_MAX_ATTEMPTS") || strings.Contains(err.Error(), raw)) {
			t.Fatal(err)
		}
	}
}
