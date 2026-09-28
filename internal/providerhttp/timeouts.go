package providerhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

type timeoutCause struct {
	reason llm.ErrorReason
}

func (e *timeoutCause) Error() string {
	return string(e.reason)
}

func (e *timeoutCause) Unwrap() error {
	return context.DeadlineExceeded
}

var (
	responseStartCause = &timeoutCause{llm.ReasonResponseStartTimeout}
	streamIdleCause    = &timeoutCause{llm.ReasonStreamIdleTimeout}
	callTotalCause     = &timeoutCause{llm.ReasonCallTimeout}
)

func timeoutReason(err error) llm.ErrorReason {
	if cause, ok := errors.AsType[*timeoutCause](err); ok {
		return cause.reason
	}
	return ""
}

// ReadFailure normalizes only Dashi-owned timeout causes. Provider protocol
// errors must not be passed here; a coincident timer must not relabel them.
func ReadFailure(provider string, err error) error {
	failure := Failure(provider, llm.ErrorTransport, "response read: %w", err)
	failure.(*llm.Error).Reason = timeoutReason(err)
	return failure
}

// timeoutBody holds the model-call context until the body is closed. The idle
// timer exists only during a raw Read, so consumer processing is not idleness.
type timeoutBody struct {
	io.ReadCloser
	ctx       context.Context
	cancel    context.CancelCauseFunc
	stopTotal func()
	idle      time.Duration
}

func (b *timeoutBody) Read(p []byte) (int, error) {
	var timer *time.Timer
	var reading atomic.Bool
	if b.idle > 0 {
		reading.Store(true)
		timer = time.AfterFunc(b.idle, func() {
			if reading.CompareAndSwap(true, false) {
				b.cancel(streamIdleCause)
			}
		})
	}
	n, err := b.ReadCloser.Read(p)
	if timer != nil {
		reading.Store(false)
		timer.Stop()
	}
	if cause := context.Cause(b.ctx); timeoutReason(cause) != "" &&
		(err != nil || n == 0) {
		if err != nil {
			return n, fmt.Errorf("%w: %w", cause, err)
		}
		return n, cause
	}
	return n, err
}

func (b *timeoutBody) Close() error {
	b.stopTotal()
	b.cancel(nil)
	return b.ReadCloser.Close()
}
