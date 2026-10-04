package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
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
		Reply:       "Done.",
	}
}

func startedStep(origin dashi.StartOrigin, input ...llm.Message) dashi.Step {
	return dashi.Step{TurnID: "t2", Seq: 1, Kind: dashi.StepTurnStarted, Started: &dashi.StartFact{Origin: origin, Input: input}}
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
	screen := tui.Sprint(blockView(b), tui.WithWidth(100), tui.WithColor(true))
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
				!strings.Contains(text, "\n    ⎿  completed · 1 model call · /task t_") || b.Detail != "" {
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
	if got, want := taskLineUnderText(b), "    ⎿  completed · 1 model call · $0.0069 · /task t_u6d77c to inspect"; got != want {
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
	// A later turn of the task has no tool line to sit under, and says
	// which turn it is.
	v.Turns, v.TurnID = 2, "child-2"
	if b = taskStatusBlock(v); b.Parent != "" || !strings.HasPrefix(FormatBlock(b), "  ↳ task t_u6d77c (explore) turn 2 record unavailable") {
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
	// The finished tool line keeps the status line, and the pair is done only
	// once the task is.
	tr.Apply(Block{ID: "t:call-1", Kind: KindTool, Name: "task", State: StateOK, Done: true})
	if len(tr.Blocks) != 1 || tr.Blocks[0].Task == nil || tr.Blocks[0].Done {
		t.Fatalf("running=%+v", tr.Blocks)
	}
	tr.Apply(taskStatusBlock(completedTaskView()))
	if len(tr.Blocks) != 1 || !tr.Blocks[0].Done || tr.Blocks[0].Task.State != StateOK {
		t.Fatalf("done=%+v", tr.Blocks)
	}
	screen := tui.Sprint(blockView(tr.Blocks[0]), tui.WithWidth(100), tui.WithColor(true))
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

// sendStep is a step of a task_send call that sends text to t_k3f2.
func sendStep(kind dashi.StepKind, deliver string, out *tool.Outcome) dashi.Step {
	return dashi.Step{TurnID: "p-2", Kind: kind, Tool: &dashi.ToolFact{
		Call: tool.Call{ID: "send-1", Name: delegate.SendToolName, Args: json.RawMessage(
			`{"task_id":"t_k3f2","message":"Skip vendor/; the parser is in config/\nmore","deliver":"` + deliver + `"}`)},
		Outcome: out,
	}}
}

// A task_send call shows as a tool line while it runs or when it fails, and
// as a message line once it sent its message.
func TestTaskSendShowsAMessageLine(t *testing.T) {
	r := NewRenderer(2, "m", "/repo")
	running := onlyBlock(t, r.Event(stepEvent(sendStep(dashi.StepToolStarted, "queue", nil))))
	if running.Kind != KindTool || running.Arg != "t_k3f2 · Skip vendor/; the parser is in config/ ..." {
		t.Fatalf("running=%+v", running)
	}
	ok := tool.Succeeded("Sent to task t_k3f2 (running).")
	sent := onlyBlock(t, r.Event(stepEvent(sendStep(dashi.StepToolFinished, "queue", &ok))))
	if sent.Kind != KindTaskMessage || sent.ID != running.ID {
		t.Fatalf("sent=%+v", sent)
	}
	if got, want := FormatBlock(sent), "  → t_k3f2 (queued)  Skip vendor/; the parser is in config/ ..."; got != want {
		t.Fatalf("plain=%q want %q", got, want)
	}
	steer := onlyBlock(t, NewRenderer(2, "m", "/repo").Event(stepEvent(sendStep(dashi.StepToolFinished, "steer", &ok))))
	if got, want := FormatBlock(steer), "  → t_k3f2  Skip vendor/; the parser is in config/ ..."; got != want {
		t.Fatalf("steer=%q want %q", got, want)
	}
	failed := tool.Failed("unknown task t_k3f2")
	bad := onlyBlock(t, NewRenderer(2, "m", "/repo").Event(stepEvent(sendStep(dashi.StepToolFinished, "steer", &failed))))
	if bad.Kind != KindTool || bad.State != StateFailed {
		t.Fatalf("failed=%+v", bad)
	}
}

// A task's message shows as a message line where the parent got it: in a
// steer, or in the queue turn it started, before the task's result marker.
func TestTaskMessageReminderShowsAMessageLine(t *testing.T) {
	msg := delegate.Message{ID: "msg:c-1/m1", From: "task t_k3f2 (explore)", To: "parent", Text: "Parse is in config/load.go; env overrides too?"}
	steered := onlyBlock(t, NewRenderer(2, "m", "/repo").Event(stepEvent(dashi.Step{
		Kind:    dashi.StepMessagesAdded,
		Context: &dashi.ContextFact{Steering: true, Items: []dashi.ContextItem{msg.Item()}},
	})))
	want := "  ← t_k3f2  Parse is in config/load.go; env overrides too?"
	if steered.Kind != KindTaskMessage || FormatBlock(steered) != want {
		t.Fatalf("steered=%q %+v", FormatBlock(steered), steered)
	}
	blocks := NewRenderer(2, "m", "/repo").Event(stepEvent(startedStep(dashi.StartScheduled,
		msg.Item().Message, completedTaskView().Notice().Item().Message)))
	if len(blocks) != 2 || FormatBlock(blocks[0]) != want || blocks[1].Kind != KindTaskResult || blocks[0].ID == blocks[1].ID {
		t.Fatalf("queue turn=%+v", blocks)
	}
}

// On screen a message line keeps to one row: the terminal edge cuts the
// text, and the arrow and the queued mark stay.
func TestTaskMessageLineKeepsToOneRow(t *testing.T) {
	b := taskMessageBlock("m", MessageToTask, "t_k3f2", strings.Repeat("word ", 40), true)
	for w := 30; w <= 120; w += 10 {
		screen := tui.SprintScreen(blockView(b), tui.WithWidth(w), tui.WithHeight(4)).Text()
		lines := strings.Split(strings.TrimRight(screen, "\n "), "\n")
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "  → t_k3f2 (queued)  word word") {
			t.Fatalf("width %d:\n%s", w, screen)
		}
	}
	styled := tui.Sprint(blockView(b), tui.WithWidth(80), tui.WithColor(true))
	if !strings.Contains(styled, "\x1b[0;36m  → ") {
		t.Fatalf("arrow color=%q", styled)
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

// deliveryNotices returns every task-delivery line the UI was sent.
func deliveryNotices(u *recorderUI) []Block {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []Block
	for _, b := range u.log {
		if b.ID == "task-delivery" {
			out = append(out, b)
		}
	}
	return out
}

// A task that ends while its parent turn runs says nothing about a stopped
// parent: its result is sent when the turn ends.
func TestTaskEndDuringARunningParentShowsNoDeliveryNotice(t *testing.T) {
	hold := make(chan struct{})
	client := &taskFixtureClient{parentHold: hold}
	ui := newRecorderUI()
	a, _ := taskTestApp(t, client, ui)
	a.ctrl.mu.Lock()
	a.ctrl.autoTaskDelivery = true
	a.ctrl.mu.Unlock()
	a.ctrl.Input("Delegate this job")
	waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
	if _, err := a.eng.taskHost().tasks.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The end's wake-up runs in the background; give it time to speak.
	time.Sleep(100 * time.Millisecond)
	if a.ctrl.Mode() != ModeRunning {
		t.Fatalf("mode=%v, want the parent still running", a.ctrl.Mode())
	}
	if got := deliveryNotices(ui); len(got) != 0 {
		t.Fatalf("notice while the parent runs: %q", got[0].Text)
	}
	close(hold)
	a.ctrl.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for parent, _ := client.counts(); parent < 3 && time.Now().Before(deadline); parent, _ = client.counts() {
		time.Sleep(time.Millisecond)
	}
	a.ctrl.Wait()
	if parent, _ := client.counts(); parent != 3 {
		t.Fatalf("parent calls=%d: the result was not delivered after the turn", parent)
	}
	if got := deliveryNotices(ui); len(got) != 0 {
		t.Fatalf("notice after delivery: %q", got[0].Text)
	}
}

// A stopped parent holds task results. The line says how to send them, in
// plain words, as a quiet indented line.
func TestStoppedParentDeliveryNoticeIsPlainAndQuiet(t *testing.T) {
	client := &taskFixtureClient{}
	ui := newRecorderUI()
	cfg := testConfig(t)
	cfg.Client = client
	cfg.ModelCalls = 1 // the parent stops after it starts the task
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.ctrl.CancelWork(); a.eng.closeTasks(); a.ctrl.Wait(); a.eng.Close() })
	a.ctrl.Input("Delegate this job")
	a.ctrl.Wait()
	waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
	if _, err := a.eng.taskHost().tasks.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if last := a.eng.Last(t.Context()); !last.Open() || !last.CanGoOn() {
		t.Fatalf("parent not stopped: %+v", last)
	}
	var got []Block
	deadline := time.Now().Add(5 * time.Second)
	for len(got) == 0 && time.Now().Before(deadline) {
		a.ctrl.scheduleTaskNotices()
		got = deliveryNotices(ui)
		time.Sleep(5 * time.Millisecond)
	}
	if len(got) == 0 {
		t.Fatal("no delivery notice for the stopped parent")
	}
	b := got[len(got)-1]
	if b.Kind != KindHint || !strings.Contains(b.Text, "Press Enter") ||
		strings.Contains(b.Text, "inspection") || strings.Contains(b.Text, "parent") {
		t.Fatalf("notice=%+v", b)
	}
	if plain := FormatBlock(b); !strings.HasPrefix(plain, "  ") {
		t.Fatalf("plain=%q is not indented", plain)
	}
	styled := tui.Sprint(blockView(b), tui.WithWidth(100), tui.WithColor(true))
	text := tui.SprintScreen(blockView(b), tui.WithWidth(100), tui.WithHeight(4)).Text()
	if !strings.Contains(styled, "38;5;248m") || !strings.HasPrefix(text, "  T") {
		t.Fatalf("screen=%q text=%q is not muted and indented", styled, text)
	}
}

// The status line under a tool line, and a status line on its own, keep a
// gap after their marker and around each separator at every width, also
// where the line wraps.
func TestTaskLinesKeepTheirSpacesAtEveryWidth(t *testing.T) {
	v := completedTaskView()
	v.ModelCalls, v.ToolCalls = 2, 1
	var under Transcript
	under.Apply(Block{ID: "t:call-1", Kind: KindTool, Name: "task", Arg: "general · Test wait and respond · t_sfgudh", State: StateOK, Done: true})
	under.Apply(taskStatusBlock(v))
	var alone Transcript
	alone.Apply(taskStatusBlock(v))
	result := Block{Kind: KindTaskResult, Arg: "task t_u6d77c (explore)", Name: "completed", Text: "result delivered", State: StateOK}
	for _, b := range []Block{under.Blocks[0], alone.Blocks[0], result} {
		for w := 30; w <= 120; w++ {
			screen := tui.SprintScreen(blockView(b), tui.WithWidth(w), tui.WithHeight(8)).Text()
			for _, bad := range []string{"⎿c", "⎿ c", "d·", "↳t"} {
				if strings.Contains(screen, bad) {
					t.Fatalf("width %d has %q:\n%s", w, bad, screen)
				}
			}
			if strings.Contains(screen, "⎿") && !strings.Contains(screen, "⎿  completed") {
				t.Fatalf("width %d:\n%s", w, screen)
			}
		}
		plain := FormatBlock(b)
		if strings.Contains(plain, "⎿") && !strings.Contains(plain, "⎿  completed") {
			t.Fatalf("plain=%q", plain)
		}
		if strings.Contains(plain, "↳") && !strings.Contains(plain, "↳ task") {
			t.Fatalf("plain=%q", plain)
		}
	}
}
