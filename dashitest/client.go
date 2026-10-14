package dashitest

import (
	"context"
	"errors"
	"sync"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// The scripted client's destination. The catalog knows no default model
// for it, so an agent run on a Client names Model.
const (
	Provider = "dashitest"
	API      = "dashitest.chat"
	Model    = "scripted"
)

// ErrScriptDone is returned by a Client's Send when no reply is left.
var ErrScriptDone = errors.New("dashitest: no scripted reply left")

// Client is a scripted llm.Client for tests and keyless examples: each Send
// returns the next reply in order, whatever the request says, so a turn
// runs without a provider or an API key. Build replies with Reply,
// CallTool, and CallTools. When the input asks to stream, as a runner with
// an observer does, Send previews each reply as deltas before returning it,
// so live views can be tested. It is safe for concurrent use.
type Client struct {
	mu       sync.Mutex
	replies  []llm.Result
	requests []llm.Request
	release  <-chan struct{}
}

// NewClient returns a client that answers with replies, in order.
func NewClient(replies ...llm.Result) *Client {
	return &Client{replies: replies}
}

// Reply is a model reply that answers with text and finishes.
func Reply(text string) llm.Result {
	return llm.Result{
		Message: llm.AssistantMessage(llm.Text(text)),
		Stop:    llm.Stop{Kind: llm.StopFinished},
	}
}

// CallTool is a model reply that calls one tool with JSON arguments.
func CallTool(id, name, args string) llm.Result {
	return CallTools(tool.Call{ID: id, Name: name, Args: []byte(args)})
}

// CallTools is a model reply that calls several tools at once, in order, as
// a model making parallel tool calls does.
func CallTools(calls ...tool.Call) llm.Result {
	parts := make([]llm.Content, len(calls))
	for i, call := range calls {
		call := call.Clone()
		parts[i] = llm.Content{Kind: llm.ContentToolCall, ToolCall: &call}
	}
	return llm.Result{
		Message: llm.AssistantMessage(parts...),
		Stop:    llm.Stop{Kind: llm.StopToolUse},
	}
}

// Add appends replies to the script.
func (c *Client) Add(replies ...llm.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies = append(c.replies, replies...)
}

// Hold makes each Send wait until release is closed, as a slow model would,
// so a test or example decides when replies arrive. A canceled context ends
// the wait with its error.
func (c *Client) Hold(release <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.release = release
}

// Inputs returns the input of each Send so far, in order, so a test can
// check what the model was shown, such as the history of earlier turns.
// Each call returns new copies, so editing them changes nothing recorded.
func (c *Client) Inputs() []llm.Input {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]llm.Input, len(c.requests))
	for i, req := range c.requests {
		out[i] = req.Input()
	}
	return out
}

// Provider returns the scripted provider name.
func (*Client) Provider() string {
	return Provider
}

// API returns the scripted API name.
func (*Client) API() string {
	return API
}

// ModelInfo knows no models, so spend is counted in tokens without a price.
func (*Client) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

// Prepare freezes the input as the request.
func (*Client) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: Provider,
			API:      API,
			Model:    in.Model,
			Scope:    "dashitest",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{}`),
	})
}

// Send returns the next scripted reply, or ErrScriptDone when none is left.
// After Hold, it first waits for the release. When the input asks to
// stream, Send first reports the reply to call.Sink: one delta for each
// text, reasoning, and tool call part, then one for usage when the reply
// has it. The returned result is the same either way.
func (c *Client) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	c.mu.Lock()
	release := c.release
	c.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	if err := ctx.Err(); err != nil {
		return llm.Result{}, err
	}
	in := call.Request.Input()
	c.mu.Lock()
	c.requests = append(c.requests, call.Request)
	if len(c.replies) == 0 {
		c.mu.Unlock()
		return llm.Result{}, ErrScriptDone
	}
	result := c.replies[0]
	c.replies = c.replies[1:]
	c.mu.Unlock()
	result.Requested = llm.ModelRef{Provider: Provider, Model: in.Model}
	if in.Stream && call.Sink != nil {
		preview(call.Sink, result)
	}
	return result, nil
}

// preview reports a reply as the deltas a streaming provider would send
// before its final result: each part whole, at its position in the message.
func preview(sink llm.Sink, result llm.Result) {
	for i, part := range result.Message.Content {
		switch {
		case part.Block != nil && part.Block.Text != nil:
			sink(llm.Delta{
				Kind:  llm.DeltaText,
				Index: i,
				Text:  part.Block.Text.Text,
			})
		case part.Reasoning != nil:
			sink(llm.Delta{
				Kind:  llm.DeltaReasoning,
				Index: i,
				Text:  part.Reasoning.Text,
			})
		case part.ToolCall != nil:
			sink(llm.Delta{
				Kind:  llm.DeltaToolArguments,
				Index: i,
				Text:  string(part.ToolCall.Args),
			})
		}
	}
	if result.Usage != nil {
		sink(llm.Delta{Kind: llm.DeltaUsage, Usage: result.Usage.Copy()})
	}
}
