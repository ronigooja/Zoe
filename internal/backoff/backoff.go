// Package backoff defines the shared model retry policy.
package backoff

import (
	"context"
	"errors"
	"math"
	"net"
	"time"

	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/modelclient"
)

type Parameters struct {
	Base, Cap   time.Duration
	MaxAttempts int
}

var (
	Background  = Parameters{Base: 30 * time.Second, Cap: time.Hour}
	Interactive = Parameters{Base: time.Second, Cap: 8 * time.Second, MaxAttempts: 3}
)

type Kind string

const (
	Auth      Kind = "auth"
	Transient Kind = "transient"
	Permanent Kind = "permanent"
)

// Delay accepts a jitter sample in [0,1]; nil selects the midpoint.
func Delay(failures int, base, cap time.Duration, jitter func() float64) time.Duration {
	if failures < 1 || base <= 0 || cap <= 0 {
		return 0
	}
	d := base
	if d > cap {
		d = cap
	}
	for i := 1; i < failures && d < cap; i++ {
		if d > cap/2 {
			d = cap
		} else {
			d *= 2
		}
	}
	sample := 0.5
	if jitter != nil {
		sample = jitter()
	}
	if math.IsNaN(sample) {
		sample = 0.5
	}
	if sample < 0 {
		sample = 0
	}
	if sample > 1 {
		sample = 1
	}
	spread := 0.4
	if d == cap {
		spread = 0.2
	}
	value := float64(d) * (0.8 + spread*sample)
	if value >= float64(cap) {
		return cap
	}
	return time.Duration(value)
}

func Classify(err error) Kind {
	if errors.Is(err, memoryapi.ErrUpstreamUnauthorized) {
		return Auth
	}
	var worker *memoryapi.Error
	if errors.As(err, &worker) {
		switch worker.Kind {
		case "upstream_unauthorized", "unauthorized":
			return Auth
		case "busy", "deadline_exceeded", "backend_unavailable", "transport", "response_read":
			return Transient
		default:
			return Permanent
		}
	}
	var httpErr *modelclient.HTTPError
	if errors.As(err, &httpErr) {
		code := httpErr.StatusCode
		if code == 401 || code == 403 {
			return Auth
		}
		if code == 408 || code == 425 || code == 429 || code >= 500 && code <= 599 {
			return Transient
		}
		return Permanent
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) {
		return Transient
	}
	return Permanent
}
