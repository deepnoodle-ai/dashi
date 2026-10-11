package feed

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func canceledExternalTurn(t *testing.T) (*dashi.MemoryRecorder, dashi.Runner, dashi.RunResult) {
	t.Helper()
	rec := dashi.NewMemoryRecorder()
	runner := dashi.Runner{
		Client:   dashitest.NewClient(dashitest.CallTool("call-late", "remote", "{}")),
		Recorder: rec,
		Tools: tool.List{
			{
				Definition: tool.Definition{
					Name:        "remote",
					Description: "Run in the application.",
					Kind:        tool.KindExternal,
					Schema:      json.RawMessage(`{"type":"object"}`),
				},
			},
		},
	}
	first, err := runner.Run(t.Context(), dashi.Turn{ID: "late"}, dashi.Start{
		CommandID: "start",
		Agent: dashi.Agent{
			Provider: dashitest.Provider,
			API:      dashitest.API,
			Model:    dashitest.Model,
		},
		Input: []llm.Message{llm.UserMessage(llm.Text("Run remote."))},
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := runner.Run(
		t.Context(), first.Turn, dashi.Cancel{CommandID: "cancel"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return rec, runner, canceled
}

func TestTailReceivesLiveResultAfterEndedStop(t *testing.T) {
	rec, runner, canceled := canceledExternalTurn(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := make(chan dashi.Step)
	done := make(chan error, 1)
	go func() {
		for step, err := range Tail(ctx, rec, "late", 0) {
			if err != nil {
				done <- err
				return
			}
			select {
			case seen <- step:
			case <-ctx.Done():
			}
		}
		done <- nil
	}()
	for _, want := range canceled.Turn.Steps {
		got := nextTailStep(t, seen)
		if !got.Equal(want) {
			t.Fatalf("received seq %d; want original seq %d", got.Seq, want.Seq)
		}
	}
	late, err := runner.Run(t.Context(), canceled.Turn, dashi.Resolve{
		CommandID: "late-result",
		CallID:    "call-late",
		Outcome:   new(tool.Succeeded("Finished late.")),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := nextTailStep(t, seen)
	if !got.Equal(late.Turn.Steps[len(late.Turn.Steps)-1]) {
		t.Fatal("tail lost or changed the committed late result")
	}
	if late.Stop.Kind != dashi.StopCanceled {
		t.Fatalf("late result changed execution stop to %s", late.Stop.Kind)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation yielded an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tail did not finish cancellation")
	}
}

func TestTailResumesAtStopAndAfterLateResult(t *testing.T) {
	rec, runner, canceled := canceledExternalTurn(t)
	stopSeq := int64(len(canceled.Turn.Steps))
	late, err := runner.Run(t.Context(), canceled.Turn, dashi.Resolve{
		CommandID: "late-result",
		CallID:    "call-late",
		Outcome:   new(tool.Succeeded("Finished late.")),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, after := range []int64{-1, stopSeq, stopSeq + 1} {
		t.Run(strconv.FormatInt(after, 10), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			var got []dashi.Step
			for step, err := range Tail(ctx, rec, "late", after) {
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, step)
			}
			want := late.Turn.Steps[max(after, 0):]
			if !slices.EqualFunc(got, want, dashi.Step.Equal) {
				t.Fatalf("cursor %d received %d steps; want %d", after, len(got), len(want))
			}
		})
	}
	for range dashi.Follow(t.Context(), rec, "late", stopSeq) {
		t.Fatal("ordinary Follow yielded a result after an ended cursor")
	}
	for range Follow(t.Context(), nil, rec, "late", stopSeq) {
		t.Fatal("feed.Follow yielded a result after an ended cursor")
	}
}

func TestTailReadersCancelIndependently(t *testing.T) {
	rec, runner, canceled := canceledExternalTurn(t)
	stopSeq := int64(len(canceled.Turn.Steps))
	oneCtx, cancelOne := context.WithCancel(t.Context())
	defer cancelOne()
	ready := make(chan struct{}, 2)
	one := &tailWaitingReader{StepReader: rec, waiter: rec, ready: ready}
	two := &tailWaitingReader{StepReader: rec, waiter: rec, ready: ready}
	doneOne, doneTwo := make(chan error, 1), make(chan error, 1)
	seen := make(chan dashi.Step, 1)
	go func() {
		for _, err := range Tail(oneCtx, one, "late", stopSeq) {
			doneOne <- err
			return
		}
		doneOne <- nil
	}()
	go func() {
		for step, err := range Tail(t.Context(), two, "late", stopSeq) {
			if err == nil {
				seen <- step
			}
			doneTwo <- err
			return
		}
	}()
	for range 2 {
		select {
		case <-ready:
		case <-time.After(time.Second):
			t.Fatal("both readers did not reach the wait")
		}
	}
	cancelOne()
	select {
	case err := <-doneOne:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first reader did not cancel")
	}
	late, err := runner.Run(t.Context(), canceled.Turn, dashi.Resolve{
		CommandID: "late-result",
		CallID:    "call-late",
		Outcome:   new(tool.Succeeded("Finished late.")),
	})
	if err != nil {
		t.Fatal(err)
	}
	step := nextTailStep(t, seen)
	if step.Seq != int64(len(late.Turn.Steps)) {
		t.Fatalf("second reader received seq %d", step.Seq)
	}
	select {
	case err := <-doneTwo:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second reader did not finish its range break")
	}
}

type tailWaitingReader struct {
	dashi.StepReader
	waiter dashi.StepWaiter
	ready  chan<- struct{}
	once   sync.Once
}

func (r *tailWaitingReader) Wait(ctx context.Context, id string, after int64) error {
	r.once.Do(func() { r.ready <- struct{}{} })
	return r.waiter.Wait(ctx, id, after)
}

func TestTailRejectsAheadAndDiscontinuousCursors(t *testing.T) {
	rec, _, canceled := canceledExternalTurn(t)
	var errs []error
	for _, err := range Tail(t.Context(), rec, "late", int64(len(canceled.Turn.Steps))+1) {
		errs = append(errs, err)
	}
	if len(errs) != 1 || !errors.Is(errs[0], dashi.ErrCursorAhead) {
		t.Fatalf("ahead cursor errors = %v", errs)
	}
	for name, steps := range map[string][]dashi.Step{
		"wrong turn": {{TurnID: "other", Seq: 1}},
		"gap":        {{TurnID: "late", Seq: 1}, {TurnID: "late", Seq: 3}},
		"repeat":     {{TurnID: "late", Seq: 1}, {TurnID: "late", Seq: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			var errs []error
			for _, err := range Tail(t.Context(), tailPage(steps), "late", 0) {
				if err != nil {
					errs = append(errs, err)
				}
			}
			if len(errs) != 1 || !errors.Is(errs[0], dashi.ErrRecordInvalid) {
				t.Fatalf("discontinuous reader errors = %v", errs)
			}
		})
	}
}

type tailPage []dashi.Step

func (p tailPage) StepsAfter(context.Context, string, int64, int) ([]dashi.Step, error) {
	return p, nil
}

func nextTailStep(t *testing.T, seen <-chan dashi.Step) dashi.Step {
	t.Helper()
	select {
	case step := <-seen:
		return step
	case <-time.After(time.Second):
		t.Fatal("tail did not receive the committed step")
		return dashi.Step{}
	}
}
