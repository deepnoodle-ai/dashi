package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/wonton/tui"
)

// A small known window and a long recorded turn make compaction eligible.
// Usage tests the policy independently of the compactor's text-size cut.
func fullWindow(ctx context.Context, call llm.Call) (llm.Result, error) {
	r, err := say("Original response.")(ctx, call)
	r.Usage = &llm.Usage{InputTokens: 1500, OutputTokens: 20}
	return r, err
}
func compactionSeed() string      { return strings.Repeat("Remember the project uses port 8443. ", 150) }
func compactWindow(c *fakeClient) { c.mu.Lock(); c.window = 4000; c.mu.Unlock() }

func TestManualCompactionQueueAndReplay(t *testing.T) {
	h := newHarness(t, fullWindow, say("<summary>PRIVATE SUMMARY</summary>"), say("Normal answer."))
	compactWindow(h.client)
	h.input("/compact")
	compactionMustHave(t, h.ui.text(), "Nothing needs compaction.")
	h.input(compactionSeed())
	title := h.app.eng.Session().Title
	if err := h.app.eng.Queue("queued before compact"); err != nil {
		t.Fatal(err)
	}
	h.input("/compact")
	if h.turns() != 2 || h.app.eng.Queued() != 1 || h.app.eng.Session().Title != title {
		t.Fatalf("manual consumed input or changed title: turns=%d queue=%d", h.turns(), h.app.eng.Queued())
	}
	compactionMustHave(t, h.ui.text(), "Compaction completed.")
	for _, b := range h.app.eng.Scrollback(t.Context()) {
		if strings.Contains(b.Text, "PRIVATE SUMMARY") || strings.Contains(b.Text, "<conversation>") {
			t.Fatalf("summary leaked in replay: %+v", b)
		}
	}
	h.input("next prompt")
	compactionMustHave(t, h.ui.text(), "Normal answer.")
	_, err := h.turn(3).View()
	if err != nil {
		t.Fatal(err)
	}
	if got := joinMessages(h.turn(3).Steps[0].Started.Input); got != "queued before compact\nnext prompt" {
		t.Fatalf("input=%q", got)
	}
}

func TestAutomaticCompactionInterruptedInputOrder(t *testing.T) {
	first, second := make(chan struct{}), make(chan struct{})
	h := newScreenHarness(t, 0, fullWindow, blockUntilDone(first), blockUntilDone(second), say("<summary>PRIVATE SUMMARY</summary>"), say("Original answered."), say("Later answered."))
	compactWindow(h.client)
	h.submit(compactionSeed())
	h.app.ctrl.Wait()
	h.scr.Reset() // Preserve recorded seed history; show the compaction workflow.
	if err := h.app.eng.Queue("pre-existing"); err != nil {
		t.Fatal(err)
	}
	h.submit("original prompt")
	<-first
	h.waitFor(func() bool { return strings.Contains(h.renderUnlocked(), "Compacting history") })
	saveCompactionScreen(t, h, "running")
	h.submit("later ordinary")
	h.submit("!later urgent")
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.app.ctrl.Wait()
	got := h.render()
	compactionMustHave(t, got, "Compaction interrupted.", "pending prompt: original prompt", "later ordinary", "later urgent")
	if h.app.eng.Queued() != 4 {
		t.Fatalf("queued=%d", h.app.eng.Queued())
	}
	saveCompactionScreen(t, h, "interrupted-pending")
	// Wonton InputField.OnSubmit invokes screen.submit for Enter.
	h.submit("")
	<-second
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.app.ctrl.Wait()
	if h.turns() != 2 || !h.app.eng.HasPending() {
		t.Fatalf("lost pending after second interruption: turns=%d", h.turns())
	}
	// Wonton InputField.OnSubmit invokes screen.submit for Enter.
	h.submit("")
	h.app.ctrl.Wait()
	if h.turns() != 4 || h.app.eng.Queued() != 0 || h.app.eng.HasPending() {
		t.Fatalf("turns=%d queued=%d pending=%v", h.turns(), h.app.eng.Queued(), h.app.eng.HasPending())
	}
	_, err := h.turn(3).View()
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.turn(4).View()
	if err != nil {
		t.Fatal(err)
	}
	if got := joinMessages(h.turn(3).Steps[0].Started.Input); got != "pre-existing\noriginal prompt" {
		t.Fatalf("original input=%q", got)
	}
	if got := joinMessages(h.turn(4).Steps[0].Started.Input); got != "later ordinary\nlater urgent" {
		t.Fatalf("later input=%q", got)
	}
	if strings.Count(inputText(h.client.inputAt(4)), "original prompt") != 1 {
		t.Fatal("original duplicated")
	}
	got = h.render()
	compactionMustHave(t, got, "Original answered.", "Later answered.")
	if !h.check(func() bool {
		for _, b := range h.scr.main().tr.Blocks {
			if b.Kind == KindCompaction && b.Text == "Compaction completed." {
				return true
			}
		}
		return false
	}) {
		t.Fatal("completed marker missing from Wonton transcript")
	}
	if strings.Contains(got, "PRIVATE SUMMARY") || strings.Contains(got, "pending prompt:") {
		t.Fatalf("leaked summary/pending:\n%s", got)
	}
	saveCompactionScreen(t, h, "ordered-delivery")
}

// Called under the Wonton loop lock by waitFor.
func (h *screenHarness) renderUnlocked() string {
	h.scr.drain()
	return tui.SprintScreen(h.scr.View(), tui.WithWidth(80), tui.WithHeight(30)).Text()
}

func TestCompactionFailureAndExitWarning(t *testing.T) {
	h := newScreenHarness(t, 0, fullWindow, say("<summary> </summary>"), say("Fallback answer."), say("<summary>success</summary>"))
	compactWindow(h.client)
	h.submit("/compact")
	h.app.ctrl.Wait()
	compactionMustHave(t, h.render(), "Nothing needs compaction.")
	saveCompactionScreen(t, h, "nothing-to-compact")
	h.submit(compactionSeed())
	h.app.ctrl.Wait()
	h.scr.Reset() // Preserve recorded seed history; show the compaction workflow.
	h.submit("/compact")
	h.app.ctrl.Wait()
	got := h.render()
	compactionMustHave(t, got, "summary was empty", "shortened")
	saveCompactionScreen(t, h, "failed-empty")
	h.submit("after failure")
	h.app.ctrl.Wait()
	// The summary fallback replaced the history; it keeps the last reply.
	if sent := inputText(h.client.inputAt(2)); !strings.Contains(sent, "shortened without a summary") ||
		!strings.Contains(sent, "Original response.") {
		t.Fatalf("history after the failure: %s", sent)
	}
	h.scr.Reset()
	h.submit("/compact")
	h.app.ctrl.Wait()
	mustHave(t, h.render(), "Compaction completed.")
	saveCompactionScreen(t, h, "completed")

	started := make(chan struct{})
	pending := newScreenHarness(t, 0, fullWindow, blockUntilDone(started))
	compactWindow(pending.client)
	pending.submit(compactionSeed())
	pending.app.ctrl.Wait()
	pending.scr.Reset()
	pending.submit("held only in memory")
	<-started
	pending.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	pending.app.ctrl.Wait()
	pending.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	got = pending.render()
	compactionMustHave(t, got, "will be lost", "memory")
	if pending.check(func() bool { return pending.scr.quitting }) {
		t.Fatal("first exit discarded input")
	}
	saveCompactionScreen(t, pending, "exit-warning")
}

func TestManualCompactionEnterLeavesQueuedPromptPending(t *testing.T) {
	started := make(chan struct{})
	h := newScreenHarness(t, 0, fullWindow, blockUntilDone(started), say("<summary>Remembered fact.</summary>"), say("Queued answered."))
	compactWindow(h.client)
	h.submit(compactionSeed())
	h.app.ctrl.Wait()
	if err := h.app.eng.Queue("queued before summary"); err != nil {
		t.Fatal(err)
	}
	h.submit("/compact")
	<-started
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.app.ctrl.Wait()
	h.submit("")
	h.app.ctrl.Wait()
	if h.turns() != 2 || h.app.eng.Queued() != 1 || len(h.client.inputs) != 3 {
		t.Fatalf("manual Enter dispatched input: turns=%d queued=%d calls=%d", h.turns(), h.app.eng.Queued(), len(h.client.inputs))
	}
	compactionMustHave(t, h.render(), "Compaction completed.", "1 queued message")
	h.submit("new prompt")
	h.app.ctrl.Wait()
	if h.turns() != 3 || h.app.eng.Queued() != 0 || joinMessages(h.turn(3).Steps[0].Started.Input) != "queued before summary\nnew prompt" {
		t.Fatal("explicit new prompt did not deliver the retained queue")
	}
}

type lostStartRecord struct {
	*filerecorder.Recorder
	mu         sync.Mutex
	lost       bool
	unreadable bool
	target     string
}

func (r *lostStartRecord) Commit(ctx context.Context, req dashi.CommitRequest) error {
	r.mu.Lock()
	lose := !r.lost && req.Step.Kind == dashi.StepTurnStarted && req.Step.Started.Origin != dashi.StartCompaction
	if lose {
		r.lost = true
		r.target = req.Step.TurnID
	}
	r.mu.Unlock()
	if err := r.Recorder.Commit(ctx, req); err != nil {
		return err
	}
	if lose {
		return errors.New("lost Start acknowledgment")
	}
	return nil
}
func (r *lostStartRecord) Load(ctx context.Context, id string) (dashi.Turn, error) {
	r.mu.Lock()
	fail := r.unreadable && id == r.target
	r.mu.Unlock()
	if fail {
		return dashi.Turn{}, errors.New("target temporarily unreadable")
	}
	return r.Recorder.Load(ctx, id)
}

func TestPendingStartUnknownAcknowledgment(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "readable", true: "unreadable"}[blocked], func(t *testing.T) {
			h := newHarness(t, say("Delivered once."), say("Later."))
			h.app.eng.Close()
			var rec *lostStartRecord
			h.recorder = func(r *filerecorder.Recorder) dashi.Recorder {
				rec = &lostStartRecord{Recorder: r, unreadable: blocked}
				return rec
			}
			h.build(Options{Resume: true})
			h.input("original prompt")
			if !h.app.eng.HasPending() {
				t.Fatal("unknown Start cleared pending")
			}
			h.input("/queue later")
			h.input("")
			if blocked {
				if !h.app.eng.HasPending() || h.turns() != 0 {
					t.Fatal("unreadable target accepted new work")
				}
				rec.mu.Lock()
				rec.unreadable = false
				rec.mu.Unlock()
				h.input("")
			}
			if h.app.eng.HasPending() || h.turns() != 2 {
				t.Fatalf("pending=%v turns=%d", h.app.eng.HasPending(), h.turns())
			}
			if strings.Count(joinMessages(h.turn(1).Steps[0].Started.Input), "original prompt") != 1 {
				t.Fatal("duplicate delivery")
			}
			compactionMustHave(t, h.ui.text(), "Delivered once.", "Later.")
		})
	}
}

// Save actual Wonton terminal cells, ANSI output, and text outside Git.
// The optional path is used by the keyless review-evidence run.
func saveCompactionScreen(t *testing.T, h *screenHarness, name string) {
	t.Helper()
	dir := os.Getenv("DASHI_COMPACTION_EVIDENCE")
	if dir == "" {
		return
	}
	h.mu.Lock()
	h.scr.drain()
	view := h.scr.View()
	screen := tui.SprintScreen(view, tui.WithWidth(80), tui.WithHeight(30))
	ansi := tui.Sprint(view, tui.WithWidth(80), tui.WithHeight(30), tui.WithColor(true))
	cells := make([][]any, 30)
	for y := range 30 {
		for x := range 80 {
			cells[y] = append(cells[y], screen.Cell(x, y))
		}
	}
	h.mu.Unlock()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(cells, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	for ext, data := range map[string][]byte{".txt": []byte(screen.Text()), ".ansi": []byte(ansi), ".cells.json": encoded} {
		if err := os.WriteFile(filepath.Join(dir, name+ext), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func compactionMustHave(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, item := range want {
		mustHave(t, text, item)
	}
}
