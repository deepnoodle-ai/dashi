package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// ErrRecordInvalid follows recorded steps, so it proves nothing about the
// start: the session loads the turn, and items its turn_started step holds
// are consumed, never added again. The engine returns it only for a
// runner or recorder bug, so the test drives the decideReserved rule directly.
func TestSessionResolveAfterRecordInvalid(t *testing.T) {
	for _, started := range []bool{true, false} {
		t.Run(fmt.Sprintf("started=%v", started), func(t *testing.T) {
			ctx := t.Context()
			rec := dashi.NewMemoryRecorder()
			client := okClient{}
			s := &Session{
				ID:     "s",
				New:    true,
				Runner: dashi.Runner{Client: client, Recorder: rec},
				Agent:  dashi.Agent{Model: "fixture"},
			}
			if _, err := s.EnqueueItem(ctx, dashi.ContextItem{
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
				if _, err := (dashi.Runner{Client: client, Recorder: rec}).Run(
					ctx, dashi.NewTurn("s-1"), dashi.Start{
						CommandID: key,
						Agent:     dashi.Agent{Model: "fixture"},
						Input:     []llm.Message{llm.UserMessage(llm.Text("a"))},
					},
				); err != nil {
					t.Fatal(err)
				}
			}
			invalid := fmt.Errorf("%w: %w: replay failed", dashi.ErrRecordInvalid, dashi.ErrInvalidTurn)
			s.mu.Lock()
			// The failed run returned the recorded steps, so the handle
			// made the session.
			s.created = started
			err := s.decideReserved(ctx, res, false, invalid)
			n := len(s.queue)
			s.mu.Unlock()
			if err != invalid {
				t.Fatalf("decideReserved returned %v", err)
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
			// A later item runs alone: "a" is not added twice.
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
	started := &dashi.Step{Kind: dashi.StepTurnStarted}
	model := &dashi.Step{Kind: dashi.StepModelCallStarted}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"rejected command", context.Canceled, true},
		{"start rejected", &dashi.CommitError{NothingWritten: true, Step: started}, true},
		{"start conflicts", &dashi.CommitError{Step: started, Err: dashi.ErrStaleTurn}, true},
		{"start key taken", &dashi.CommitError{Step: started, Err: dashi.ErrCommandIDReused}, true},
		{"start unknown", &dashi.CommitError{Step: started}, false},
		{"model rejected", &dashi.CommitError{NothingWritten: true, Step: model}, false},
		{"record invalid", fmt.Errorf("%w: x", dashi.ErrRecordInvalid), false},
		{"stream stopped", errStreamStopped, false},
	}
	for _, tc := range cases {
		if got := notStarted(tc.err); got != tc.want {
			t.Errorf("%s: notStarted=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// okClient answers every model call with "ok". The internal tests cannot
// use package dashitest, which imports this package.
type okClient struct{}

func (okClient) Provider() string { return "test" }

func (okClient) API() string { return "test.chat" }

func (okClient) ModelInfo(string) (llm.ModelInfo, bool) { return llm.ModelInfo{}, false }

func (okClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{"ok":true}`),
	})
}

func (okClient) Send(context.Context, llm.Call) (llm.Result, error) {
	return llm.Result{
		Message:   llm.AssistantMessage(llm.Text("ok")),
		Stop:      llm.Stop{Kind: llm.StopFinished},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}, nil
}
