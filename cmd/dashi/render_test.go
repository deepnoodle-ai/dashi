package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	tl "github.com/deepnoodle-ai/dashi/tool"
)

func stepEvent(s dashi.Step) dashi.Event {
	return dashi.Event{TurnID: "t1", Seq: s.Seq, Step: &s}
}

func onlyBlock(t *testing.T, blocks []Block) Block {
	t.Helper()
	if len(blocks) != 1 {
		t.Fatalf("blocks=%+v", blocks)
	}
	return blocks[0]
}

func TestRenderTextDeltasGrowOneBlock(t *testing.T) {
	r := NewRenderer(1, "m", "/repo")
	ev := func(text string) dashi.Event {
		return dashi.Event{
			MessageID: "m1",
			Delta:     &llm.Delta{Kind: llm.DeltaText, Text: text},
		}
	}
	first := onlyBlock(t, r.Event(ev("Hel")))
	second := onlyBlock(t, r.Event(ev("lo")))
	if first.ID != second.ID || second.Text != "Hello" || second.Done {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	// A reasoning delta shows nothing.
	if got := r.Event(dashi.Event{
		MessageID: "m1",
		Delta:     &llm.Delta{Kind: llm.DeltaReasoning, Text: "hmm"},
	}); len(got) != 0 {
		t.Fatalf("reasoning shown: %+v", got)
	}
	// The settled step replaces the text and closes the block.
	done := onlyBlock(t, r.Event(stepEvent(dashi.Step{
		Kind: dashi.StepModelCallFinished,
		Model: &dashi.ModelFact{
			MessageID: "m1",
			Result: &llm.Result{
				Message: llm.AssistantMessage(llm.Text("Hello there")),
			},
		},
	})))
	if done.ID != first.ID || done.Text != "Hello there" || !done.Done {
		t.Fatalf("done=%+v", done)
	}
}

func TestFooterShowsIncompleteUsageWithoutQualifyingExactCost(t *testing.T) {
	view := dashi.TurnView{Spend: dashi.Spend{
		Usage:           llm.Usage{InputTokens: 1},
		UsageIncomplete: true,
		Cost:            map[string]money.Amount{"USD": money.MustParseAmount("0.5")},
	}}
	got := FooterText(1, "fixture", view)
	want := "[turn 1 | fixture | 0 model calls | usage incomplete | $0.500]"
	if got != want {
		t.Fatalf("footer presented incomplete counts or uncertain money: %q", got)
	}
}

func toolStep(kind dashi.StepKind, at time.Time, out *tl.Outcome) dashi.Step {
	return dashi.Step{
		Kind: kind,
		At:   at,
		Tool: &dashi.ToolFact{
			Call: tl.Call{
				ID:   "c1",
				Name: "bash",
				Args: json.RawMessage(`{"command":"go test ./...\nsecond line"}`),
			},
			Outcome: out,
		},
	}
}

func TestRenderToolLifecycle(t *testing.T) {
	r := NewRenderer(1, "m", "/repo")
	t0 := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	running := onlyBlock(t, r.Event(stepEvent(toolStep(dashi.StepToolStarted, t0, nil))))
	if running.State != StateRunning || running.Name != "bash" ||
		running.Arg != "go test ./... ..." {
		t.Fatalf("running=%+v", running)
	}
	prog := onlyBlock(t, r.Event(dashi.Event{Progress: &dashi.ToolProgress{
		CallID: "c1",
		Name:   "bash",
		Progress: tl.Progress{
			Message: "a\nb\nc\nd\ne",
		},
	}}))
	if prog.State != StateRunning || strings.Join(prog.Lines, "|") != "c|d|e" {
		t.Fatalf("progress=%+v", prog)
	}
	ok := tl.Succeeded("PASS")
	done := onlyBlock(t, r.Event(stepEvent(toolStep(
		dashi.StepToolFinished, t0.Add(1500*time.Millisecond), &ok,
	))))
	if done.State != StateOK || done.Elapsed != 1500*time.Millisecond || !done.Done {
		t.Fatalf("done=%+v", done)
	}
	if got := FormatBlock(done); !strings.Contains(got, "ok  1.5s") {
		t.Fatalf("line=%q", got)
	}
	failed := tl.Failed("exit status 1\nmore")
	bad := onlyBlock(t, r.Event(stepEvent(toolStep(dashi.StepToolFinished, t0, &failed))))
	if bad.State != StateFailed || strings.Join(bad.Lines, "|") != "more" {
		t.Fatalf("bad=%+v", bad)
	}
}

func TestRenderDeniedTool(t *testing.T) {
	r := NewRenderer(1, "m", "/repo")
	decision := toolStep(dashi.StepToolDecision, time.Time{}, nil)
	decision.Tool.Verdict = dashi.ToolVerdict{
		Kind:   dashi.DecisionDeny,
		Reason: "The user declined this call.",
	}
	if got := r.Event(stepEvent(decision)); len(got) != 0 {
		t.Fatalf("decision shown: %+v", got)
	}
	out := tl.Outcome{State: tl.StateNotExecuted}
	b := onlyBlock(t, r.Event(stepEvent(toolStep(dashi.StepToolFinished, time.Time{}, &out))))
	if b.State != StateDenied || len(b.Lines) != 1 ||
		b.Lines[0] != "The user declined this call." {
		t.Fatalf("denied=%+v", b)
	}
}

func TestRenderStopKinds(t *testing.T) {
	tests := []struct {
		name string
		stop dashi.Stop
		want string
	}{
		{
			"limit",
			dashi.Stop{
				Kind:  dashi.StopLimited,
				Limit: &dashi.LimitDetail{Kind: dashi.LimitModelCalls, Used: 40},
			},
			"Stopped after 40 model calls. Press Enter or type /continue to go on.",
		},
		{
			"other limit",
			dashi.Stop{Kind: dashi.StopLimited, Reason: "deadline"},
			"Stopped at a limit: deadline.",
		},
		{
			"interrupted",
			dashi.Stop{Kind: dashi.StopInterrupted},
			"Interrupted. Press Enter to continue, or type a new prompt.",
		},
		{"canceled", dashi.Stop{Kind: dashi.StopCanceled}, "Turn 3 canceled."},
		{
			"failed",
			dashi.Stop{Kind: dashi.StopFailed, Reason: "boom"},
			"The turn failed: boom.",
		},
		{"refused", dashi.Stop{Kind: dashi.StopRefused}, "The model refused the request."},
		{
			"waiting",
			dashi.Stop{Kind: dashi.StopWaiting, Reason: "tool result"},
			"The turn is waiting: tool result.",
		},
		{
			"uncertain",
			dashi.Stop{Kind: dashi.StopResultUnknown, Reason: "model call"},
			"The turn stopped with an unknown result: model call.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRenderer(3, "m", "/repo")
			stop := tt.stop
			b := onlyBlock(t, r.Event(stepEvent(dashi.Step{
				Kind: dashi.StepTurnStopped,
				Stop: &stop,
			})))
			if b.Kind != KindStop || b.Text != tt.want {
				t.Fatalf("block=%+v, want %q", b, tt.want)
			}
		})
	}
	// A completed turn adds no stop line.
	r := NewRenderer(1, "m", "/repo")
	if got := r.Event(stepEvent(dashi.Step{
		Kind: dashi.StepTurnStopped,
		Stop: &dashi.Stop{Kind: dashi.StopCompleted},
	})); len(got) != 0 {
		t.Fatalf("completed shown: %+v", got)
	}
}

func TestRenderSteerAndWarning(t *testing.T) {
	r := NewRenderer(1, "m", "/repo")
	got := r.Event(stepEvent(dashi.Step{
		Kind: dashi.StepMessagesAdded,
		Context: &dashi.ContextFact{
			Steering: true,
			Items: []dashi.ContextItem{{
				ID:      "s1",
				Message: llm.UserMessage(llm.Text("use go 1.26")),
			}},
		},
		Warnings: []dashi.Warning{{Code: dashi.WarningSteering, Message: "late"}},
	}))
	if len(got) != 2 {
		t.Fatalf("blocks=%+v", got)
	}
	if got[0].ID != "s:s1" || got[0].State != SteerDelivered ||
		FormatBlock(got[0]) != "  steer delivered: use go 1.26" {
		t.Fatalf("steer=%+v", got[0])
	}
	if FormatBlock(got[1]) != "warning: late" {
		t.Fatalf("warning=%+v", got[1])
	}
}

func TestFooterUsageAndCost(t *testing.T) {
	var view dashi.TurnView
	view.ModelCalls = 4
	view.Spend.Usage = llm.Usage{InputTokens: 3000, OutputTokens: 210}
	view.Spend.Cost = map[string]money.Amount{"USD": money.MustParseAmount("0.012")}
	got := FooterText(1, "gpt-6-luna", view)
	want := "[turn 1 | gpt-6-luna | 4 model calls | 3,210 tokens | $0.012]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	view.Spend.CostUnknown = 1
	if got := FooterText(1, "", view); !strings.Contains(got, ">=$0.012") {
		t.Fatalf("partial cost: %q", got)
	}
}

func TestMainArgPicksPathThenCommand(t *testing.T) {
	tests := []struct{ args, want string }{
		{`{"path":"a.go","command":"x"}`, "a.go"},
		{`{"command":"ls"}`, "ls"},
		{`{"pattern":"func Parse"}`, "func Parse"},
		{`{"zeta":"z","alpha":"a"}`, "a"},
		{`{"n":1}`, ""},
		{`not json`, ""},
	}
	for _, tt := range tests {
		if got := mainArg(json.RawMessage(tt.args), "/repo"); got != tt.want {
			t.Errorf("mainArg(%s)=%q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestTranscriptReplacesByID(t *testing.T) {
	var tr Transcript
	tr.Apply(Block{ID: "a", Text: "1"})
	tr.Apply(Block{Text: "no id"})
	tr.Apply(Block{ID: "a", Text: "2"})
	if len(tr.Blocks) != 2 || tr.Blocks[0].Text != "2" {
		t.Fatalf("blocks=%+v", tr.Blocks)
	}
}

func TestFinishedToolOnlyMessageClearsStreamedText(t *testing.T) {
	r := NewRenderer(1, "m", "/repo")
	live := onlyBlock(t, r.Event(dashi.Event{
		MessageID: "m1",
		Delta:     &llm.Delta{Kind: llm.DeltaText, Text: "Let me look."},
	}))
	var tr Transcript
	tr.Apply(live)
	done := onlyBlock(t, r.Event(stepEvent(dashi.Step{
		Kind: dashi.StepModelCallFinished,
		Model: &dashi.ModelFact{
			MessageID: "m1",
			Result:    &llm.Result{Message: llm.AssistantMessage()},
		},
	})))
	if done.ID != live.ID || done.Text != "" || !done.Done {
		t.Fatalf("done=%+v", done)
	}
	tr.Apply(done)
	if len(tr.Blocks) != 1 || tr.Blocks[0].Text != "" {
		t.Fatalf("live text stays: %+v", tr.Blocks)
	}
	// Replay has no text and no block.
	replay := NewRenderer(1, "m", "/repo")
	if got := replay.Event(stepEvent(dashi.Step{
		Kind: dashi.StepModelCallFinished,
		Model: &dashi.ModelFact{
			MessageID: "m1",
			Result:    &llm.Result{Message: llm.AssistantMessage()},
		},
	})); len(got) != 0 {
		t.Fatalf("replay blocks=%+v", got)
	}
}

func TestLineModeMarksEmptyFinishedCorrection(t *testing.T) {
	var out strings.Builder
	ui := newLineUI(&out)
	ui.Emit(Block{ID: "a", Kind: KindAssistant, Text: "Let me look."})
	ui.Emit(Block{ID: "a", Kind: KindAssistant, Done: true})
	if !strings.Contains(out.String(), "[Corrected: the model gave no answer text]") {
		t.Fatalf("output=%q", out.String())
	}
}
