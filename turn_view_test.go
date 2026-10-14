package dashi

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func messageTexts(messages []llm.Message) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
		out[i] = m.Text()
	}
	return out
}

// Input holds the start's input and later user items, steers and
// AddMessages alike, but not items a hook added, and a hook and a tool
// both read it.
func TestTurnViewInput(t *testing.T) {
	inbox := NewMemoryInbox()
	mustSend(t, inbox, "use staging")
	var hookSaw, toolSaw [][]string
	note := ModelHook{Name: "note", Func: func(
		_ context.Context,
		p ModelPlan,
	) (ModelDecision, error) {
		hookSaw = append(hookSaw, messageTexts(p.Turn.Input))
		// A hook's item is not input, whatever its origin.
		return ModelDecision{AddMessages: []ContextItem{{
			Message: llm.UserMessage(llm.Text("hook note")),
		}}}, nil
	}}
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(ctx context.Context, _ tool.Call) (tool.Outcome, error) {
			view, ok := CurrentTurn(ctx)
			if !ok {
				t.Error("the tool cannot reach the turn")
			}
			toolSaw = append(toolSaw, messageTexts(view.Input))
			return tool.Succeeded("found"), nil
		}),
	}
	wait := tool.Tool{Definition: tool.Definition{
		Name:   "wait",
		Schema: json.RawMessage(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	client := &scriptedClient{results: []llm.Result{
		callResult("c1", "lookup"),
		callResult("c2", "wait"),
		final("done"),
	}}
	runner := Runner{Client: client, Tools: tool.List{lookup, wait}, Hooks: note.Hooks()}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, startInput(), WithInbox(inbox))
	if err != nil || out.Stop.Kind != StopWaiting {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	out, err = runner.Run(t.Context(), out.Turn, AddMessages{
		CommandID: "add",
		Items: []ContextItem{
			{Origin: ContextUser, Message: llm.UserMessage(llm.Text("and the README"))},
			{Origin: ContextRuntime, Message: llm.UserMessage(llm.Text("cwd: /src"))},
			{Name: "plan", Message: llm.UserMessage(llm.Text("step 1"))},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	early := []string{"do it", "use staging"}
	if len(hookSaw) == 0 || !slices.Equal(hookSaw[0], early) {
		t.Fatalf("the hook saw %q", hookSaw)
	}
	if len(toolSaw) != 1 || !slices.Equal(toolSaw[0], early) {
		t.Fatalf("the tool saw %q", toolSaw)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"do it", "use staging", "and the README", "step 1"}
	if got := messageTexts(view.Input); !slices.Equal(got, want) {
		t.Fatalf("input = %q; want %q", got, want)
	}
	if c := view.Input[3].Context; c == nil || c.Name != "plan" ||
		c.Origin != string(ContextUser) {
		t.Fatalf("named item context = %+v", c)
	}
	if view.Input[2].Context != nil {
		t.Fatalf("unnamed item context = %+v", view.Input[2].Context)
	}
}

// A view's Input is a copy: changing it changes neither the record nor
// another view.
func TestTurnViewInputIsACopy(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := (Runner{Client: client}).Run(t.Context(), Turn{ID: "t1"}, startInput())
	if err != nil {
		t.Fatal(err)
	}
	first, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	first.Input[0].Content[0].Block.Text.Text = "changed"
	out.TurnView.Input[0].Content[0].Block.Text.Text = "changed too"
	second, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Input[0].Text(); got != "do it" {
		t.Fatalf("input = %q", got)
	}
	if got := out.Turn.Steps[0].Started.Input[0].Text(); got != "do it" {
		t.Fatalf("recorded input = %q", got)
	}
}

// Views, events, and hook plans carry snake_case JSON keys that match their
// field names, like the step facts they are built from.
func TestViewJSONTags(t *testing.T) {
	for _, v := range []any{
		TurnView{}, ToolView{}, RunResult{}, Event{}, ToolProgress{},
		ModelPlan{}, ModelDecision{}, SendPlan{}, SendDecision{},
		ToolPlan{}, ToolDecision{}, ResultPlan{}, ResultDecision{},
		FinishPlan{}, FinishDecision{},
	} {
		typ := reflect.TypeOf(v)
		for f := range typ.Fields() {
			if !f.IsExported() || f.Anonymous {
				continue
			}
			tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if tag == "" {
				t.Errorf("%s.%s has no json tag", typ.Name(), f.Name)
			}
		}
	}

	view := TurnView{ID: "t1", ModelCalls: 2, ReadyToContinue: true,
		Tools: map[string]ToolView{"c1": {Status: ToolStarted}}}
	raw, err := json.Marshal(RunResult{Turn: Turn{ID: "t1"}, TurnView: view})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"turn":`, `"model_calls":2`, `"ready_to_continue":true`,
		`"status":"started"`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("RunResult JSON lacks %s: %s", key, raw)
		}
	}
	var decoded RunResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ModelCalls != 2 || !decoded.ReadyToContinue ||
		decoded.Tools["c1"].Status != ToolStarted {
		t.Fatalf("decoded %+v", decoded.TurnView)
	}
}
