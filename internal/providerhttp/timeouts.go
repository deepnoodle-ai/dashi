package providerhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// TimeoutReason reports the Dashi timeout that ended a call, or "" when err
// carries none. Only causes set by Post or TimeoutTransport count.
func TimeoutReason(err error) llm.ErrorReason {
	if cause, ok := errors.AsType[*timeoutCause](err); ok {
		return cause.reason
	}
	return ""
}

// ReadFailure normalizes only Dashi-owned timeout causes. Provider protocol
// errors must not be passed here; a coincident timer must not relabel them.
func ReadFailure(provider string, err error) error {
	failure := Failure(provider, llm.ErrorTransport, "response read: %w", err)
	failure.(*llm.Error).Reason = TimeoutReason(err)
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

// defaultTotal bounds a call whose timeouts leave Total nil and whose
// http.Client Dashi built.
const defaultTotal = 5 * time.Minute

// callTotal resolves the total bound for one call. An explicit Total wins.
// Otherwise a client Dashi built gets the default, and a supplied client keeps
// its own total behavior. Zero means no Dashi total bound.
func callTotal(timeouts llm.HTTPTimeouts, supplied bool) time.Duration {
	if timeouts.Total != nil {
		return *timeouts.Total
	}
	if supplied {
		return 0
	}
	return defaultTotal
}

// bound performs one dispatch through do under timeouts. The total timer
// runs until the returned body is closed or Complete is called, so it also
// bounds reading a streamed body. Errors carry a Dashi timeout cause when one
// fired; callers convert them with ReadFailure.
func bound(
	r *http.Request,
	timeouts llm.HTTPTimeouts,
	total time.Duration,
	stream bool,
	do func(*http.Request) (*http.Response, error),
) (*http.Response, error) {
	callCtx, cancel := context.WithCancelCause(r.Context())
	r = r.WithContext(callCtx)
	stopTotal := func() {}
	if total > 0 {
		timer := startTimeout(total, cancel, callTotalCause)
		stopTotal = timer.stop
	}
	var startTimer *timeoutTimer
	if timeouts.ResponseStart > 0 {
		startTimer = startTimeout(
			timeouts.ResponseStart,
			cancel,
			responseStartCause,
		)
	}
	resp, err := do(r)
	if startTimer != nil {
		startTimer.stop()
	}
	if err != nil {
		stopTotal()
		cause := context.Cause(callCtx)
		cancel(nil)
		if TimeoutReason(cause) != "" {
			err = fmt.Errorf("%w: %w", cause, err)
		}
		return nil, err
	}
	if cause := context.Cause(callCtx); TimeoutReason(cause) != "" {
		resp.Body.Close()
		stopTotal()
		cancel(nil)
		return nil, cause
	}
	idle := time.Duration(0)
	if stream {
		idle = timeouts.StreamIdle
	}
	resp.Body = &timeoutBody{
		ReadCloser: resp.Body,
		ctx:        callCtx,
		cancel:     cancel,
		stopTotal:  stopTotal,
		idle:       idle,
	}
	return resp, nil
}

// TimeoutTransport bounds each round trip through next as Post bounds its
// dispatch, for providers whose HTTP calls go through an SDK. supplied reports
// that the application supplied the http.Client: with a nil Total, that
// client keeps its own total behavior instead of the five-minute default.
// stream applies StreamIdle to body reads. Callers report a timeout with
// ReadFailure when TimeoutReason finds one, and call Complete or Expired with
// the response body after a successful decode.
func TimeoutTransport(
	next http.RoundTripper,
	timeouts llm.HTTPTimeouts,
	supplied, stream bool,
) http.RoundTripper {
	return timeoutTransport{
		next:     next,
		timeouts: timeouts,
		total:    callTotal(timeouts, supplied),
		stream:   stream,
	}
}

type timeoutTransport struct {
	next     http.RoundTripper
	timeouts llm.HTTPTimeouts
	total    time.Duration
	stream   bool
}

func (t timeoutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return bound(r, t.timeouts, t.total, t.stream, t.next.RoundTrip)
}

// ValidTimeouts reports whether every duration in timeouts is nonnegative.
func ValidTimeouts(timeouts llm.HTTPTimeouts) bool {
	return timeouts.ResponseStart >= 0 && timeouts.StreamIdle >= 0 &&
		(timeouts.Total == nil || *timeouts.Total >= 0)
}

// Expired reports a Dashi timeout that fired before body was closed, as
// Complete does, for callers whose SDK closes the body before they can call
// Complete. It ignores a parent context's cancellation, and returns nil for a
// body that TimeoutTransport did not return.
func Expired(provider string, body io.ReadCloser) error {
	b, ok := body.(*timeoutBody)
	if !ok {
		return nil
	}
	b.stopTotal()
	if cause := context.Cause(b.ctx); TimeoutReason(cause) != "" {
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
	if cause := context.Cause(b.ctx); TimeoutReason(cause) != "" &&
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
