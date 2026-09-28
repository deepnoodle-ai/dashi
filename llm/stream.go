package llm

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
)

// ErrStreamConsumed means an effectful stream was ranged more than once.
var ErrStreamConsumed = errors.New("llm: stream already consumed")

// StreamEvent carries either a provisional Delta or the terminal Result of
// one model call. A terminal error is paired with the partial Result.
type StreamEvent struct {
	Delta  *Delta
	Result *Result
}

// Stream dispatches one call when iteration begins. It yields
// deltas with backpressure, then exactly one terminal result when fully read.
// Breaking the range cancels the request and suppresses the terminal event.
// The sequence is single-use: a second range yields ErrStreamConsumed instead
// of dispatching another paid request.
// The client must stop invoking its sink before Send returns. A non-nil
// call.Sink also observes each delta before it is yielded.
func Stream(ctx context.Context, client Client, call Call) iter.Seq2[StreamEvent, error] {
	var started atomic.Bool
	return func(yield func(StreamEvent, error) bool) {
		if !started.CompareAndSwap(false, true) {
			yield(StreamEvent{}, ErrStreamConsumed)
			return
		}
		if client == nil {
			yield(StreamEvent{}, errors.New("llm: client is required"))
			return
		}
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var mu sync.Mutex
		stopped := false
		observe := call.Sink
		call.Sink = func(delta Delta) {
			mu.Lock()
			defer mu.Unlock()
			if stopped {
				return
			}
			if delta.Content != nil {
				value := delta.Content.Clone()
				delta.Content = &value
			}
			if delta.Usage != nil {
				delta.Usage = delta.Usage.Copy()
			}
			if observe != nil {
				observe(delta)
			}
			if !yield(StreamEvent{Delta: &delta}, nil) {
				stopped = true
				cancel()
			}
		}
		result, err := client.Send(callCtx, call)
		mu.Lock()
		wasStopped := stopped
		mu.Unlock()
		if !wasStopped {
			yield(StreamEvent{Result: &result}, err)
		}
	}
}
