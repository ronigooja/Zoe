package backoff

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/modelclient"
)

func TestDelay(t *testing.T) {
	for _, tc := range []struct {
		failures int
		sample   float64
		want     time.Duration
	}{
		{1, .5, 30 * time.Second}, {2, .5, time.Minute}, {3, .5, 2 * time.Minute},
		{1, 0, 24 * time.Second}, {1, 1, 36 * time.Second},
		{8, 0, 48 * time.Minute}, {8, 1, time.Hour},
		{1000000, .5, 54 * time.Minute}, {1000000, 1, time.Hour}, {1000000, 0, 48 * time.Minute},
	} {
		got := Delay(tc.failures, Background.Base, Background.Cap, func() float64 { return tc.sample })
		if got != tc.want {
			t.Errorf("delay(%d,%v)=%v want %v", tc.failures, tc.sample, got, tc.want)
		}
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want Kind
	}{
		{&modelclient.HTTPError{StatusCode: 401}, Auth}, {&modelclient.HTTPError{StatusCode: 403}, Auth},
		{&modelclient.HTTPError{StatusCode: 408}, Transient}, {&modelclient.HTTPError{StatusCode: 425}, Transient},
		{&modelclient.HTTPError{StatusCode: 429}, Transient}, {&modelclient.HTTPError{StatusCode: 503}, Transient},
		{&modelclient.HTTPError{StatusCode: 400}, Permanent}, {&modelclient.HTTPError{StatusCode: 404}, Permanent},
		{memoryapi.ErrUpstreamUnauthorized, Auth}, {&memoryapi.Error{Kind: "backend_unavailable"}, Transient},
		{context.DeadlineExceeded, Transient}, {&net.DNSError{Err: "network"}, Transient}, {errors.New("invalid output"), Permanent},
	} {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%v: got %s want %s", tc.err, got, tc.want)
		}
	}
}
