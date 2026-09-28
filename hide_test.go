package dashi

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// namedTool is a local tool with an object schema that succeeds.
func namedTool(name string, runs *int) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   name,
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			if runs != nil {
				*runs++
			}
			return tool.Succeeded("ok"), nil
		}),
	}
}

func hideHook(name string, tools ...string) ModelHook {
	return ModelHook{Name: name, Func: func(context.Context, ModelPlan) (ModelDecision, error) {
		return ModelDecision{Hide: tools}, nil
	}}
}

func toolNames(defs []tool.Definition) []string {
	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return names
}

func intentTools(t *testing.T, turn Turn, n int) []string {
	t.Helper()
	prefix := prefixThrough(t, turn, StepModelIntent, n)
	return toolNames(prefix.Steps[len(prefix.Steps)-1].Model.Tools)
}

func TestHideRemovesToolsFromTheCall(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	tools := tool.List{namedTool("read", nil), namedTool("write", nil), namedTool("bash", nil)}
	out, err := runHooked(t, client, tools, Hooks{BeforeModel: []ModelHook{
		hideHook("plan", "write", "not-offered"),
		hideHook("safety", "bash"),
	}})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if got := toolNames(client.inputs[0].Tools); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("offered %v", got)
	}
	if got := intentTools(t, out.Turn, 1); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("intent records %v", got)
	}
	if len(out.Warnings) != 0 {
		t.Fatalf("warnings = %+v", out.Warnings)
	}
}

func TestHideCannotRemoveTheSubmitTool(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{submitResult("out-1", `{"answer":1}`)}}
	agent := structuredAgent()
	_, out, err := runStructured(t, agent, client, hideHook("oops", SubmitOutputTool).Hooks())
	if err != nil || out.Stop.Kind != StopCompleted || !hasWarning(out, WarningHookFailed) {
		t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
	}
	if got := toolNames(client.inputs[0].Tools); !slices.Contains(got, SubmitOutputTool) {
		t.Fatalf("offered %v", got)
	}
}

func TestHideWinsOverAForcedToolChoice(t *testing.T) {
	for name, tc := range map[string]struct {
		choice llm.ToolChoice
		hide   []string
	}{
		"named tool hidden": {
			choice: llm.ToolChoice{Mode: llm.ToolChoiceTool, Name: "write"},
			hide:   []string{"write"},
		},
		"any with no tools left": {
			choice: llm.ToolChoice{Mode: llm.ToolChoiceAny},
			hide:   []string{"write", "read"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			agent := testAgent()
			agent.Settings.ToolChoice = &tc.choice
			client := &scriptedClient{results: []llm.Result{final("done")}}
			out, err := (Runner{
				Agent:  agent,
				Client: client,
				Tools:  tool.List{namedTool("read", nil), namedTool("write", nil)},
				Hooks:  hideHook("plan", tc.hide...).Hooks(),
			}).Run(t.Context(), Turn{ID: "t1"}, Start{
				CommandID: "start",
				Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
			})
			if err != nil || out.Stop.Kind != StopCompleted ||
				client.inputs[0].Settings.ToolChoice != nil ||
				!hasWarning(out, WarningToolChoiceCleared) {
				t.Fatalf("stop=%+v choice=%+v warnings=%+v err=%v", out.Stop, client.inputs[0].Settings.ToolChoice, out.Warnings, err)
			}
		})
	}
}

func TestHideHasNoEffectOnTheReservedFinalAnswer(t *testing.T) {
	agent := structuredAgent()
	agent.Limits.ModelAttempts = 2
	remote := &valueTool{}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "remote"),
		submitResult("out-1", `{"answer":1}`),
	}}
	out, err := (Runner{
		Agent:  agent,
		Client: client,
		Tools:  tool.List{remote.binding()},
		Hooks:  hideHook("hider", "remote", SubmitOutputTool).Hooks(),
	}).Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Output == nil {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	// Only the first call warns about the submit tool; the final call,
	// which offers only that tool, ignores the hide silently.
	warned := 0
	for _, w := range out.Warnings {
		if w.Code == WarningHookFailed {
			warned++
		}
	}
	if warned != 1 || !slices.Equal(toolNames(client.inputs[1].Tools), []string{SubmitOutputTool}) {
		t.Fatalf("warned=%d second call tools=%v", warned, toolNames(client.inputs[1].Tools))
	}
}

func TestCallToAHiddenToolIsUnavailable(t *testing.T) {
	gateCalls := 0
	gate := ToolHook{Name: "gate", Func: func(context.Context, ToolPlan) (ToolDecision, error) {
		gateCalls++
		return ToolDecision{}, nil
	}}
	runs := 0
	both := llm.Result{
		Message: llm.AssistantMessage(
			llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{ID: "hidden", Name: "write", Args: []byte(`{}`)}},
			llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{ID: "made-up", Name: "nope", Args: []byte(`{}`)}},
		),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
	client := &scriptedClient{results: []llm.Result{both, final("done")}}
	out, err := runHooked(
		t,
		client,
		tool.List{namedTool("read", nil), namedTool("write", &runs)},
		Hooks{
			BeforeModel: []ModelHook{hideHook("plan", "write")},
			BeforeTool:  []ToolHook{gate},
		},
	)
	if err != nil || out.Stop.Kind != StopCompleted || runs != 0 || gateCalls != 0 {
		t.Fatalf("stop=%+v runs=%d gate=%d err=%v", out.Stop, runs, gateCalls, err)
	}
	if got := toolResultText(t, out.Turn, "hidden"); got != "This tool is not available now." {
		t.Fatalf("hidden call result = %q", got)
	}
	if got := toolResultText(t, out.Turn, "made-up"); got != "Unknown tool." {
		t.Fatalf("unknown call result = %q", got)
	}
}

func TestFailingHideHookOffersTheTools(t *testing.T) {
	broken := ModelHook{Name: "plan", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
		return ModelDecision{Hide: []string{"write"}}, errors.New("flag store down")
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := runHooked(t, client, tool.List{namedTool("read", nil), namedTool("write", nil)}, broken.Hooks())
	if err != nil || !hasWarning(out, WarningHookFailed) ||
		!slices.Equal(toolNames(client.inputs[0].Tools), []string{"read", "write"}) {
		t.Fatalf("offered=%v warnings=%+v err=%v", toolNames(client.inputs[0].Tools), out.Warnings, err)
	}
}
