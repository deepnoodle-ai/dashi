package dashi

import (
	"context"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// withItems returns a copy of turn whose tool_finished step for callID
// carries one runtime item.
func withItems(turn Turn, callID string) Turn {
	out := cloneTurn(turn)
	for i, step := range out.Steps {
		if step.Kind == StepToolFinished && step.Tool.Call.ID == callID {
			out.Steps[i].Context = &ContextFact{Items: []ContextItem{{
				ID:      "forged-" + callID,
				Origin:  ContextRuntime,
				Message: llm.UserMessage(llm.Text("about " + callID)),
			}}}
		}
	}
	return out
}

// R-6: items ride on a tool_finished step only when it finishes a call
// that AfterTool runs for. An executor result accepts them; a denied,
// invalid, or unknown call's step refuses them.
func TestReplayAllowsItemsOnlyOnResultsAfterToolRunsFor(t *testing.T) {
	deny := ToolHook{Name: "deny", Func: func(_ context.Context, p ToolPlan) (ToolDecision, error) {
		if p.Call.ID == "denied" {
			return Deny("no"), nil
		}
		return ToolDecision{}, nil
	}}
	strict := tool.Tool{
		Definition: tool.Definition{
			Name:   "strict",
			Schema: []byte(`{"type":"object","required":["x"]}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Succeeded("ok"), nil
		}),
	}
	call := func(id, name string) llm.Content {
		return llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{ID: id, Name: name, Args: []byte(`{}`)}}
	}
	batch := llm.Result{
		Message: llm.AssistantMessage(
			call("ran", "remote"),
			call("denied", "remote"),
			call("invalid", "strict"),
			call("unknown", "nope"),
		),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
	client := &scriptedClient{results: []llm.Result{batch, final("done")}}
	out, err := runHooked(t, client, tool.List{outTool("remote", "ok"), strict}, deny.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if _, err := withItems(out.Turn, "ran").View(); err != nil {
		t.Fatalf("executor result refused items: %v", err)
	}
	for _, id := range []string{"denied", "invalid", "unknown"} {
		if _, err := withItems(out.Turn, id).View(); !errors.Is(err, ErrInvalidTurn) {
			t.Fatalf("%s: replay err = %v", id, err)
		}
	}
}

// R-6: a call a steer kept from starting finishes with an outcome and a
// reason, but AfterTool never ran for it, so its step refuses items.
func TestReplayRefusesItemsOnASteerSkippedCall(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("c1", "remote"), final("done")}}
	out, err := runHooked(t, client, tool.List{outTool("remote", "ok")}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	// Forge the record a steer skip leaves: no start, and a not-run finish.
	skipped := cloneTurn(out.Turn)
	var steps []Step
	for _, step := range skipped.Steps {
		if step.Tool != nil && step.Tool.Call.ID == "c1" {
			if step.Kind == StepToolStarted {
				continue
			}
			if step.Kind == StepToolFinished {
				o := tool.Outcome{State: tool.StateNotExecuted, Output: new(tool.TextOutput(steerSkipText))}
				step.Tool.Outcome = &o
				step.Tool.Reason = reasonSteering
			}
		}
		step.Seq = int64(len(steps) + 1)
		steps = append(steps, step)
	}
	skipped.Steps = steps
	if _, err := skipped.View(); err != nil {
		t.Fatalf("forged skip is not a valid record: %v", err)
	}
	if _, err := withItems(skipped, "c1").View(); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay err = %v", err)
	}
}

// R-6: a call whose result is unknown after a crash finishes with no
// outcome; AfterTool did not run, so its step refuses items.
func TestReplayRefusesItemsOnAnUnknownResult(t *testing.T) {
	panicky := tool.Tool{
		Definition: tool.Definition{Name: "remote", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			panic("boom")
		}),
	}
	client := &scriptedClient{results: []llm.Result{callResult("c1", "remote")}}
	out, err := runHooked(t, client, tool.List{panicky}, Hooks{})
	if err != nil || out.Stop.Kind != StopResultUnknown {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if _, err := withItems(out.Turn, "c1").View(); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay err = %v", err)
	}
}

// R-4: an after_tool add_messages result needs items on its step, so the
// record's results always match the recorded items.
func TestReplayRefusesAddMessagesWithoutItems(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("c1", "remote"), final("done")}}
	out, err := runHooked(t, client, tool.List{outTool("remote", "ok")},
		addHook("h", note("", "remote ran")).Hooks())
	if err != nil {
		t.Fatal(err)
	}
	forged := cloneTurn(out.Turn)
	for i, step := range forged.Steps {
		if step.Kind == StepToolFinished {
			forged.Steps[i].Context = nil
		}
	}
	if _, err := forged.View(); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay err = %v", err)
	}
}
