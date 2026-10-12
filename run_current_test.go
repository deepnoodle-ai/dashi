package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

func startWithParent(
	t *testing.T,
	rec Recorder,
	parent *CallRef,
) (RunResult, error) {
	t.Helper()
	client := &scriptedClient{results: []llm.Result{final("done")}}
	return (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		Turn{ID: "child"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Parent:    parent,
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
}

func TestStartParentSurvivesReplayAndReload(t *testing.T) {
	rec := NewMemoryRecorder()
	parent := &CallRef{TurnID: "p", CallID: "c1"}
	out, err := startWithParent(t, rec, parent)
	if err != nil {
		t.Fatal(err)
	}
	if out.Parent == nil || *out.Parent != *parent {
		t.Fatalf("outcome parent=%+v", out.Parent)
	}
	saved, err := rec.Load(t.Context(), "child")
	if err != nil {
		t.Fatal(err)
	}
	view, err := saved.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Parent == nil || *view.Parent != *parent {
		t.Fatalf("replayed parent=%+v", view.Parent)
	}
	// The view must not alias the recorded fact.
	view.Parent.CallID = "changed"
	again, err := saved.View()
	if err != nil || again.Parent.CallID != "c1" {
		t.Fatalf("parent link was shared: %+v %v", again.Parent, err)
	}
}

func TestStartParentRejectsChange(t *testing.T) {
	rec := NewMemoryRecorder()
	if _, err := startWithParent(
		t,
		rec,
		&CallRef{TurnID: "p", CallID: "c1"},
	); err != nil {
		t.Fatal(err)
	}
	_, err := startWithParent(t, rec, &CallRef{TurnID: "p", CallID: "c2"})
	if !errors.Is(err, ErrCommandIDReused) {
		t.Fatalf("err=%v, want command conflict", err)
	}
	_, err = startWithParent(t, rec, nil)
	if !errors.Is(err, ErrCommandIDReused) {
		t.Fatalf("dropping the parent: err=%v, want command conflict", err)
	}
}

func TestStartParentRequiresIDs(t *testing.T) {
	for name, parent := range map[string]*CallRef{
		"empty turn":   {TurnID: "", CallID: "c"},
		"empty call":   {TurnID: "p", CallID: ""},
		"invalid utf8": {TurnID: "p", CallID: "\xff"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := NewMemoryRecorder()
			_, err := startWithParent(t, rec, parent)
			if err == nil {
				t.Fatal("invalid parent was accepted")
			}
			if _, loadErr := rec.Load(t.Context(), "child"); loadErr == nil {
				t.Fatal("rejected start was recorded")
			}
		})
	}
}

func TestRootTurnRecordsNoParent(t *testing.T) {
	out, err := startWithParent(t, NewMemoryRecorder(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(out.Turn.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "parent") || out.Parent != nil {
		t.Fatalf("root turn has a parent: %s", b)
	}
}

func TestCurrentCallNamesTurnAndCall(t *testing.T) {
	var got CallRef
	var ok bool
	exec := execFunc(func(
		ctx context.Context,
		call tool.Call,
	) (tool.Outcome, error) {
		got, ok = CurrentCall(ctx)
		return tool.Outcome{
			State:  tool.StateSucceeded,
			Output: new(tool.TextOutput("ok")),
		}, nil
	})
	def := tool.Definition{Name: "probe", Schema: json.RawMessage(`{}`)}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-7", "probe"), final("done")},
	}
	_, err := start(
		t,
		client,
		tool.List{{Definition: def, Executor: exec}},
		NewMemoryRecorder(),
		Hooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != (CallRef{TurnID: "t1", CallID: "call-7"}) {
		t.Fatalf("current call=%+v ok=%v", got, ok)
	}
	if _, ok := CurrentCall(t.Context()); ok {
		t.Fatal("a plain context reported a current call")
	}
}

func TestSpendAddKeepsCostsSeparate(t *testing.T) {
	a := Spend{
		Cost:                   map[string]money.Amount{"USD": amount("0.5")},
		ToolCost:               map[string]money.Amount{"USD": amount("0.1")},
		CallsWithoutFinalUsage: 1,
		CostUnknown:            1,
	}
	b := Spend{
		Cost:             map[string]money.Amount{"USD": amount("0.25")},
		CostPartial:      2,
		UnpricedRequests: map[string]int{"search": 3},
	}
	sum := a.Add(b)
	if sum.Cost["USD"] != amount("0.75") {
		t.Fatalf("cost=%v", sum.Cost)
	}
	if sum.ToolCost["USD"] != amount("0.1") {
		t.Fatalf("model cost leaked into tool cost: %v", sum.ToolCost)
	}
	if sum.CallsWithoutFinalUsage != 1 || sum.CostUnknown != 1 || sum.CostPartial != 2 ||
		sum.UnpricedRequests["search"] != 3 {
		t.Fatalf("counters=%+v", sum)
	}
	if a.Cost["USD"] != amount("0.5") || b.UnpricedRequests["search"] != 3 {
		t.Fatal("Add changed an operand")
	}
}
