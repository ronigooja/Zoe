package main

import "testing"

func TestUIControllerOptionalConfiguration(t *testing.T) {
	reads := 0
	read := func(string) ([]byte, error) { reads++; return []byte("fixture"), nil }
	c, err := uiControllerFromEnv(func(string) string { return "" }, read)
	if err != nil || c != nil || reads != 0 {
		t.Fatalf("unconfigured client=%v reads=%d err=%v", c, reads, err)
	}
	for _, tc := range []struct {
		timeout string
		valid   bool
	}{{"", true}, {"500", true}, {"120000", true}, {"499", false}, {"120001", false}} {
		env := map[string]string{"ZOE_UI_CONTROLLER_BASE_URL": "https://ui.invalid", "ZOE_UI_CONTROLLER_MODEL": "screen", "ZOE_UI_CONTROLLER_API_KEY_FILE": "fixture-reference", "ZOE_UI_CONTROLLER_TIMEOUT_MS": tc.timeout}
		c, err := uiControllerFromEnv(func(k string) string { return env[k] }, read)
		if (err == nil) != tc.valid || (tc.valid && c == nil) {
			t.Fatalf("timeout=%q client=%v err=%v", tc.timeout, c, err)
		}
	}
}
