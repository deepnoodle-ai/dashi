package dashitest_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func send(ctx context.Context, c *dashitest.Client) (llm.Result, error) {
	return llm.Generate(ctx, c, llm.Input{
		Model:    dashitest.Model,
		Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	}, nil)
}

func TestClientHoldWaitsForRelease(t *testing.T) {
	c := dashitest.NewClient(dashitest.Reply("done"))
	release := make(chan struct{})
	c.Hold(release)
	got := make(chan string, 1)
	go func() {
		result, err := send(t.Context(), c)
		if err != nil {
			got <- err.Error()
			return
		}
		got <- result.Message.Text()
	}()
	select {
	case text := <-got:
		t.Fatalf("Send answered %q before release", text)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if text := <-got; text != "done" {
		t.Fatalf("reply = %q, want done", text)
	}
}

func TestClientHoldEndsWithContext(t *testing.T) {
	c := dashitest.NewClient(dashitest.Reply("done"))
	c.Hold(make(chan struct{}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := send(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send error = %v, want context.Canceled", err)
	}
	if n := len(c.Inputs()); n != 0 {
		t.Fatalf("canceled Send recorded %d inputs", n)
	}
}

func TestClientInputsAreCopies(t *testing.T) {
	c := dashitest.NewClient(dashitest.Reply("done"))
	if _, err := send(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	first := c.Inputs()
	first[0].Messages[0].Content[0].Block.Text.Text = "edited"
	first[0].Messages = append(first[0].Messages, llm.UserMessage(llm.Text("x")))
	got := c.Inputs()[0]
	if len(got.Messages) != 1 || got.Messages[0].Text() != "hi" {
		t.Fatalf("recorded input changed: %+v", got.Messages)
	}
}

func TestCallToolsCallsEachTool(t *testing.T) {
	result := dashitest.CallTools(
		tool.Call{ID: "c1", Name: "read", Args: []byte(`{"path":"a"}`)},
		tool.Call{ID: "c2", Name: "read", Args: []byte(`{"path":"b"}`)},
	)
	if result.Stop.Kind != llm.StopToolUse {
		t.Fatalf("stop = %q, want tool use", result.Stop.Kind)
	}
	var ids []string
	for _, part := range result.Message.Content {
		ids = append(ids, part.ToolCall.ID)
	}
	if !reflect.DeepEqual(ids, []string{"c1", "c2"}) {
		t.Fatalf("calls = %v, want [c1 c2]", ids)
	}
}

func TestClientStreamsReplyAsDeltas(t *testing.T) {
	reply := llm.Result{
		Message: llm.AssistantMessage(
			llm.Content{Kind: llm.ContentReasoning, Reasoning: &llm.Reasoning{Text: "hmm"}},
			llm.Text("calling"),
			dashitest.CallTool("c1", "read", `{"path":"a"}`).Message.Content[0],
		),
		Stop:  llm.Stop{Kind: llm.StopToolUse},
		Usage: &llm.Usage{InputTokens: 3, OutputTokens: 4},
	}
	in := llm.Input{
		Model:    dashitest.Model,
		Stream:   true,
		Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	}
	streamed := dashitest.NewClient(reply)
	req, err := streamed.Prepare(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []llm.Delta
	var final llm.Result
	for event, err := range llm.Stream(t.Context(), streamed, llm.Call{Request: req}) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Delta != nil {
			deltas = append(deltas, *event.Delta)
		}
		if event.Result != nil {
			final = *event.Result
		}
	}
	want := []llm.Delta{
		{Kind: llm.DeltaReasoning, Index: 0, Text: "hmm"},
		{Kind: llm.DeltaText, Index: 1, Text: "calling"},
		{Kind: llm.DeltaToolArguments, Index: 2, Text: `{"path":"a"}`},
		{Kind: llm.DeltaUsage, Usage: &llm.Usage{InputTokens: 3, OutputTokens: 4}},
	}
	if !reflect.DeepEqual(deltas, want) {
		t.Fatalf("deltas = %+v\nwant %+v", deltas, want)
	}

	in.Stream = false
	plain, err := llm.Generate(t.Context(), dashitest.NewClient(reply), in, func(d llm.Delta) {
		t.Errorf("unrequested delta %+v", d)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(final, plain) {
		t.Fatalf("streamed result differs:\n%+v\n%+v", final, plain)
	}
}

func TestRunnerStreamShowsScriptedText(t *testing.T) {
	runner := dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("hello there"))}
	start := dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	}
	var text string
	for event, err := range runner.Stream(t.Context(), dashi.Turn{}, start) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Delta != nil && event.Delta.Kind == llm.DeltaText {
			text += event.Delta.Text
		}
	}
	if text != "hello there" {
		t.Fatalf("streamed text = %q, want %q", text, "hello there")
	}
}
