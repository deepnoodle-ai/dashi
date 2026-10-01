package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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
}

// The tool_not_offered warning is recorded, so a turn that is interrupted and
// continued does not repeat it.
// A named tool warns even when the runner has no toolset, and the warning
// is in the record.
func TestMissingSelectedToolWarnsWithoutToolset(t *testing.T) {
	rec := NewMemoryRecorder()
	agent := testAgent()
	agent.Tools = []string{"lookup"}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	_, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(), Turn{ID: "t1"}, Start{Agent: agent, Input: userInput("go")})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(client.inputs[0].Tools); n != 0 {
		t.Fatalf("offered %d tools, want none", n)
	}
	turn, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	var warned []Warning
	for _, w := range view.Warnings {
		if w.Code == WarningToolNotOffered {
			warned = append(warned, w)
		}
	}
	if len(warned) != 1 || !strings.Contains(warned[0].Message, `"lookup"`) {
		t.Fatalf("recorded warnings=%+v, want one tool_not_offered", warned)
	}

	// NoTools turns the selection off, so it does not warn.
	agent.NoTools = true
	client = &scriptedClient{results: []llm.Result{final("done")}}
	out, err := (Runner{Client: client}).Run(
		t.Context(), Turn{}, Start{Agent: agent, Input: userInput("go")})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range out.Warnings {
		if w.Code == WarningToolNotOffered {
			t.Fatalf("NoTools warned: %+v", w)
		}
	}
}

func TestMissingSelectedToolWarnsOnceAcrossContinue(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Interrupt the turn before its second model call.
	calls := 0
	interrupt := ModelHook{Name: "interrupt", Func: func(
		ctx context.Context, _ ModelPlan,
	) (ModelDecision, error) {
		calls++
		if calls == 2 {
			cancel()
			return ModelDecision{}, ctx.Err()
		}
		return ModelDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "a"),
		callResult("call-2", "a"),
		final("done"),
	}}
	runner := Runner{
		Client: client,
		Tools:  selectionTools(t, map[string]int{}),
		Hooks:  interrupt.Hooks(),
	}
	agent := testAgent()
	agent.Tools = []string{"a", "missing"}
	// The interrupted attempt counts toward the limit.
	agent.Limits.ModelCalls = 5
	out, err := runner.Run(ctx, Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     agent,
		Input:     userInput("go"),
	})
	if err != nil || out.Stop.Kind != StopInterrupted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "go-on"})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if len(client.inputs) != 3 {
		t.Fatalf("model calls=%d, want 3", len(client.inputs))
	}
	warned := 0
	for _, step := range out.Turn.Steps {
		for _, w := range step.Warnings {
			if w.Code == WarningToolNotOffered {
				warned++
			}
		}
	}
	if warned != 1 {
		t.Fatalf("recorded %d tool_not_offered warnings, want 1", warned)
	}
}

func TestToolSelectionChangesAutomaticVersion(t *testing.T) {
	revision := func(a Agent) string {
		t.Helper()
		client := &scriptedClient{results: []llm.Result{final("done")}}
		out, err := (Runner{Client: client}).Run(
			t.Context(), Turn{}, Start{Agent: a, Input: userInput("go")})
		if err != nil {
			t.Fatal(err)
		}
		return out.Agent.Version
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

// The order of Agent.Tools does not change the agent: a host that builds the
// list from a map can retry its Start.
func TestToolSelectionOrderDoesNotChangeVersion(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	runner := Runner{Client: client, Tools: selectionTools(t, map[string]int{})}
	start := func(tools ...string) Start {
		agent := testAgent()
		agent.Tools = tools
		return Start{CommandID: "start", Agent: agent, Input: userInput("go")}
	}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, start("b", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Agent.Tools; !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("recorded tools=%v, want sorted", got)
	}
	again, err := runner.Run(t.Context(), out.Turn, start("a", "b", "a"))
	if err != nil {
		t.Fatalf("retry with [a b a]: %v", err)
	}
	if again.Agent.Version != out.Agent.Version {
		t.Fatalf("revision %s, then %s", out.Agent.Version, again.Agent.Version)
	}
	if len(again.Turn.Steps) != len(out.Turn.Steps) {
		t.Fatal("the retry ran the turn again")
	}
}

// ErrAgentChanged tells the caller to retry with TurnView.Agent. A retry that
// passes the recorded agent back exactly succeeds, from a snapshot and
// through a Loader.
func TestRetryWithRecordedAgentSucceeds(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	runner := Runner{Client: client, Recorder: rec}
	agent := Agent{Model: "fixture", Tools: []string{"b", "a"}}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     agent,
		Input:     userInput("go"),
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := agent
	changed.Instructions = "different"
	if _, err := runner.Run(t.Context(), out.Turn, Start{
		CommandID: "start",
		Agent:     changed,
		Input:     userInput("go"),
	}); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("err=%v, want ErrAgentChanged", err)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	for name, turn := range map[string]Turn{
		"snapshot": out.Turn,
		"loader":   {ID: "t1"},
	} {
		again, err := runner.Run(t.Context(), turn, Start{
			CommandID: "start",
			Agent:     view.Agent,
			Input:     userInput("go"),
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if again.Stop.Kind != StopCompleted || again.Reply != "done" {
			t.Fatalf("%s: stop=%+v answer=%q", name, again.Stop, again.Reply)
		}
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
	if out.Next() != NextResolvePending {
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
	if err != nil || again.LastSeq != out.LastSeq || client.sends != 1 {
		t.Fatalf("retry err=%v revision=%d sends=%d", err, again.LastSeq, client.sends)
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

// Every hook the runner calls during a turn can read the turn with
// CurrentTurn, and sees the same view as its plan's Turn.
func TestCurrentTurnInHooks(t *testing.T) {
	seen := map[HookPoint]TurnView{}
	planned := map[HookPoint]TurnView{}
	record := func(point HookPoint, ctx context.Context, plan TurnView) {
		v, ok := CurrentTurn(ctx)
		if !ok {
			t.Errorf("%s: CurrentTurn reported no turn", point)
		}
		seen[point], planned[point] = v, plan
	}
	hooks := Hooks{
		BeforeModel: []ModelHook{{Name: "m", Func: func(
			ctx context.Context, p ModelPlan,
		) (ModelDecision, error) {
			record(HookBeforeModel, ctx, p.Turn)
			return ModelDecision{}, nil
		}}},
		BeforeSend: []SendHook{{Name: "s", Func: func(
			ctx context.Context, p SendPlan,
		) (SendDecision, error) {
			record(HookBeforeSend, ctx, p.Turn)
			return SendDecision{}, nil
		}}},
		BeforeTool: []ToolHook{{Name: "t", Func: func(
			ctx context.Context, p ToolPlan,
		) (ToolDecision, error) {
			record(HookBeforeTool, ctx, p.Turn)
			return ToolDecision{}, nil
		}}},
		AfterTool: []ResultHook{{Name: "r", Func: func(
			ctx context.Context, p ResultPlan,
		) (ResultDecision, error) {
			record(HookAfterTool, ctx, p.Turn)
			return ResultDecision{}, nil
		}}},
		BeforeFinish: []FinishHook{{Name: "f", Func: func(
			ctx context.Context, p FinishPlan,
		) (FinishDecision, error) {
			record(HookBeforeFinish, ctx, p.Turn)
			return FinishDecision{}, nil
		}}},
	}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "a"),
		final("done"),
	}}
	runner := Runner{
		Client: client,
		Tools:  selectionTools(t, map[string]int{}),
		Hooks:  hooks,
	}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		Agent: testAgent(),
		Input: userInput("go"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	for _, point := range []HookPoint{
		HookBeforeModel, HookBeforeSend, HookBeforeTool, HookAfterTool,
		HookBeforeFinish,
	} {
		v, ok := seen[point]
		if !ok {
			t.Fatalf("%s: hook did not run", point)
		}
		if v.ID != "t1" || v.Agent.Model != "fixture" ||
			v.Next() != NextRunning {
			t.Fatalf("%s: CurrentTurn=%+v", point, v)
		}
		if !reflect.DeepEqual(v, planned[point]) {
			t.Fatalf("%s: CurrentTurn differs from the plan's Turn", point)
		}
	}
}

// A run that a hook starts gives its executor and Toolset its own turn, and
// the hook still sees its plan's turn.
func TestCurrentTurnInNestedRun(t *testing.T) {
	var execView, toolsetView, hookView TurnView
	probe, err := tool.Func("a", "Probe.",
		func(ctx context.Context, _ tool.Call, _ struct {
			Value int `json:"value"`
		}) (tool.Outcome, error) {
			execView, _ = CurrentTurn(ctx)
			return tool.Succeeded("ok"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	inner := Runner{
		Client: &scriptedClient{results: []llm.Result{
			callResult("call-1", "a"),
			final("inner done"),
		}},
		Tools: toolsetFunc(func(ctx context.Context) ([]tool.Tool, error) {
			toolsetView, _ = CurrentTurn(ctx)
			return []tool.Tool{probe}, nil
		}),
	}
	innerAgent := testAgent()
	innerAgent.ID = "inner-agent"
	ran := false
	hooks := Hooks{BeforeModel: []ModelHook{{Name: "nest", Func: func(
		ctx context.Context, p ModelPlan,
	) (ModelDecision, error) {
		if ran {
			return ModelDecision{}, nil
		}
		ran = true
		if _, err := inner.Run(ctx, Turn{ID: "inner"}, Start{
			Agent: innerAgent,
			Input: userInput("go"),
		}); err != nil {
			return ModelDecision{}, err
		}
		hookView, _ = CurrentTurn(ctx)
		return ModelDecision{}, nil
	}}}}
	outer := Runner{
		Client: &scriptedClient{results: []llm.Result{final("done")}},
		Hooks:  hooks,
	}
	if _, err := outer.Run(t.Context(), Turn{ID: "outer"}, Start{
		Agent: testAgent(),
		Input: userInput("go"),
	}); err != nil {
		t.Fatal(err)
	}
	if execView.ID != "inner" || execView.Agent.ID != "inner-agent" {
		t.Fatalf("executor view=%s %s, want the inner turn",
			execView.ID, execView.Agent.ID)
	}
	if toolsetView.ID != "inner" || toolsetView.Agent.ID != "inner-agent" {
		t.Fatalf("toolset view=%s %s, want the inner turn",
			toolsetView.ID, toolsetView.Agent.ID)
	}
	if hookView.ID != "outer" || hookView.Agent.ID != "agent" {
		t.Fatalf("hook view=%s %s, want the outer turn",
			hookView.ID, hookView.Agent.ID)
	}
}

// An executor context whose turn cannot be replayed reports no turn, so a
// tool such as delegate's task tool fails instead of guessing the agent.
func TestCurrentTurnReportsFalseWhenReplayFails(t *testing.T) {
	ctx := context.WithValue(t.Context(), currentCallKey{}, callContext{
		ref:  CallRef{TurnID: "t1", CallID: "c1"},
		turn: func() *replayState { return nil },
	})
	if _, ok := CurrentCall(ctx); !ok {
		t.Fatal("CurrentCall reported no call")
	}
	if _, ok := CurrentTurn(ctx); ok {
		t.Fatal("CurrentTurn reported a turn it could not replay")
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
		view, err := out.Turn.View()
		if err != nil {
			t.Fatal(err)
		}
		if out.Origin != tc.want || view.Origin != tc.want {
			t.Fatalf("origin %q: outcome=%q replay=%q, want %q",
				tc.origin, out.Origin, view.Origin, tc.want)
		}
	}
	if v, _ := (Turn{}).View(); v.Origin != "" {
		t.Fatalf("unstarted origin=%q", v.Origin)
	}
}

type toolsetFunc func(context.Context) ([]tool.Tool, error)

func (f toolsetFunc) Tools(ctx context.Context) ([]tool.Tool, error) {
	return f(ctx)
}
