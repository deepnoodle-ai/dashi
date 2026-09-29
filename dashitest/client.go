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
// runs without a provider or an API key. Build replies with Reply and
// CallTool. It is safe for concurrent use.
type Client struct {
	mu      sync.Mutex
	replies []llm.Result
	inputs  []llm.Input
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
	call := tool.Call{ID: id, Name: name, Args: []byte(args)}
	return llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		}),
		Stop: llm.Stop{Kind: llm.StopToolUse},
	}
}

// Add appends replies to the script.
func (c *Client) Add(replies ...llm.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies = append(c.replies, replies...)
}

// Inputs returns the input of each Send so far, in order, so a test can
// check what the model was shown, such as the history of earlier turns.
func (c *Client) Inputs() []llm.Input {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]llm.Input, len(c.inputs))
	copy(out, c.inputs)
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
func (c *Client) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	if err := ctx.Err(); err != nil {
		return llm.Result{}, err
	}
	in := call.Request.Snapshot().Input
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputs = append(c.inputs, in)
	if len(c.replies) == 0 {
		return llm.Result{}, ErrScriptDone
	}
	result := c.replies[0]
	c.replies = c.replies[1:]
	result.Requested = llm.ModelRef{Provider: Provider, Model: in.Model}
	return result, nil
}
