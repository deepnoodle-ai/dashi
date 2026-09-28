package dashi

import (
	"context"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestRunnerTimeoutRetainsOrderedMixedPartialData(t *testing.T) {
	reasoning := llm.Content{
		Kind:      llm.ContentReasoning,
		Reasoning: &llm.Reasoning{Text: "thought"},
	}
	textC := llm.Text("C")
	client := &scriptedClient{
		results: []llm.Result{{}},
		errors: []error{&llm.Error{
			Provider: "test", Category: llm.ErrorTransport,
			Reason: llm.ReasonStreamIdleTimeout,
		}},
		deltas: []llm.Delta{
			{Kind: llm.DeltaText, Index: 0, Text: "A"},
			{Kind: llm.DeltaContent, Index: 1, Content: &reasoning},
			{Kind: llm.DeltaText, Index: 2, Text: "B"},
			// A snapshot takes priority if a client also supplies Text.
			{Kind: llm.DeltaText, Index: 3, Text: "duplicate", Content: &textC},
			{Kind: llm.DeltaUsage, Usage: &llm.Usage{InputTokens: 5, OutputTokens: 1}},
			{Kind: llm.DeltaUsage, Usage: &llm.Usage{OutputTokens: 2}},
		},
	}
	out, err := start(t, client, nil, NewMemoryRecorder(), Hooks{})
	if err != nil || out.Stop.Kind != StopUncertain || out.Answer != "" {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
	var settled *ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelSettled {
			settled = step.Model
		}
	}
	if settled == nil || settled.ErrorReason != llm.ReasonStreamIdleTimeout ||
		settled.Failure != llm.FailureUnknown || !settled.Uncertain ||
		!settled.EstimatedUsage || settled.Result == nil ||
		settled.Result.Stop.Kind != llm.StopIncomplete {
		t.Fatalf("settlement=%+v", settled)
	}
	parts := settled.Result.Message.Content
	if len(parts) != 4 || parts[0].Block.Text.Text != "A" ||
		parts[1].Reasoning.Text != "thought" ||
		parts[2].Block.Text.Text != "B" ||
		parts[3].Block.Text.Text != "C" ||
		settled.Result.Message.Text() != "ABC" {
		t.Fatalf("observed content=%+v", parts)
	}
	if settled.Result.Usage == nil || settled.Result.Usage.InputTokens != 5 ||
		settled.Result.Usage.OutputTokens != 2 || out.Spend.Usage.OutputTokens != 2 {
		t.Fatalf("usage=%+v spend=%+v", settled.Result.Usage, out.Spend)
	}
	replayed, err := Replay(out.Turn)
	if err != nil || replayed.Answer != "" || replayed.Stop.Kind != StopUncertain {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
}

type timeoutThenCancelClient struct {
	*scriptedClient
	cancel context.CancelFunc
}

func (c timeoutThenCancelClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	result, err := c.scriptedClient.Send(ctx, call)
	c.cancel()
	return result, err
}

func TestRunnerKeepsProviderTimeoutWhenHostCancelsLater(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := timeoutThenCancelClient{
		scriptedClient: &scriptedClient{
			results: []llm.Result{{}},
			errors: []error{&llm.Error{
				Provider: "test",
				Category: llm.ErrorTransport,
				Reason:   llm.ReasonStreamIdleTimeout,
			}},
		},
		cancel: cancel,
	}
	out, err := (Runner{
		Agent:    testAgent(),
		Client:   client,
		Recorder: NewMemoryRecorder(),
	}).Run(ctx, Turn{ID: "t1"}, streamStart())
	if err != nil || out.Stop.Kind != StopUncertain {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelSettled {
			if step.Model.Interrupted ||
				step.Model.ErrorReason != llm.ReasonStreamIdleTimeout {
				t.Fatalf("provider timeout lost to later host cancel: %+v", step.Model)
			}
			return
		}
	}
	t.Fatal("missing model settlement")
}
