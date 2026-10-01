package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/wonton/tui"
)

func completedTaskView() delegate.TaskView {
	return delegate.TaskView{
		ID:          "t_u6d77c",
		SessionID:   "child",
		TurnID:      "child-1",
		Turns:       1,
		Subagent:    "explore",
		Description: "messaging test wait",
		Parent:      dashi.CallRef{TurnID: "parent-1", CallID: "call-1"},
		State:       delegate.TaskState(dashi.StopCompleted),
		ModelCalls:  1,
		Spend:       dashi.Spend{Cost: map[string]money.Amount{"USD": money.MustParseAmount("0.0069255")}},
		Answer:      "Done.",
	}
}

func startedStep(origin dashi.StartOrigin, input ...llm.Message) dashi.Step {
	return dashi.Step{TurnID: "t2", Seq: 1, Kind: dashi.StepStarted, Started: &dashi.StartFact{Origin: origin, Input: input}}
}

func TestTaskResultTurnShowsMarkerInsteadOfEmptyUserLine(t *testing.T) {
	notice := completedTaskView().Notice().Item().Message
	blocks := NewRenderer(2, "m", "/repo").Event(stepEvent(startedStep(dashi.StartScheduled, notice)))
	b := onlyBlock(t, blocks)
	if b.Kind != KindTaskResult || b.State != StateOK || b.Name != "completed" {
		t.Fatalf("marker=%+v", b)
	}
	if got, want := FormatBlock(b), "↳ task t_u6d77c (explore) completed · result delivered"; got != want {
		t.Fatalf("plain=%q want %q", got, want)
	}
	screen := tui.Sprint(blockView(b), tui.WithWidth(100))
	if !strings.Contains(screen, "\x1b[0;32m↳ ") || !strings.Contains(screen, "\x1b[0;32m completed") {
		t.Fatalf("marker colors=%q", screen)
	}

	// Typed text with a notice keeps the user line after the marker.
	blocks = NewRenderer(2, "m", "/repo").Event(stepEvent(startedStep(dashi.StartUser, notice, llm.UserMessage(llm.Text("next")))))
	if len(blocks) != 2 || blocks[0].Kind != KindTaskResult || blocks[1].Kind != KindUser || blocks[1].Text != "next" {
		t.Fatalf("blocks=%+v", blocks)
	}

	// A failed task's marker takes the failure color.
	failed := completedTaskView()
	failed.State, failed.Reason = delegate.TaskState(dashi.StopFailed), "boom"
	b = onlyBlock(t, NewRenderer(2, "m", "/repo").Event(stepEvent(startedStep(dashi.StartScheduled, failed.Notice().Item().Message))))
	if b.State != StateFailed || b.Name != "failed" {
		t.Fatalf("failed marker=%+v", b)
	}
}

func TestTaskResultTurnHasNoEmptyUserLineLiveOrReplayed(t *testing.T) {
	client := &taskFixtureClient{}
	ui := newRecorderUI()
	a, _ := taskTestApp(t, client, ui)
	result, err := a.eng.Prompt(t.Context(), "Delegate this job")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.eng.QuietTasks(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	ui.mu.Lock()
	live := append([]Block(nil), ui.log...)
	ui.mu.Unlock()
	for name, blocks := range map[string][]Block{"live": live, "replay": a.eng.Scrollback(t.Context())} {
		users, markers := 0, 0
		for _, b := range blocks {
			switch b.Kind {
			case KindUser:
				if strings.TrimSpace(b.Text) == "" {
					t.Fatalf("%s: empty user line in %+v", name, blocks)
				}
				users++
			case KindTaskResult:
				markers++
				if !strings.HasPrefix(FormatBlock(b), "↳ task t_") || !strings.HasSuffix(FormatBlock(b), "(general) completed · result delivered") {
					t.Fatalf("%s: marker=%q", name, FormatBlock(b))
				}
			case KindNotice:
				if strings.Contains(b.Text, "delivery is recorded") {
					t.Fatalf("%s: repeated delivery notice", name)
				}
			}
		}
		if users != 1 || markers != 1 {
			t.Fatalf("%s: users=%d markers=%d", name, users, markers)
		}
	}
	// Live, the status line sits under the tool line that started the task.
	var tr Transcript
	for _, b := range live {
		tr.Apply(b)
	}
	for _, b := range tr.Blocks {
		if b.Kind == KindTaskStatus {
			t.Fatalf("status line left the tool line: %+v", b)
		}
		if b.Kind == KindTool && b.Name == delegate.ToolName {
			text := FormatBlock(b)
			if b.Task == nil || !b.Done || !strings.Contains(text, "general · Check the exact effect · t_") ||
				!strings.Contains(text, "\n    ⎿ completed · 1 model call · /task t_") || b.Detail != "" {
				t.Fatalf("task tool=%q %+v", text, b)
			}
		}
	}
}

func TestTaskStatusLineCountsAndCost(t *testing.T) {
	v := completedTaskView()
	b := taskStatusBlock(v)
	if b.Kind != KindTaskStatus || b.State != StateOK || !b.Done || b.Parent != "t:call-1" || b.ID != "task:child-1" {
		t.Fatalf("block=%+v", b)
	}
	if got, want := taskLineUnderText(b), "    ⎿ completed · 1 model call · $0.0069 · /task t_u6d77c to inspect"; got != want {
		t.Fatalf("line=%q want %q", got, want)
	}
	v.State, v.ModelCalls, v.ToolCalls = delegate.TaskRunning, 3, 1
	v.Spend = dashi.Spend{Cost: map[string]money.Amount{"USD": money.MustParseAmount("0.0231")}}
	b = taskStatusBlock(v)
	if b.State != StateRunning || b.Done || b.Text != "3 model calls · 1 tool call · $0.023 · /task t_u6d77c to inspect" {
		t.Fatalf("running=%+v", b)
	}
	v.ModelCalls, v.ToolCalls, v.Spend = 0, 0, dashi.Spend{}
	if b = taskStatusBlock(v); b.Text != "/task t_u6d77c to inspect" {
		t.Fatalf("starting=%q", b.Text)
	}
	v.State = taskApprovalPending
	if b = taskStatusBlock(v); b.State != StateWaiting || b.Done || !strings.HasPrefix(b.Text, "0 model calls") {
		t.Fatalf("approval=%+v", b)
	}
	v.State, v.Reason = delegate.TaskState(dashi.StopFailed), "provider said no\nmore"
	if b = taskStatusBlock(v); b.State != StateFailed || b.Name != "failed: provider said no ..." {
		t.Fatalf("failed=%+v", b)
	}
	v.State, v.Reason, v.ModelCalls = taskRecordUnavailable, "journal missing", 4
	if b = taskStatusBlock(v); b.State != StateFailed || b.Text != "/task t_u6d77c to inspect" {
		t.Fatalf("unavailable=%+v", b)
	}
	v.Turns = 2 // A later turn of the task has no tool line to sit under.
	if b = taskStatusBlock(v); b.Parent != "" || !strings.HasPrefix(FormatBlock(b), "  ↳ task t_u6d77c (explore) record unavailable") {
		t.Fatalf("later turn=%q", FormatBlock(b))
	}
	if got, want := taskSummary(completedTaskView()), "t_u6d77c · explore · messaging test wait · completed · 1 model call · $0.0069"; got != want {
		t.Fatalf("summary=%q want %q", got, want)
	}
	if got, want := taskToolArg([]byte(`{"subagent":"explore","description":"messaging test wait","prompt":"p"}`), "turn", "call"), "explore · messaging test wait · "+delegate.TaskID(delegate.ChildID("turn", "call")); got != want {
		t.Fatalf("arg=%q want %q", got, want)
	}
}

func TestTranscriptKeepsTaskStatusUnderItsToolLine(t *testing.T) {
	var tr Transcript
	tr.Apply(Block{ID: "t:call-1", Kind: KindTool, Name: "task", State: StateRunning})
	running := taskStatusBlock(completedTaskView())
	running.State, running.Done = StateRunning, false
	tr.Apply(running)
	// The settled tool line keeps the status line, and the pair is done only
	// once the task is.
	tr.Apply(Block{ID: "t:call-1", Kind: KindTool, Name: "task", State: StateOK, Done: true})
	if len(tr.Blocks) != 1 || tr.Blocks[0].Task == nil || tr.Blocks[0].Done {
		t.Fatalf("running=%+v", tr.Blocks)
	}
	tr.Apply(taskStatusBlock(completedTaskView()))
	if len(tr.Blocks) != 1 || !tr.Blocks[0].Done || tr.Blocks[0].Task.State != StateOK {
		t.Fatalf("done=%+v", tr.Blocks)
	}
	screen := tui.Sprint(blockView(tr.Blocks[0]), tui.WithWidth(100))
	if !strings.Contains(screen, "\x1b[0;32mcompleted") || !strings.Contains(screen, "⎿") {
		t.Fatalf("screen=%q", screen)
	}
	// Without its tool line, the status line stands alone and names its task.
	var alone Transcript
	alone.Apply(taskStatusBlock(completedTaskView()))
	if len(alone.Blocks) != 1 || alone.Blocks[0].Kind != KindTaskStatus {
		t.Fatalf("alone=%+v", alone.Blocks)
	}
}

func TestLineUIPrintsTaskStatusOncePerState(t *testing.T) {
	var out bytes.Buffer
	ui := newLineUI(&out)
	v := completedTaskView()
	v.State = delegate.TaskRunning
	ui.Emit(taskStatusBlock(v))
	v.ModelCalls = 2
	ui.Emit(taskStatusBlock(v))
	v.State = delegate.TaskState(dashi.StopCompleted)
	ui.Emit(taskStatusBlock(v))
	ui.Emit(taskStatusBlock(v))
	if got, want := out.String(), "  ↳ task t_u6d77c (explore) completed · 2 model calls · $0.0069 · /task t_u6d77c to inspect\n"; got != want {
		t.Fatalf("plain=%q want %q", got, want)
	}
}

func TestToolOutputHintPluralizes(t *testing.T) {
	if got := outputLinesText("one"); got != "1 output line" {
		t.Fatal(got)
	}
	if got := outputLinesText("one\ntwo"); got != "2 output lines" {
		t.Fatal(got)
	}
}
