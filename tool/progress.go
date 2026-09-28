package tool

import (
	"context"
	"encoding/json"
	"math"
)

// maxProgressData bounds Progress.Data. Progress is a live status for people,
// not a channel for results, so oversized detail is dropped rather than
// copied into every delivered event.
const maxProgressData = 64 << 10

// Progress is a snapshot of a running call, for people watching it. Each
// report replaces the previous one, so a snapshot must be self-contained.
// Progress is live-only: it is never recorded, never replayed, and never
// reaches the model; the call's Outcome remains its only result.
type Progress struct {
	// Message is a short status line, such as "47/100 files".
	Message string `json:"message,omitempty"`

	// Done is the work finished so far, in units the tool chooses.
	Done float64 `json:"done,omitempty"`

	// Total is the work expected in the same units; zero means unknown.
	Total float64 `json:"total,omitempty"`

	// Data is optional structured detail for custom displays. It is raw JSON,
	// not a map, so every delivered event can be copied exactly.
	Data json.RawMessage `json:"data,omitempty"`
}

type reporterKey struct{}

// Report publishes p for the call running under ctx. It never blocks the tool
// and does nothing when no one is listening, so tools may call it freely.
// Report at human rates, a few times a second at most; consumers merge rapid
// snapshots and throttle rendering. Report copies Data and adjusts rather than
// fails: it drops Data that is not valid JSON or exceeds 64 KiB, and treats a
// Done or Total that is NaN or infinite as zero.
func Report(ctx context.Context, p Progress) {
	fn, _ := ctx.Value(reporterKey{}).(func(Progress))
	if fn == nil {
		return
	}
	if len(p.Data) > maxProgressData || !json.Valid(p.Data) {
		p.Data = nil
	} else {
		p.Data = append(json.RawMessage(nil), p.Data...)
	}
	p.Done = finite(p.Done)
	p.Total = finite(p.Total)
	fn(p)
}

// WithReporter returns a context whose Report calls invoke fn, replacing any
// reporter already in ctx. Tests and adapters that run executors directly use
// it; the dashi runner installs its own for each call. fn runs synchronously
// on the reporting goroutine, may be called from several goroutines at once,
// and keeps receiving reports after Execute returns if the tool leaks ctx, so
// it must be safe for concurrent use and should return quickly. A nil fn
// silences Report.
func WithReporter(ctx context.Context, fn func(Progress)) context.Context {
	if fn == nil {
		fn = func(Progress) {}
	}
	return context.WithValue(ctx, reporterKey{}, fn)
}

func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}
