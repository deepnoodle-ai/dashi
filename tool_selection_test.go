package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// selectionTools returns tools named a, b, and c that count their runs.
func selectionTools(t *testing.T, ran map[string]int) tool.List {
	t.Helper()
	var list tool.List
	for _, name := range []string{"a", "b", "c"} {
		tl, err := tool.Func(name, "Tool "+name+".",
			func(context.Context, tool.Call, struct {
				Value int `json:"value"`
			}) (tool.Outcome, error) {
				ran[name]++
				return tool.Succeeded("ok"), nil
			})
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, tl)
	}
	return list
}

func TestAgentToolsSelectsByName(t *testing.T) {
	ran := map[string]int{}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "b"),
		final("done"),
	}}
	agent := testAgent()
	agent.Tools = []string{"c", "a"}
	out, err := (Runner{Client: client, Tools: selectionTools(t, ran)}).Run(
		t.Context(), Turn{}, Start{Agent: agent, Input: userInput("go")})
	if err != nil {
		t.Fatal(err)
	}
	// The toolset's order is kept, and b is not offered.
	if got := toolNames(client.inputs[0].Tools); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("offered=%v, want [a c]", got)
	}
	// A call to a tool the agent did not select does not run.
	if ran["b"] != 0 || out.Tools["call-1"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf("b ran=%d outcome=%+v", ran["b"], out.Tools["call-1"].Outcome)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
}

func TestNoToolsOffersNoApplicationTools(t *testing.T) {
	ran := map[string]int{}
	client := &scriptedClient{results: []llm.Result{final(`{"ok":true}`)}}
	agent := testAgent()
	agent.Tools = []string{"a"}
	agent.NoTools = true
	_, err := (Runner{Client: client, Tools: selectionTools(t, ran)}).Run(
		t.Context(), Turn{}, Start{
			Agent:        agent,
			OutputSchema: json.RawMessage(`{"type":"object"}`),
			Input:        userInput("go"),
		})
	if err != nil {
		t.Fatal(err)
	}
	// The output tool is not an application tool, so it stays.
	got := toolNames(client.inputs[0].Tools)
	if !slices.Equal(got, []string{SubmitOutputTool}) {
		t.Fatalf("offered=%v, want only %s", got, SubmitOutputTool)
	}
}

func TestMissingSelectedToolWarnsOncePerTurn(t *testing.T) {
	ran := map[string]int{}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "a"),
		callResult("call-2", "a"),
		final("done"),
	}}
	agent := testAgent()
	agent.Tools = []string{"a", "missing", "missing"}
	out, err := (Runner{Client: client, Tools: selectionTools(t, ran)}).Run(
		t.Context(), Turn{}, Start{Agent: agent, Input: userInput("go")})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.inputs) != 3 || ran["a"] != 2 {
		t.Fatalf("calls=%d a ran=%d", len(client.inputs), ran["a"])
	}
	var warned []Warning
	for _, w := range out.Warnings {
		if w.Code == WarningToolNotOffered {
			warned = append(warned, w)
		}
	}
	if len(warned) != 1 || !strings.Contains(warned[0].Message, `"missing"`) {
		t.Fatalf("warnings=%+v, want one tool_not_offered for missing", warned)
	}
	// The warning is recorded, so a resumed turn does not repeat it.
	if _, err := Replay(out.Turn); err != nil {
		t.Fatal(err)
	}
}

func TestToolSelectionChangesAutomaticRevision(t *testing.T) {
	revision := func(a Agent) string {
		t.Helper()
		client := &scriptedClient{results: []llm.Result{final("done")}}
		out, err := (Runner{Client: client}).Run(
			t.Context(), Turn{}, Start{Agent: a, Input: userInput("go")})
		if err != nil {
			t.Fatal(err)
		}
		return out.Agent.Revision
	}
	all := revision(Agent{Model: "fixture"})
	named := revision(Agent{Model: "fixture", Tools: []string{"a"}})
	none := revision(Agent{Model: "fixture", NoTools: true})
	if all == named || all == none || named == none {
		t.Fatalf("revisions all=%s named=%s none=%s, want all different",
			all, named, none)
	}
	if again := revision(Agent{Model: "fixture", Tools: []string{"a"}}); again != named {
		t.Fatalf("revision %s, then %s for the same selection", named, again)
	}
}

// Commands on an existing turn run with the recorded agent: a runner that
// never saw the agent settles, continues, and cancels the turn.
func TestCommandsOnExistingTurnUseRecordedAgent(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "a"),
		final("done"),
	}}
	rec := NewMemoryRecorder()
	runner := Runner{
		Client:   client,
		Tools:    selectionTools(t, map[string]int{}),
		Recorder: rec,
		Hooks:    RequireApproval(),
	}
	agent := testAgent()
	agent.Instructions = "recorded instructions"
	out, err := runner.Run(t.Context(), Turn{}, Start{
		Agent: agent,
		Input: userInput("go"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Next() != NextAnswer {
		t.Fatalf("next=%s, want answer", out.Next())
	}
	out, err = runner.Run(t.Context(), out.Turn, out.Pending[0].Approve())
	if err != nil {
		t.Fatal(err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	last := client.inputs[len(client.inputs)-1]
	if !strings.Contains(last.System, "recorded instructions") ||
		last.Model != "fixture" {
		t.Fatalf("continued call system=%q model=%q", last.System, last.Model)
	}
	if out.Agent.ID != "agent" {
		t.Fatalf("agent=%+v", out.Agent)
	}
}

// A retried Start compares its agent with the recorded one.
func TestRetriedStartWithChangedAgent(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	runner := Runner{Client: client, Recorder: rec}
	start := Start{CommandID: "s1", Agent: testAgent(), Input: userInput("go")}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, start)
	if err != nil {
		t.Fatal(err)
	}
	// The same Start returns the recorded result without a model call.
	again, err := runner.Run(t.Context(), Turn{ID: "t1"}, start)
	if err != nil || again.Revision != out.Revision || client.sends != 1 {
		t.Fatalf("retry err=%v revision=%d sends=%d", err, again.Revision, client.sends)
	}
	start.Agent.Instructions = "changed"
	_, err = runner.Run(t.Context(), Turn{ID: "t1"}, start)
	if !errors.Is(err, ErrAgentChanged) ||
		!strings.Contains(err.Error(), "instructions") {
		t.Fatalf("err=%v, want ErrAgentChanged naming instructions", err)
	}
}

func TestCurrentTurnInExecutor(t *testing.T) {
	if _, ok := CurrentTurn(t.Context()); ok {
		t.Fatal("CurrentTurn reported a turn outside the runner")
	}
	var seen TurnView
	var seenOK bool
	var toolsetView TurnView
	probe, err := tool.Func("a", "Probe.",
		func(ctx context.Context, _ tool.Call, _ struct {
			Value int `json:"value"`
		}) (tool.Outcome, error) {
			seen, seenOK = CurrentTurn(ctx)
			return tool.Succeeded("ok"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	tools := toolsetFunc(func(ctx context.Context) ([]tool.Tool, error) {
		toolsetView, _ = CurrentTurn(ctx)
		return []tool.Tool{probe}, nil
	})
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "a"),
		final("done"),
	}}
	agent := testAgent()
	_, err = (Runner{Client: client, Tools: tools}).Run(
		t.Context(), Turn{ID: "t1"}, Start{
			Agent:  agent,
			Origin: StartScheduled,
			Input:  userInput("go"),
		})
	if err != nil {
		t.Fatal(err)
	}
	if !seenOK || seen.ID != "t1" || seen.Agent.Model != "fixture" ||
		seen.Agent.ID != "agent" || seen.Origin != StartScheduled {
		t.Fatalf("CurrentTurn=%v %+v", seenOK, seen)
	}
	// The call has started, and the running turn has no stop yet.
	if seen.Tools["call-1"].Status != ToolStarted || seen.Next() != NextRunning {
		t.Fatalf("call status=%s next=%s", seen.Tools["call-1"].Status, seen.Next())
	}
	if toolsetView.ID != "t1" || toolsetView.Agent.Model != "fixture" {
		t.Fatalf("toolset view=%+v", toolsetView)
	}
}

func TestTurnViewOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin StartOrigin
		want   StartOrigin
	}{
		{"", StartUser},
		{StartUser, StartUser},
		{StartScheduled, StartScheduled},
		{StartOperator, StartOperator},
	} {
		client := &scriptedClient{results: []llm.Result{final("done")}}
		out, err := (Runner{Client: client}).Run(t.Context(), Turn{}, Start{
			Agent:  testAgent(),
			Origin: tc.origin,
			Input:  userInput("go"),
		})
		if err != nil {
			t.Fatal(err)
		}
		view, err := Replay(out.Turn)
		if err != nil {
			t.Fatal(err)
		}
		if out.Origin != tc.want || view.Origin != tc.want {
			t.Fatalf("origin %q: outcome=%q replay=%q, want %q",
				tc.origin, out.Origin, view.Origin, tc.want)
		}
	}
	if v, _ := Replay(Turn{}); v.Origin != "" {
		t.Fatalf("unstarted origin=%q", v.Origin)
	}
}

type toolsetFunc func(context.Context) ([]tool.Tool, error)

func (f toolsetFunc) Tools(ctx context.Context) ([]tool.Tool, error) {
	return f(ctx)
}
