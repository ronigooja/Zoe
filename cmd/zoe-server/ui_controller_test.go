package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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

func TestUIControllerConfiguredTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout string
		want    time.Duration
	}{
		{"default", "", 30 * time.Second},
		{"configured", "500", 500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				<-r.Context().Done()
			}))
			defer server.Close()
			env := map[string]string{"ZOE_UI_CONTROLLER_BASE_URL": server.URL, "ZOE_UI_CONTROLLER_MODEL": "screen", "ZOE_UI_CONTROLLER_API_KEY_FILE": "fixture-reference", "ZOE_UI_CONTROLLER_TIMEOUT_MS": tc.timeout}
			c, err := uiControllerFromEnv(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("fixture"), nil })
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			_, err = c.Complete(context.Background(), "fixture", "fixture")
			elapsed := time.Since(started)
			if !errors.Is(err, context.DeadlineExceeded) || elapsed < tc.want || elapsed > tc.want+5*time.Second {
				t.Fatalf("elapsed=%s want=%s err=%v", elapsed, tc.want, err)
			}
		})
	}
}

func TestUIControllerMaxRetries(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
		want  int
	}{
		{"", true, 0}, {"0", true, 0}, {"1", true, 1}, {"2", true, 2}, {"3", true, 3},
		{"-1", false, 0}, {"4", false, 0}, {"x", false, 0}, {"1.5", false, 0}, {" ", false, 0},
	} {
		got, err := uiControllerMaxRetries(func(string) string { return tc.value })
		if (err == nil) != tc.valid || got != tc.want {
			t.Fatalf("value %q: got %d err %v", tc.value, got, err)
		}
		if err != nil && (!strings.Contains(err.Error(), "ZOE_UI_CONTROLLER_MAX_RETRIES") || strings.Contains(err.Error(), tc.value)) {
			t.Fatalf("unsafe error %q", err)
		}
	}
}
