package llm

import "time"

// HTTPTimeouts bounds one HTTP model call. Zero phase durations disable those
// bounds. A nil Total keeps the provider client's existing total behavior;
// an explicit zero removes only Dashi's default five-minute total bound.
// A supplied http.Client may still have its own shorter Timeout.
type HTTPTimeouts struct {
	// ResponseStart bounds dispatch through receipt of response headers.
	ResponseStart time.Duration
	// StreamIdle bounds each blocking raw body read of a streaming response.
	StreamIdle time.Duration
	// Total bounds the whole call. Nil keeps the existing default; zero
	// removes Dashi's total bound, but not a supplied client's own Timeout.
	Total *time.Duration
}
