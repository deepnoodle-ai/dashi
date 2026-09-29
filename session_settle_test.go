package dashi

import (
	"context"
	"fmt"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

// ErrRecordInvalid follows recorded steps, so it proves nothing about the
// start: the session loads the turn, and items its turn_started step holds
// are consumed, never delivered again. The engine returns it only for a
// runner or recorder bug, so the test drives the settle rule directly.
func TestSessionSettleAfterRecordInvalid(t *testing.T) {
	for _, started := range []bool{true, false} {
		t.Run(fmt.Sprintf("started=%v", started), func(t *testing.T) {
			ctx := t.Context()
			rec := NewMemoryRecorder()
			client := &scriptedClient{results: []llm.Result{
				{
					Message: llm.AssistantMessage(llm.Text("ok")),
					Stop:    llm.Stop{Kind: llm.StopFinished},
				},
				{
					Message: llm.AssistantMessage(llm.Text("ok")),
					Stop:    llm.Stop{Kind: llm.StopFinished},
				},
			}}
			s := &Session{
				ID:     "s",
				New:    true,
				Runner: Runner{Client: client, Recorder: rec},
				Agent:  Agent{Model: "fixture"},
			}
			if _, err := s.EnqueueItem(ctx, ContextItem{
				ID:      "a",
				Message: llm.UserMessage(llm.Text("a")),
			}); err != nil {
				t.Fatal(err)
			}
			const key = `queue:["a"]`
			s.mu.Lock()
			s.ready()
			res := s.reserve(s.free(nil), key, 1)
			s.mu.Unlock()
			if started {
				// The start committed before the record went bad.
				if _, err := (Runner{Client: client, Recorder: rec}).Run(
					ctx, NewTurn("s-1"), Start{
						CommandID: key,
						Agent:     Agent{Model: "fixture"},
						Input:     []llm.Message{llm.UserMessage(llm.Text("a"))},
					},
				); err != nil {
					t.Fatal(err)
				}
			}
			invalid := fmt.Errorf("%w: %w: replay failed", ErrRecordInvalid, ErrInvalidTurn)
			s.mu.Lock()
			// The failed run returned the recorded steps, so the handle
			// made the session.
			s.created = started
			err := s.settle(ctx, res, false, invalid)
			n := len(s.queue)
			s.mu.Unlock()
			if err != invalid {
				t.Fatalf("settle returned %v", err)
			}
			want := 1
			if started {
				want = 0
			}
			if n != want {
				t.Fatalf("queued=%d, want %d", n, want)
			}
			if !started {
				return
			}
			// A later item runs alone: "a" is not delivered twice.
			if _, err := s.Enqueue(ctx, "b"); err != nil {
				t.Fatal(err)
			}
			out, ran, err := s.RunQueue(ctx)
			if err != nil || !ran || out.Turn.ID != "s-2" {
				t.Fatalf("ran=%v turn=%s err=%v", ran, out.Turn.ID, err)
			}
			in := out.Turn.Steps[0].Started.Input
			if len(in) != 1 || in[0].Text() != "b" {
				t.Fatalf("input=%v", in)
			}
		})
	}
}

// notStarted names the proofs that a Start's turn_started step was not
// recorded.
func TestSessionNotStarted(t *testing.T) {
	started := &Step{Kind: StepStarted}
	model := &Step{Kind: StepModelIntent}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"rejected command", context.Canceled, true},
		{"start rejected", &CommitError{Persistence: PersistenceRejected, Step: started}, true},
		{"start conflicts", &CommitError{Persistence: PersistenceUnknown, Step: started, Err: ErrRevisionConflict}, true},
		{"start key taken", &CommitError{Persistence: PersistenceUnknown, Step: started, Err: ErrCommandConflict}, true},
		{"start unknown", &CommitError{Persistence: PersistenceUnknown, Step: started}, false},
		{"model rejected", &CommitError{Persistence: PersistenceRejected, Step: model}, false},
		{"record invalid", fmt.Errorf("%w: x", ErrRecordInvalid), false},
		{"stream stopped", errStreamStopped, false},
	}
	for _, tc := range cases {
		if got := notStarted(tc.err); got != tc.want {
			t.Errorf("%s: notStarted=%v, want %v", tc.name, got, tc.want)
		}
	}
}
