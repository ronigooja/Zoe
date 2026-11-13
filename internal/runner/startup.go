package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/logging"
)

type controlHTTPError struct {
	operation string
	status    int
}

func (e controlHTTPError) Error() string {
	return safeHTTPError(e.operation, e.status).Error()
}

type controlDecodeError struct{ error }

func (e controlDecodeError) Unwrap() error { return e.error }

// TransientErrorKind applies to retryable control-plane transport and HTTP failures.
// Response decoding is a protocol failure, even when its cause is EOF.
func TransientErrorKind(err error) string {
	var decode controlDecodeError
	if errors.As(err, &decode) {
		return ""
	}
	var status controlHTTPError
	if errors.As(err, &status) {
		switch status.status {
		case 408, 429, 502, 503, 504:
			return fmt.Sprintf("http_%d", status.status)
		}
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "connection_reset"
	}
	if errors.Is(err, syscall.EPIPE) {
		return "broken_pipe"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof"
	}
	if errors.Is(err, io.EOF) {
		return "eof"
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "timeout"
	}
	return ""
}

// StartupCurrent retries only the initial read of assigned claims. Callers must
// not start recovery, workers, or claim polling until this method succeeds.
// A zero wait preserves a single request without a startup deadline.
func (e *Engine) StartupCurrent(ctx context.Context, wait time.Duration) ([]contracts.JobClaim, error) {
	requestCtx := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	delay := 500 * time.Millisecond
	var last error
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if requestCtx.Err() != nil && last != nil {
			return nil, last
		}
		claims, err := e.Control.Current(requestCtx)
		if err == nil {
			logging.Infof("runner startup status=ready attempt=%d", attempt)
			return claims, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		kind := TransientErrorKind(err)
		if wait <= 0 || kind == "" {
			return nil, err
		}
		logging.Warnf("runner startup status=waiting_for_server attempt=%d error_kind=%s", attempt, kind)
		if requestCtx.Err() != nil && last != nil {
			return nil, last
		}
		last = err
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-requestCtx.Done():
			timer.Stop()
			return nil, last
		case <-timer.C:
		}
		delay = min(2*delay, 5*time.Second)
	}
}
