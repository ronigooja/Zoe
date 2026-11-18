package steward

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/modelclient"
)

func TestInteractiveRetry(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		status, successAt, calls int
		delays                   []time.Duration
	}{
		{"exhaust", 503, 0, 3, []time.Duration{time.Second, 2 * time.Second}},
		{"success", 503, 2, 2, []time.Duration{time.Second}},
		{"permanent", 400, 0, 1, nil},
		{"unauthorized", 401, 0, 1, nil},
		{"forbidden", 403, 0, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var delays []time.Duration
			result, err := RetryComplete(context.Background(), func(context.Context) (string, error) {
				calls++
				if calls == tc.successAt {
					return "ok", nil
				}
				return "", &modelclient.HTTPError{StatusCode: tc.status}
			}, func() float64 { return 0.5 }, func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil })
			if calls != tc.calls || !reflect.DeepEqual(delays, tc.delays) {
				t.Fatalf("calls=%d delays=%v", calls, delays)
			}
			if tc.successAt > 0 && (err != nil || result != "ok") {
				t.Fatalf("result=%q err=%v", result, err)
			}
			if tc.successAt == 0 && err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestChooseDeadlineBoundsRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	c := configured(t, server.URL, "")
	c.timeout = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Choose(ctx, Input{Request: "request"}); err == nil {
		t.Fatal("expected failure")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("deadline exceeded: %v", elapsed)
	}
}

func TestInteractiveRetryContext(t *testing.T) {
	for _, short := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		if short {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
		}
		calls, sleeps := 0, 0
		_, err := RetryComplete(ctx, func(context.Context) (string, error) { calls++; return "", &modelclient.HTTPError{StatusCode: 503} }, func() float64 { return .5 }, func(context.Context, time.Duration) error { sleeps++; cancel(); return context.Canceled })
		cancel()
		if calls != 1 || err == nil || (short && sleeps != 0) || (!short && (sleeps != 1 || !errors.Is(err, context.Canceled))) {
			t.Fatalf("short=%t calls=%d sleeps=%d err=%v", short, calls, sleeps, err)
		}
	}
}
