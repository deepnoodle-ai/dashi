package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// recordedChildTurn returns a real recorded Turn with a tool call between
// two model calls, to stand in for a child Turn.
func recordedChildTurn(t *testing.T) dashi.Turn {
	t.Helper()
	h := newHarness(t, callTool("c1", "read_file", `{"path":"notes.txt"}`), say("The notes say hello."))
	if err := os.WriteFile(filepath.Join(h.dir, "notes.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.input("Read the notes")
	turn := h.turn(1)
	if len(turn.Steps) < 6 {
		t.Fatalf("turn has %d steps", len(turn.Steps))
	}
	return turn
}

func newTestFeed() *taskFeed {
	view := newTranscriptPane("task:t_1", func() int { return 80 }, func() string { return "" })
	return newTaskFeed("t_1", 1, "/repo", view)
}

func stepsOf(turn dashi.Turn) []dashi.Event {
	var out []dashi.Event
	for i := range turn.Steps {
		s := turn.Steps[i]
		out = append(out, dashi.Event{TurnID: turn.ID, Seq: s.Seq, Step: &s})
	}
	return out
}

// paneText is the plain text of a pane's blocks after the facts block.
func paneText(f *taskFeed) string {
	var lines []string
	for _, b := range f.view.tr.Blocks {
		if b.ID == taskFactsID {
			continue
		}
		lines = append(lines, FormatBlock(b))
	}
	return strings.Join(lines, "\n")
}

// wantText is what the pane shows for the whole turn: what RenderTurn
// shows for a reopened session.
func wantText(t *testing.T, turn dashi.Turn) string {
	t.Helper()
	blocks, err := RenderTurn(turnNumber(turn.ID), "", "/repo", turn, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tr Transcript
	for _, b := range blocks {
		tr.Apply(b)
	}
	var lines []string
	for _, b := range tr.Blocks {
		lines = append(lines, FormatBlock(b))
	}
	return strings.Join(lines, "\n")
}

func openLoad(turns ...dashi.Turn) taskLoad {
	return taskLoad{kind: taskLoadOpen, turns: turns}
}

func TestTaskFeedReplaysRecordThenHeldLiveEvents(t *testing.T) {
	turn := recordedChildTurn(t)
	events := stepsOf(turn)
	f := newTestFeed()
	// Live events from step 3 on arrive while the record of steps 1..4
	// loads; steps 3 and 4 come both ways.
	for _, ev := range events[2:] {
		f.push(ev)
	}
	if len(f.view.tr.Blocks) != 0 {
		t.Fatal("events applied before the record")
	}
	f.loaded(openLoad(dashi.Turn{ID: turn.ID, Steps: turn.Steps[:4]}))
	if got, want := paneText(f), wantText(t, turn); got != want {
		t.Fatalf("pane:\n%s\nwant:\n%s", got, want)
	}
	if len(f.held) != 0 {
		t.Fatalf("held=%v", f.held)
	}
}

func TestTaskFeedOrdersStepsAndDropsDuplicates(t *testing.T) {
	turn := recordedChildTurn(t)
	events := stepsOf(turn)
	f := newTestFeed()
	f.loaded(openLoad())
	// Overlapping observer calls deliver out of order and twice.
	order := []int{0, 2, 1, 1, 3, 0, 5, 4}
	for i := 6; i < len(events); i++ {
		order = append(order, i)
	}
	for _, i := range order {
		f.push(events[i])
	}
	if got, want := paneText(f), wantText(t, turn); got != want {
		t.Fatalf("pane:\n%s\nwant:\n%s", got, want)
	}
	if len(f.loads) != 1 || f.loads[0].kind != taskLoadFacts {
		t.Fatalf("the stop step did not ask for fresh facts: %+v", f.loads)
	}
}

func TestTaskFeedReloadsATurnWithAMissingStep(t *testing.T) {
	turn := recordedChildTurn(t)
	events := stepsOf(turn)
	f := newTestFeed()
	f.loaded(openLoad())
	f.push(events[0])
	f.push(events[1])
	// Step 3 is only in the record.
	f.push(events[3])
	if f.checkGaps(1) != true || len(f.takeLoads()) != 0 {
		t.Fatal("a gap found in this flush must wait one flush")
	}
	if f.checkGaps(2) {
		t.Fatal("the same gap counted as fresh")
	}
	loads := f.takeLoads()
	if len(loads) != 1 || loads[0].kind != taskLoadReload || loads[0].turn != turn.ID {
		t.Fatalf("loads=%+v", loads)
	}
	// Events keep arriving during the reload, and wait.
	for _, ev := range events[4:] {
		f.push(ev)
	}
	before := len(f.view.tr.Blocks)
	f.loaded(taskLoad{kind: taskLoadReload, turn: turn.ID, turns: []dashi.Turn{{ID: turn.ID, Steps: turn.Steps[:5]}}})
	if got, want := paneText(f), wantText(t, turn); got != want {
		t.Fatalf("pane:\n%s\nwant:\n%s", got, want)
	}
	if before == 0 || strings.Contains(paneText(f), "missing") {
		t.Fatal("reload did not replace the turn cleanly")
	}
}

func TestTaskFeedFailedReloadContinuesFromHeldStep(t *testing.T) {
	turn := recordedChildTurn(t)
	events := stepsOf(turn)
	f := newTestFeed()
	f.loaded(openLoad())
	f.push(events[0])
	f.push(events[3])
	f.push(events[4])
	f.checkGaps(1)
	f.checkGaps(2)
	f.takeLoads()
	f.loaded(taskLoad{kind: taskLoadReload, turn: turn.ID, err: errors.New("disk gone")})
	text := paneText(f)
	if !strings.Contains(text, "disk gone") || f.turns[turn.ID].lastSeq != events[4].Seq {
		t.Fatalf("failed reload: lastSeq=%d\n%s", f.turns[turn.ID].lastSeq, text)
	}
	if f.checkGaps(3) || len(f.takeLoads()) != 0 {
		t.Fatal("a failed reload loaded again")
	}
}

func TestTaskFeedFailedOpenShowsWarningAndGoesLive(t *testing.T) {
	turn := recordedChildTurn(t)
	events := stepsOf(turn)
	f := newTestFeed()
	f.push(events[3])
	f.loaded(taskLoad{kind: taskLoadOpen, err: errors.New("record locked")})
	facts := f.view.tr.Blocks[0]
	if facts.ID != taskFactsID || facts.Kind != KindWarning || !strings.Contains(facts.Text, "record locked") {
		t.Fatalf("facts=%+v", facts)
	}
	if f.turns[turn.ID].lastSeq != events[3].Seq {
		t.Fatal("the feed did not continue from the first held step")
	}
	// A Turn met after the failure starts at its first live step.
	f.push(dashi.Event{TurnID: "child-2", Seq: 7, Step: &dashi.Step{Kind: dashi.StepMessagesAdded, Seq: 7}})
	if f.turns["child-2"].lastSeq != 7 {
		t.Fatal("a later Turn waited for steps the record never gave")
	}
}

func TestTaskFeedPreviewsFollowTheirStep(t *testing.T) {
	turn := recordedChildTurn(t)
	events := stepsOf(turn)
	f := newTestFeed()
	f.loaded(openLoad())
	// Find the second model call: its started step and finished step.
	var started, finished dashi.Event
	for _, ev := range events {
		if ev.Step.Model != nil && ev.Step.Kind == dashi.StepModelCallStarted {
			started = ev
		}
		if ev.Step.Model != nil && ev.Step.Kind == dashi.StepModelCallFinished {
			finished = ev
		}
	}
	m := finished.Step.Model
	delta := func(text string) dashi.Event {
		return dashi.Event{TurnID: turn.ID, Seq: started.Seq, ModelCallID: m.ModelCallID, MessageID: m.MessageID, Delta: &llm.Delta{Kind: llm.DeltaText, Text: text}}
	}
	key := messageKey(m.MessageID, m.ModelCallID)
	// A delta ahead of its started step waits for it.
	for _, ev := range events[:started.Seq-1] {
		f.push(ev)
	}
	f.push(delta("The notes"))
	if _, ok := f.view.tr.index[key]; ok {
		t.Fatal("a preview applied before its step")
	}
	f.push(started)
	if i, ok := f.view.tr.index[key]; !ok || f.view.tr.Blocks[i].Text != "The notes" {
		t.Fatal("the held preview did not apply after its step")
	}
	// A gap drops the preview and ignores the message's deltas.
	f.push(dashi.Event{TurnID: turn.ID, Seq: started.Seq, ModelCallID: m.ModelCallID, MessageID: m.MessageID, Gap: true})
	f.push(delta(" say"))
	if i := f.view.tr.index[key]; f.view.tr.Blocks[i].Text != "" {
		t.Fatalf("gap kept preview text %q", f.view.tr.Blocks[i].Text)
	}
	for _, ev := range events[started.Seq:] {
		f.push(ev)
	}
	// A delta after its message committed is stale.
	f.push(delta(" stale"))
	if got, want := paneText(f), wantText(t, turn); got != want {
		t.Fatalf("pane:\n%s\nwant:\n%s", got, want)
	}
}

func TestTaskFeedCapsHeldPreviews(t *testing.T) {
	f := newTestFeed()
	for i := range taskPreviewCap + 1 {
		f.push(dashi.Event{TurnID: "child-1", Seq: 2, MessageID: fmt.Sprintf("m%d", i%3), Delta: &llm.Delta{Kind: llm.DeltaText, Text: "x"}})
	}
	f.push(dashi.Event{TurnID: "child-1", Seq: 1, Step: &dashi.Step{Kind: dashi.StepMessagesAdded, Seq: 1}})
	if f.heldPreviews != 0 || len(f.held["child-1"]) != 1 {
		t.Fatalf("held previews=%d events=%d", f.heldPreviews, len(f.held["child-1"]))
	}
	if t1 := f.turns["child-1"]; !t1.skip["a:m0"] || !t1.skip["a:m2"] {
		t.Fatal("dropped previews left no gap")
	}
}
