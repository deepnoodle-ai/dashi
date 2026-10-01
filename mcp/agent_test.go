package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// scripted is a model that calls one tool, then answers.
type scripted struct {
	toolName string
	args     string
	inputs   []llm.Input
	sends    int
}

func (*scripted) Provider() string { return "test" }

func (*scripted) API() string { return "test.chat" }

func (*scripted) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (c *scripted) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	c.inputs = append(c.inputs, in)
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{}`),
	})
}

func (c *scripted) Send(_ context.Context, call llm.Call) (llm.Result, error) {
	requested := llm.ModelRef{Provider: "test", Model: "fixture"}
	if c.sends++; c.sends == 1 {
		tc := tool.Call{ID: "call-1", Name: c.toolName, Args: json.RawMessage(c.args)}
		return llm.Result{
			Message: llm.AssistantMessage(llm.Content{
				Kind:     llm.ContentToolCall,
				ToolCall: &tc,
			}),
			Stop:      llm.Stop{Kind: llm.StopToolUse},
			Requested: requested,
		}, nil
	}
	return llm.Result{
		Message:   llm.AssistantMessage(llm.Text("finished")),
		Stop:      llm.Stop{Kind: llm.StopFinished},
		Requested: requested,
	}, nil
}

// lastToolResult returns the text of the tool result in the model's last
// request.
func lastToolResult(t *testing.T, c *scripted) (string, bool) {
	t.Helper()
	if len(c.inputs) < 2 {
		t.Fatalf("the model made %d requests, want 2", len(c.inputs))
	}
	for _, m := range c.inputs[1].Messages {
		for _, part := range m.Content {
			if part.ToolResult == nil {
				continue
			}
			var sb strings.Builder
			for _, b := range part.ToolResult.Output.Content {
				if b.Text != nil {
					sb.WriteString(b.Text.Text)
				}
			}
			return sb.String(), part.ToolResult.IsError
		}
	}
	t.Fatal("no tool result in the second request")
	return "", false
}

func transports(t *testing.T) map[string]Server {
	t.Helper()
	stdio, _ := stdioServer(t)
	web, _ := httpServer(t, nil)
	return map[string]Server{"stdio": stdio, "http": web}
}

func TestScriptedModelCallsAnMCPTool(t *testing.T) {
	for name, srv := range transports(t) {
		t.Run(name, func(t *testing.T) {
			ts, err := Open(t.Context(), Config{
				Servers: map[string]Server{"test": srv},
			}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer ts.Close()
			model := &scripted{toolName: "mcp__test__echo", args: `{"text":"from mcp"}`}
			out, err := runAgent(t, model, ts, dashi.Hooks{})
			if err != nil {
				t.Fatal(err)
			}
			if out.Stop.Kind != dashi.StopCompleted {
				t.Fatalf("stop = %+v", out.Stop)
			}
			got, isErr := lastToolResult(t, model)
			if got != "from mcp" || isErr {
				t.Fatalf("model saw %q (error %v)", got, isErr)
			}
		})
	}
}

func TestBeforeToolHookBlocksAnMCPCall(t *testing.T) {
	srv, log := stdioServer(t)
	ts, err := Open(t.Context(), Config{
		Servers: map[string]Server{"test": srv},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	deny := dashi.ToolHook{Name: "deny-mcp", Func: func(
		_ context.Context,
		p dashi.ToolPlan,
	) (dashi.ToolDecision, error) {
		if info, ok := ts.Info(p.Call.Name); ok {
			return dashi.Deny("server " + info.Server + " is off limits"), nil
		}
		return dashi.ToolDecision{}, nil
	}}
	model := &scripted{toolName: "mcp__test__echo", args: `{"text":"x"}`}
	if _, err := runAgent(t, model, ts, deny.Hooks()); err != nil {
		t.Fatal(err)
	}
	got, _ := lastToolResult(t, model)
	if !strings.Contains(got, "off limits") {
		t.Fatalf("model saw %q", got)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "call echo") {
		t.Fatalf("the denied call reached the server: %q", data)
	}
}

func TestAgentRunsWhenOneServerFails(t *testing.T) {
	good, _ := stdioServer(t)
	ts, err := Open(t.Context(), Config{
		Servers: map[string]Server{
			"test": good,
			"dead": {Command: "/no/such/program-xyz"},
		},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	model := &scripted{toolName: "mcp__test__add", args: `{"a":1,"b":2}`}
	out, err := runAgent(t, model, ts, dashi.Hooks{})
	if err != nil || out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("err = %v, stop = %+v", err, out.Stop)
	}
	if got, _ := lastToolResult(t, model); !strings.Contains(got, `"sum":3`) {
		t.Fatalf("model saw %q", got)
	}
	for _, s := range ts.Status() {
		if s.Name == "dead" && (s.State != StateFailed || s.Err == nil) {
			t.Fatalf("dead status = %+v", s)
		}
	}
}

func runAgent(
	t *testing.T,
	model *scripted,
	ts *Toolset,
	hooks dashi.Hooks,
) (dashi.RunResult, error) {
	t.Helper()
	return dashi.Runner{
		Client: model,
		Tools:  tool.Join(ts),
		Hooks:  hooks,
	}.Run(t.Context(), dashi.NewTurn("t1"), dashi.Start{
		Agent: dashi.Agent{Model: "fixture"},
		Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
	})
}
