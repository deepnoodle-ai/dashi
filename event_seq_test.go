package dashi

import (
	"context"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// loseSettle fails the n-th model_settled commit as a lost write, so the
// record ends at its model_requested step, as after a crash.
type loseSettle struct {
	*MemoryRecorder
	n, seen int
}

func (r *loseSettle) Commit(ctx context.Context, req CommitRequest) error {
	if req.Step.Kind == StepModelSettled {
		r.seen++
		if r.seen == r.n {
			return &CommitError{Persistence: PersistenceUnknown, Err: errors.New("lost")}
		}
	}
	return r.MemoryRecorder.Commit(ctx, req)
}

// Event.Seq places each preview after the step it follows: a delta at its
// message's model_requested step, a progress snapshot between its call's
// tool_started and tool_settled steps. This holds only while commit and
// emit share the run goroutine; a change that emits from a tool goroutine
// or commits from another goroutine breaks it.
func TestEventSeqPlacesPreviews(t *testing.T) {
	work := progressTool("work", func(ctx context.Context, call tool.Call) (tool.Outcome, error) {
		for i := range 3 {
			tool.Report(ctx, tool.Progress{Message: call.ID, Done: float64(i)})
		}
		return tool.Succeeded("ok"), nil
	})
	client := &scriptedClient{
		results: []llm.Result{
			namedCallsResult("work", "work", "work"),
			final("lost"), // its settle is lost, so the next run resends it
			final("done"),
		},
		deltas: []llm.Delta{{Kind: llm.DeltaText, Text: "a"}, {Kind: llm.DeltaText, Text: "b"}},
	}
	mem := NewMemoryRecorder()
	agent := testAgent()
	agent.Limits.ToolConcurrency = 3
	runner := Runner{
		Agent:    agent,
		Client:   client,
		Tools:    tool.List{work},
		Recorder: &loseSettle{MemoryRecorder: mem, n: 2},
	}
	var events []Event
	for e, err := range runner.Stream(t.Context(), Turn{ID: "t1"}, limitsStart()) {
		if e.Outcome == nil && err == nil {
			events = append(events, e)
		}
	}
	saved, err := mem.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if last := saved.Steps[len(saved.Steps)-1]; last.Kind != StepModelIntent {
		t.Fatalf("record ends at %s; want a model_requested to recover", last.Kind)
	}
	for e, err := range runner.Stream(t.Context(), saved, Continue{CommandID: "resume"}) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Outcome == nil {
			events = append(events, e)
		}
	}
	final, err := mem.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	requested := map[string]int64{}
	started := map[string]int64{}
	settled := map[string]int64{}
	for _, step := range final.Steps {
		switch step.Kind {
		case StepModelIntent:
			requested[step.Model.MessageID] = step.Seq
		case StepToolStarted:
			started[step.Tool.Call.ID] = step.Seq
		case StepToolSettled:
			settled[step.Tool.Call.ID] = step.Seq
		}
	}
	var deltas, progress int
	for _, e := range events {
		switch {
		case e.Step != nil:
			if e.Seq != e.Step.Seq {
				t.Fatalf("step %d carries Seq %d", e.Step.Seq, e.Seq)
			}
		case e.Delta != nil:
			deltas++
			if want, ok := requested[e.MessageID]; !ok || e.Seq != want {
				t.Fatalf("delta of %s at Seq %d; its model_requested is %d", e.MessageID, e.Seq, want)
			}
		case e.Progress != nil:
			progress++
			id := e.Progress.CallID
			if e.Seq < started[id] || e.Seq >= settled[id] {
				t.Fatalf("progress of %s at Seq %d; want [%d, %d)", id, e.Seq, started[id], settled[id])
			}
		}
	}
	if deltas != 2*3 || progress == 0 { // three sends, two deltas each
		t.Fatalf("deltas=%d progress=%d", deltas, progress)
	}
}
