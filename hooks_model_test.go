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

// intentOf returns the n-th recorded model intent.
func intentOf(t *testing.T, turn Turn, n int) *ModelFact {
	t.Helper()
	prefix := prefixThrough(t, turn, StepModelCallStarted, n)
	return prefix.Steps[len(prefix.Steps)-1].Model
}

func countWarnings(out Outcome, code WarningCode) int {
	n := 0
	for _, w := range out.Warnings {
		if w.Code == code {
			n++
		}
	}
	return n
}

func runAgent(
	t *testing.T,
	agent Agent,
	client *scriptedClient,
	tools tool.Toolset,
	hooks Hooks,
) Outcome {
	t.Helper()
	out, err := (Runner{Client: client, Tools: tools, Hooks: hooks}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     agent,
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHideRemovesToolsFromTheCall(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	tools := tool.List{namedTool("read", nil), namedTool("write", nil), namedTool("bash", nil)}
	out := runAgent(t, testAgent(), client, tools, Hooks{BeforeModel: []ModelHook{
		hideHook("plan", "write"),
		hideHook("safety", "bash", "write"),
	}})
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
	if got := toolNames(client.inputs[0].Tools); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("offered %v", got)
	}
	intent := intentOf(t, out.Turn, 1)
	if got := toolNames(intent.Tools); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("intent tools = %v", got)
	}
	if !slices.Equal(intent.Hidden, []string{"write", "bash"}) {
		t.Fatalf("intent hidden = %v", intent.Hidden)
	}
	if len(out.Warnings) != 0 {
		t.Fatalf("warnings = %+v", out.Warnings)
	}
}

func TestHideWarnsOnceAboutNamesItCannotApply(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{submitResult("out-1", `{"answer":1}`)}}
	_, out, err := runStructured(t, structuredAgent(), client, Hooks{BeforeModel: []ModelHook{
		hideHook("typo", "write-file", SubmitOutputTool),
		hideHook("typo2", "write-file"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
	if countWarnings(out, WarningHideIgnored) != 1 {
		t.Fatalf("warnings = %+v", out.Warnings)
	}
	if countWarnings(out, WarningHookFailed) != 0 {
		t.Fatalf("a hide name counted as a hook failure: %+v", out.Warnings)
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
			tools := tool.List{namedTool("read", nil), namedTool("write", nil)}
			out := runAgent(t, agent, client, tools, hideHook("plan", tc.hide...).Hooks())
			if out.Stop.Kind != StopCompleted {
				t.Fatalf("stop = %+v", out.Stop)
			}
			if choice := client.inputs[0].Settings.ToolChoice; choice != nil {
				t.Fatalf("choice = %+v", choice)
			}
			if countWarnings(out, WarningToolChoiceCleared) != 1 {
				t.Fatalf("warnings = %+v", out.Warnings)
			}
		})
	}
}

func TestHideAndAForcingHookInEitherOrder(t *testing.T) {
	hider := hideHook("plan", "write")
	forcer := ModelHook{Name: "forcer", Func: func(
		_ context.Context,
		p ModelPlan,
	) (ModelDecision, error) {
		s := p.Input.Settings
		s.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceTool, Name: "write"}
		return ModelDecision{Settings: &s, Hide: []string{"bash"}}, nil
	}}
	for name, hooks := range map[string][]ModelHook{
		"hide first":  {hider, forcer},
		"force first": {forcer, hider},
	} {
		t.Run(name, func(t *testing.T) {
			client := &scriptedClient{results: []llm.Result{final("done")}}
			tools := tool.List{namedTool("read", nil), namedTool("write", nil), namedTool("bash", nil)}
			out := runAgent(t, testAgent(), client, tools, Hooks{BeforeModel: hooks})
			if got := toolNames(client.inputs[0].Tools); !slices.Equal(got, []string{"read"}) {
				t.Fatalf("offered %v", got)
			}
			if choice := client.inputs[0].Settings.ToolChoice; choice != nil {
				t.Fatalf("choice = %+v", choice)
			}
			if countWarnings(out, WarningToolChoiceCleared) != 1 {
				t.Fatalf("warnings = %+v", out.Warnings)
			}
			if countWarnings(out, WarningHookFailed) != 0 {
				t.Fatalf("a hook was skipped: %+v", out.Warnings)
			}
		})
	}
}

func TestHidingEveryToolClearsToolSettings(t *testing.T) {
	agent := testAgent()
	agent.Settings.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	agent.Settings.ParallelTools = new(true)
	client := &scriptedClient{results: []llm.Result{final("done")}}
	tools := tool.List{namedTool("read", nil), namedTool("write", nil)}
	out := runAgent(t, agent, client, tools, hideHook("all", "read", "write").Hooks())
	sent := client.inputs[0]
	if len(sent.Tools) != 0 {
		t.Fatalf("offered %v", toolNames(sent.Tools))
	}
	if sent.Settings.ToolChoice != nil {
		t.Fatalf("choice = %+v", sent.Settings.ToolChoice)
	}
	if sent.Settings.ParallelTools != nil {
		t.Fatalf("parallel tools = %v", *sent.Settings.ParallelTools)
	}
	if countWarnings(out, WarningToolChoiceCleared) != 0 {
		t.Fatalf("an auto choice warned: %+v", out.Warnings)
	}
}

func TestInvalidHideAnswerIsSkippedWhole(t *testing.T) {
	// The hook forces a tool the call never offered: its answer is
	// invalid, so its hide does not apply either.
	bad := ModelHook{Name: "bad", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
		s := p.Input.Settings
		s.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceTool, Name: "missing"}
		return ModelDecision{Settings: &s, Hide: []string{"bash"}}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	tools := tool.List{namedTool("read", nil), namedTool("bash", nil)}
	out := runAgent(t, testAgent(), client, tools, bad.Hooks())
	if countWarnings(out, WarningHookFailed) != 1 {
		t.Fatalf("warnings = %+v", out.Warnings)
	}
	if got := toolNames(client.inputs[0].Tools); !slices.Equal(got, []string{"read", "bash"}) {
		t.Fatalf("offered %v", got)
	}
}

func TestHideWithDeliverAppliesBoth(t *testing.T) {
	both := ModelHook{Name: "plan", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
		return ModelDecision{
			Hide: []string{"write", "nope"},
			AddMessages: []ContextItem{{
				Origin:  ContextRuntime,
				Message: llm.UserMessage(llm.Text("plan mode is on")),
			}},
		}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	tools := tool.List{namedTool("read", nil), namedTool("write", nil)}
	out := runAgent(t, testAgent(), client, tools, both.Hooks())
	if got := toolNames(client.inputs[0].Tools); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("offered %v", got)
	}
	sent := client.inputs[0].Messages
	if sent[len(sent)-1].Text() != "plan mode is on" {
		t.Fatalf("delivery missing: %+v", sent[len(sent)-1])
	}
	delivered := false
	for _, step := range out.Turn.Steps {
		if step.Kind == StepMessagesAdded {
			delivered = true
			if len(step.Warnings) != 1 || step.Warnings[0].Code != WarningHideIgnored {
				t.Fatalf("delivered step warnings = %+v", step.Warnings)
			}
		}
	}
	if !delivered {
		t.Fatal("no delivered step")
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
	out := runAgent(t, agent, client, tool.List{remote.binding()},
		hideHook("hider", "remote", SubmitOutputTool).Hooks())
	if out.Output == nil {
		t.Fatalf("stop = %+v", out.Stop)
	}
	// Only the first call warns about the submit tool; the final call,
	// which offers only that tool, ignores the hide silently.
	if countWarnings(out, WarningHideIgnored) != 1 {
		t.Fatalf("warnings = %+v", out.Warnings)
	}
	if got := toolNames(client.inputs[1].Tools); !slices.Equal(got, []string{SubmitOutputTool}) {
		t.Fatalf("final call tools = %v", got)
	}
	if len(intentOf(t, out.Turn, 2).Hidden) != 0 {
		t.Fatalf("final call recorded hides")
	}
	// Replay rejects a record that claims hides on the final call.
	turn := cloneTurn(out.Turn)
	for i, step := range turn.Steps {
		if step.Kind == StepModelCallStarted && step.Model.FinalReplyOnly {
			turn.Steps[i].Model.Hidden = []string{"remote"}
		}
	}
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay err = %v", err)
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
	out := runAgent(
		t,
		testAgent(),
		client,
		tool.List{namedTool("read", nil), namedTool("write", &runs)},
		Hooks{
			BeforeModel: []ModelHook{hideHook("plan", "write")},
			BeforeTool:  []ToolHook{gate},
		},
	)
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop = %+v", out.Stop)
	}
	if runs != 0 || gateCalls != 0 {
		t.Fatalf("runs=%d gate=%d", runs, gateCalls)
	}
	if got := toolResultText(t, out.Turn, "hidden"); got != "This tool is not available now." {
		t.Fatalf("hidden call result = %q", got)
	}
	if got := toolResultText(t, out.Turn, "made-up"); got != "Unknown tool." {
		t.Fatalf("unknown call result = %q", got)
	}
}

func TestHiddenCallResolvesFromTheRecordAfterACrash(t *testing.T) {
	hiding := 0
	hider := ModelHook{Name: "plan", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
		hiding++
		return ModelDecision{Hide: []string{"write"}}, nil
	}}
	call := llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &tool.Call{ID: "hidden", Name: "write", Args: []byte(`{}`)},
		}),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
	client := &scriptedClient{results: []llm.Result{call, final("done")}}
	base := NewMemoryRecorder()
	tools := tool.List{namedTool("read", nil), namedTool("write", nil)}
	_, err := (Runner{
		Client:   client,
		Tools:    tools,
		Recorder: &failNthKind{base: base, kind: StepToolFinished, n: 1},
		Hooks:    hider.Hooks(),
	}).Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     testAgent(),
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if persistence(err) != PersistenceRejected {
		t.Fatalf("err = %v", err)
	}
	saved, err := base.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	// The resuming worker's hook hides nothing and its toolset no longer
	// has write, which alone would read "Unknown tool.": the call still
	// resolves from the recorded hides, and its hook runs only for the next
	// call.
	resumed := 0
	counter := ModelHook{Name: "plan", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
		resumed++
		return ModelDecision{}, nil
	}}
	out, err := (Runner{
		Client:   client,
		Tools:    tool.List{namedTool("read", nil)},
		Recorder: base,
		Hooks:    counter.Hooks(),
	}).Run(t.Context(), saved, Continue{CommandID: "recover"})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if got := toolResultText(t, out.Turn, "hidden"); got != "This tool is not available now." {
		t.Fatalf("hidden call result = %q", got)
	}
	if hiding != 1 || resumed != 1 {
		t.Fatalf("hooks ran %d and %d times", hiding, resumed)
	}
}

func TestFailingHideHookOffersTheTools(t *testing.T) {
	broken := ModelHook{Name: "plan", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
		return ModelDecision{Hide: []string{"write"}}, errors.New("flag store down")
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out := runAgent(t, testAgent(), client,
		tool.List{namedTool("read", nil), namedTool("write", nil)}, broken.Hooks())
	if !hasWarning(out, WarningHookFailed) {
		t.Fatalf("warnings = %+v", out.Warnings)
	}
	if got := toolNames(client.inputs[0].Tools); !slices.Equal(got, []string{"read", "write"}) {
		t.Fatalf("offered %v", got)
	}
}

func TestReplayRejectsInvalidHiddenTools(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out := runAgent(t, testAgent(), client, tool.List{namedTool("read", nil)}, Hooks{})
	for name, hidden := range map[string][]string{
		"offered":   {"read"},
		"submit":    {SubmitOutputTool},
		"duplicate": {"x", "x"},
		"empty":     {""},
	} {
		t.Run(name, func(t *testing.T) {
			turn := cloneTurn(out.Turn)
			for i, step := range turn.Steps {
				if step.Kind == StepModelCallStarted {
					turn.Steps[i].Model.Hidden = hidden
				}
			}
			if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
				t.Fatalf("replay err = %v", err)
			}
		})
	}
}
