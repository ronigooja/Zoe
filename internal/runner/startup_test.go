package runner

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startupTestEngine(t *testing.T, server *httptest.Server, client *http.Client) *Engine {
	t.Helper()
	control, err := NewControlClient(server.URL, "secret-runner-token", client)
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Control: control}
}

func TestStartupCurrentRetriesHTTPAndSanitizesLogs(t *testing.T) {
	for _, status := range []int{408, 429, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/runner/v1/claims/current" {
					t.Errorf("unexpected startup operation: %s", r.URL.Path)
				}
				if calls.Add(1) <= 2 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte("secret-response-body"))
					return
				}
				_, _ = w.Write([]byte(`{"claims":[]}`))
			}))
			defer server.Close()
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(previous)
			_, err := startupTestEngine(t, server, server.Client()).StartupCurrent(context.Background(), 5*time.Second)
			if err != nil || calls.Load() != 3 {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
			if strings.Count(logs.String(), "status=waiting_for_server") != 2 || !strings.Contains(logs.String(), "runner startup status=ready attempt=3") {
				t.Fatalf("unexpected status logs: %s", &logs)
			}
			for _, secret := range []string{"secret-runner-token", "secret-response-body", server.URL} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("startup logs contain sensitive data")
				}
			}
		})
	}
}

func TestStartupCurrentRetriesConnectionRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"claims":[]}`))
	}))
	defer server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddress := listener.Addr().String()
	_ = listener.Close()
	var attempts atomic.Int32
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if attempts.Add(1) <= 2 {
			address = closedAddress
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	defer transport.CloseIdleConnections()
	_, err = startupTestEngine(t, server, &http.Client{Transport: transport}).StartupCurrent(context.Background(), 5*time.Second)
	if err != nil || attempts.Load() != 3 {
		t.Fatalf("attempts=%d err=%v", attempts.Load(), err)
	}
}

func TestStartupCurrentImmediateErrorsAndZeroWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		wait   time.Duration
	}{
		{"unauthorized", 401, "secret-response-body", time.Second},
		{"forbidden", 403, "", time.Second},
		{"malformed", 200, "not JSON", time.Second},
		{"zero_wait", 503, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			start := time.Now()
			_, err := startupTestEngine(t, server, server.Client()).StartupCurrent(context.Background(), tc.wait)
			if err == nil || calls.Load() != 1 || time.Since(start) >= 500*time.Millisecond {
				t.Fatalf("expected immediate single failure: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestStartupCurrentCapReturnsLastError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	start := time.Now()
	_, err := startupTestEngine(t, server, server.Client()).StartupCurrent(context.Background(), 650*time.Millisecond)
	var status currentClaimsHTTPError
	if !errors.As(err, &status) || status.status != 503 || calls.Load() != 2 || time.Since(start) > time.Second {
		t.Fatalf("expected bounded wait with last HTTP error: calls=%d err=%v", calls.Load(), err)
	}
}

func TestStartupCurrentCancellation(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(map[bool]string{false: "backoff", true: "request"}[inFlight], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			called := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(called)
				if inFlight {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(503)
			}))
			defer server.Close()
			engine := startupTestEngine(t, server, server.Client())
			done := make(chan error, 1)
			go func() {
				_, err := engine.StartupCurrent(ctx, 120*time.Second)
				done <- err
			}()
			<-called
			if !inFlight {
				time.Sleep(20 * time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected cancellation, got %v", err)
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatal("startup did not stop promptly")
			}
		})
	}
}

func TestStartupCurrentRetriesTimeout(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"claims":[]}`))
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 50 * time.Millisecond
	_, err := startupTestEngine(t, server, client).StartupCurrent(context.Background(), 3*time.Second)
	if err != nil || calls.Load() != 2 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}
