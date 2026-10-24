package providerhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/sse"
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

// StreamFailure classifies the error that ended an event stream. An oversized
// or malformed event is a protocol error, as it is in a complete response;
// anything else is a read failure.
func StreamFailure(provider string, err error) error {
	if errors.Is(err, sse.ErrEventTooLarge) || errors.Is(err, sse.ErrMalformedEvent) {
		return Failure(provider, llm.ErrorProtocol, "stream read: %w", err)
	}
	return ReadFailure(provider, err)
}

// Complete closes the successful decoding window. A timer that won before
// completion remains a failure even if the decoder had already buffered its
// terminal event. Stopping the total timer first prevents a late expiry from
// changing a result that completed in time.
func Complete(provider string, body io.ReadCloser) error {
	b, ok := body.(*timeoutBody)
	if !ok {
		return nil
	}
	b.stopTotal()
	if cause := context.Cause(b.ctx); cause != nil {
		return ReadFailure(provider, cause)
	}
	return nil
}

// timeoutTimer serializes expiry with completion. Timer.Stop alone does not
// wait for a callback already in progress.
type timeoutTimer struct {
	mu     sync.Mutex
	active bool
	timer  *time.Timer
	cancel context.CancelCauseFunc
	cause  error
}

func startTimeout(
	duration time.Duration,
	cancel context.CancelCauseFunc,
	cause error,
) *timeoutTimer {
	guard := &timeoutTimer{active: true, cancel: cancel, cause: cause}
	guard.timer = time.AfterFunc(duration, guard.fire)
	return guard
}

func (g *timeoutTimer) fire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active {
		g.active = false
		g.cancel(g.cause)
	}
}

func (g *timeoutTimer) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active = false
	g.timer.Stop()
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
	var timer *timeoutTimer
	if b.idle > 0 {
		timer = startTimeout(b.idle, b.cancel, streamIdleCause)
	}
	n, err := b.ReadCloser.Read(p)
	if timer != nil {
		timer.stop()
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
