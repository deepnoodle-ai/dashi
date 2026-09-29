package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

// fakeClient is a priced model client whose reply depends on the request.
type fakeClient struct {
	window int
	reply  func(llm.Input) llm.Result
	inputs []llm.Input
}

func (*fakeClient) Provider() string {
	return "test"
}

func (*fakeClient) API() string {
	return "test.chat"
}

func (c *fakeClient) ModelInfo(string) (llm.ModelInfo, bool) {
	info := llm.ModelInfo{}
	info.Capabilities.ContextWindow = c.window
	return info, true
}

func (c *fakeClient) Prepare(
	_ context.Context,
	in llm.Input,
) (llm.Request, error) {
	c.inputs = append(c.inputs, in)
	pricing := llm.Pricing{
		Currency:  "USD",
		UpdatedAt: "2026-09-28",
		Base: llm.Rates{
			Input:      money.MustParseAmount("1"),
			Output:     money.MustParseAmount("2"),
			CacheRead:  money.MustParseAmount("1"),
			CacheWrite: money.MustParseAmount("1"),
		},
	}
	info := llm.ModelInfo{
		Pricing: &pricing,
		Evidence: llm.ModelEvidence{
			Model:       in.Model,
			API:         "test.chat",
			Endpoint:    "https://example.test/model",
			Owner:       "test",
			Source:      "fixture",
			Method:      llm.EvidenceFixture,
			QualifiedAt: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC),
		},
	}
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:            in,
		Effective:        in.Settings,
		Body:             []byte(`{"ok":true}`),
		ModelInfo:        &info,
		CapabilityPolicy: llm.CapabilityAdjust,
	})
}

func (c *fakeClient) Send(
	_ context.Context,
	_ llm.Call,
) (llm.Result, error) {
	r := c.reply(c.inputs[len(c.inputs)-1])
	r.Requested = llm.ModelRef{Provider: "test", Model: "fixture"}
	return r, nil
}

func textResult(text string) llm.Result {
	return llm.Result{
		Message: llm.AssistantMessage(llm.Text(text)),
		Stop:    llm.Stop{Kind: llm.StopFinished},
		Usage:   &llm.Usage{InputTokens: 1_000_000, OutputTokens: 500_000},
	}
}

// fakeSummarizer records its input and returns fixed text.
type fakeSummarizer struct {
	text  string
	err   error
	calls int
	input []llm.Message
}

func (f *fakeSummarizer) Summarize(
	_ context.Context,
	messages []llm.Message,
) (Summary, error) {
	f.calls++
	f.input = messages
	if f.err != nil {
		return Summary{}, f.err
	}
	return Summary{
		Text:  f.text,
		Model: "summary-model",
		Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		Cost:  map[string]money.Amount{"USD": money.MustParseAmount("0.02")},
	}, nil
}

// exchange is one Turn of history: a user prompt, an assistant message with
// parallel tool calls, the results, and a final answer. Each text is padded
// to size bytes.
func exchange(n, size int) []llm.Message {
	pad := strings.Repeat("x", size)
	id := func(k int) string { return fmt.Sprintf("call-%d-%d", n, k) }
	call := func(k int) llm.Content {
		c := tool.Call{
			ID:   id(k),
			Name: "read",
			Args: json.RawMessage(`{"path":"a.go"}`),
		}
		return llm.Content{Kind: llm.ContentToolCall, ToolCall: &c}
	}
	result := func(k int) llm.Content {
		return llm.Content{
			Kind: llm.ContentToolResult,
			ToolResult: &llm.ToolResult{
				CallID: id(k),
				Output: tool.TextOutput(pad),
			},
		}
	}
	return []llm.Message{
		llm.UserMessage(llm.Text(fmt.Sprintf("prompt %d %s", n, pad))),
		llm.AssistantMessage(call(1), call(2)),
		llm.UserMessage(result(1), result(2)),
		llm.AssistantMessage(llm.Text(fmt.Sprintf("answer %d", n))),
	}
}

// session builds a history of turns Turns and the index where each starts.
func session(turns, size int) ([]llm.Message, []int) {
	var history []llm.Message
	var starts []int
	for n := 0; n < turns; n++ {
		starts = append(starts, len(history))
		history = append(history, exchange(n, size)...)
	}
	return history, starts
}

// bigModel is a policy whose window is large enough for the given tokens.
func policyWith(window int) Policy {
	p := Policy{}
	p.Model.Capabilities.ContextWindow = window
	return p
}

// validPairs fails the test if a tool call lacks its result or a result lacks
// its call.
func validPairs(t *testing.T, messages []llm.Message) {
	t.Helper()
	calls := map[string]bool{}
	results := map[string]bool{}
	for _, m := range messages {
		for _, c := range m.Content {
			if c.ToolCall != nil {
				calls[c.ToolCall.ID] = true
			}
			if c.ToolResult != nil {
				if !calls[c.ToolResult.CallID] {
					t.Fatalf("orphaned result %q", c.ToolResult.CallID)
				}
				results[c.ToolResult.CallID] = true
			}
		}
	}
	for id := range calls {
		if !results[id] {
			t.Fatalf("orphaned call %q", id)
		}
	}
}

func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}

var errBoom = errors.New("boom")
