package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/wonton/tui"
)

// screenHarness runs an app on a screen without a terminal. One goroutine
// plays the Wonton loop: it runs the events the screen posts, one at a time,
// under mu.
type screenHarness struct {
	*harness
	scr *screen
	mu  sync.Mutex
}

func newScreenHarness(t *testing.T, attempts int, script ...reply) *screenHarness {
	t.Helper()
	store := newHarness(t).store // fresh store and temp dir
	dir := t.TempDir()
	scr := newScreen(dir)
	client := &fakeClient{script: script}
	h := &harness{t: t, dir: dir, store: store, client: client, limit: attempts}
	a, err := buildApp(t.Context(), appConfig{
		Dir:        dir,
		HomeDir:    t.TempDir(),
		Model:      "fixture",
		Client:     client,
		Store:      store,
		ModelCalls: attempts,
	}, scr)
	if err != nil {
		t.Fatal(err)
	}
	scr.bind(a)
	if _, err := a.openSession(store, dir, Options{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.eng.Close)
	h.app = a
	sh := &screenHarness{harness: h, scr: scr}
	events := make(chan tui.Event, 1000)
	scr.send = func(e tui.Event) { events <- e }
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range events {
			sh.mu.Lock()
			scr.HandleEvent(e)
			sh.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		a.ctrl.CancelWork()
		a.ctrl.Wait()
		close(events)
		<-done
	})
	return sh
}

func (h *screenHarness) key(e tui.KeyEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scr.HandleEvent(e)
}

func (h *screenHarness) submit(text string) {
	h.mu.Lock()
	h.scr.submit(text)
	h.mu.Unlock()
}

// render draws the screen at 80 by 30 and returns its text.
func (h *screenHarness) render() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scr.drain()
	view := h.scr.View()
	return tui.SprintScreen(view, tui.WithWidth(80), tui.WithHeight(30)).Text()
}

// check runs cond under the loop lock.
func (h *screenHarness) check(cond func() bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return cond()
}

func (h *screenHarness) waitFor(cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !h.check(cond) {
		if time.Now().After(deadline) {
			h.t.Fatalf("timeout; screen:\n%s", h.render())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestScreenShowsTurnAsItStreams(t *testing.T) {
	h := newScreenHarness(t, 0,
		callTool("c1", "list_directory", `{}`),
		say("The fix is done."),
	)
	h.submit("fix the test")
	h.app.ctrl.Wait()
	got := h.render()
	for _, want := range []string{
		"fix the test",
		"list_directory",
		"ok",
		"The fix is done.",
		"turn 1 · fixture · 2 model calls",
		"fixture ·",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestScreenApprovalPromptAndKeys(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	h := newScreenHarness(t, 0,
		callTool("c1", "write_note", `{"text":"one"}`),
		say("Noted."),
		callTool("c2", "write_note", `{"text":"two"}`),
		say("Refused."),
	)
	h.submit("write a note")
	h.waitFor(func() bool { return h.scr.approval != nil })
	shown := h.render()
	for _, want := range []string{"Allow write_note?", "[y] Allow once", "[a] Allow for this Dashi run", "[n] Deny"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("missing %q in:\n%s", want, shown)
		}
	}
	if h.app.ctrl.Mode() != ModeApproval {
		t.Fatalf("mode=%v", h.app.ctrl.Mode())
	}
	// Other keys do nothing. y allows once.
	h.key(tui.KeyEvent{Rune: 'x'})
	if !h.check(func() bool { return h.scr.approval != nil }) {
		t.Fatal("a stray key answered the prompt")
	}
	h.key(tui.KeyEvent{Rune: 'y'})
	h.app.ctrl.Wait()
	if runs.Load() != 1 {
		t.Fatalf("runs=%d", runs.Load())
	}

	h.submit("again")
	h.waitFor(func() bool { return h.scr.approval != nil })
	h.key(tui.KeyEvent{Rune: 'n'})
	h.mu.Lock()
	h.scr.finishApproval(Approval{Reason: declinedReason})
	h.mu.Unlock()
	h.app.ctrl.Wait()
	if runs.Load() != 1 {
		t.Fatalf("a denied call ran: runs=%d", runs.Load())
	}
	if !strings.Contains(h.render(), "denied") {
		t.Fatalf("no denied line:\n%s", h.render())
	}
}

func TestScreenCtrlCByMode(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	started := make(chan struct{})
	h := newScreenHarness(t, 0, blockUntilDone(started), say("Then this."))
	h.submit("first")
	<-started
	// Running: Ctrl-C cancels the turn and does not quit.
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.app.ctrl.Wait()
	if h.check(func() bool { return h.scr.quitting }) {
		t.Fatal("Ctrl-C while running quit the program")
	}
	if !strings.Contains(h.render(), "Interrupted.") {
		t.Fatalf("no interrupt line:\n%s", h.render())
	}
	// Idle: the first Ctrl-C protects input, then an unchanged repeat quits.
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	if h.check(func() bool { return h.scr.quitting }) {
		t.Fatal("first idle Ctrl-C quit")
	}
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	if !h.check(func() bool { return h.scr.quitting }) {
		t.Fatal("repeated Ctrl-C at idle did not quit")
	}
}

func TestScreenInterruptDuringApprovalKeepsSession(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	h := newScreenHarness(t, 0, callTool("c1", "write_note", `{"text":"one"}`))
	h.submit("write")
	h.waitFor(func() bool { return h.scr.approval != nil })
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.app.ctrl.Wait()
	h.waitFor(func() bool { return h.app.ctrl.Mode() == ModeIdle && h.scr.approval == nil })
	if h.check(func() bool { return h.scr.quitting }) {
		t.Fatal("foreground interrupt quit the session")
	}
	if runs.Load() != 0 || h.check(func() bool { return h.scr.approval != nil }) {
		t.Fatalf("runs=%d", runs.Load())
	}
	last := h.app.eng.Last(t.Context())
	if !last.CanGoOn() {
		t.Fatalf("stop=%+v", last.View.Stop)
	}
}

func TestScreenSteerWhileRunning(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	first := func(ctx context.Context, call llm.Call) (llm.Result, error) {
		close(started)
		<-release
		return callTool("c1", "list_directory", `{}`)(ctx, call)
	}
	h := newScreenHarness(t, 0, first, say("Done."))
	h.submit("start")
	<-started
	h.submit("!use plan B")
	if !strings.Contains(h.render(), "steer queued: use plan B") {
		t.Fatalf("no queued steer:\n%s", h.render())
	}
	close(release)
	h.app.ctrl.Wait()
	got := h.render()
	if !strings.Contains(got, "steer delivered: use plan B") ||
		strings.Contains(got, "steer queued") {
		t.Fatalf("steer not delivered:\n%s", got)
	}
}

func TestScreenPickerChoosesModel(t *testing.T) {
	h := newScreenHarness(t, 0)
	h.submit("/model")
	h.waitFor(func() bool { return h.scr.picker != nil })
	if !strings.Contains(h.render(), "Model") {
		t.Fatalf("no picker:\n%s", h.render())
	}
	h.key(tui.KeyEvent{Key: tui.KeyArrowDown})
	var want string
	h.check(func() bool { want = h.scr.picker.options[1]; return true })
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.app.ctrl.Wait()
	if h.app.eng.Model() != want {
		t.Fatalf("model=%s, want %s", h.app.eng.Model(), want)
	}
	// Esc cancels and keeps the model.
	h.submit("/model")
	h.waitFor(func() bool { return h.scr.picker != nil })
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.app.ctrl.Wait()
	if h.app.eng.Model() != want {
		t.Fatalf("model changed to %s", h.app.eng.Model())
	}
}

func TestScreenResetClearsTranscript(t *testing.T) {
	h := newScreenHarness(t, 0, say("Hi."))
	h.submit("hello")
	h.app.ctrl.Wait()
	h.submit("/new")
	h.app.ctrl.Wait()
	got := h.render()
	if strings.Contains(got, "Hi.") || !strings.Contains(got, "New session") {
		t.Fatalf("transcript not reset:\n%s", got)
	}
}

func TestPromptCancellationCannotClearNewQuestion(t *testing.T) {
	s := newScreen("fixture")
	s.input = "existing draft"
	first := &askPrompt{q: Question{Prompt: "Old question"}, reply: make(chan questionAnswer, 1)}
	second := &askPrompt{q: Question{Prompt: "New question"}, reply: make(chan questionAnswer, 1)}
	s.HandleEvent(promptEvent{id: 1, asking: first})
	s.input = "old answer draft"
	s.HandleEvent(promptEvent{id: 2, asking: second})
	s.HandleEvent(endPromptEvent{id: 1})
	if s.asking != second || s.input != "" {
		t.Fatal("old cancellation changed the new prompt")
	}
	s.HandleEvent(endPromptEvent{id: 2})
	if s.asking != nil || s.input != "existing draft" {
		t.Fatalf("original draft not restored: %q", s.input)
	}
	s.HandleEvent(promptEvent{id: 1, asking: first})
	if s.asking != nil || s.input != "existing draft" {
		t.Fatal("stale question was redisplayed")
	}
}

func TestStatusLineRightAlignsHelpHintWhenItFits(t *testing.T) {
	h := newScreenHarness(t, 0)
	line := func(width int) string {
		h.mu.Lock()
		defer h.mu.Unlock()
		return strings.TrimRight(tui.SprintScreen(h.scr.statusView(), tui.WithWidth(width), tui.WithHeight(1)).Text(), " \n")
	}
	wide := line(160)
	if !strings.HasPrefix(wide, " fixture · ") || !strings.HasSuffix(wide, "/help lists commands") || len([]rune(wide)) != 159 {
		t.Fatalf("wide = %q", wide)
	}
	narrow := line(60)
	if !strings.HasPrefix(narrow, " fixture · ") || strings.Contains(narrow, "/help") {
		t.Fatalf("narrow = %q", narrow)
	}
}

func TestIdleInputHintIsEmpty(t *testing.T) {
	h := newScreenHarness(t, 0)
	got := h.render()
	if strings.Contains(got, "Ask anything") || !strings.Contains(got, "\n\n────") {
		t.Fatalf("idle footer:\n%s", got)
	}
}

func TestFooterBlockRendersAsRule(t *testing.T) {
	f := footerFacts{Turn: 3, Model: "claude-x", Calls: 2, Tokens: 12345, Cost: "$0.0421"}
	b := Block{Kind: KindFooter, Text: f.String(), Footer: &f, Done: true}
	if FormatBlock(b) != "── turn 3 · claude-x · 2 model calls · 12,345 tokens · $0.0421" {
		t.Fatalf("plain = %q", FormatBlock(b))
	}
	got := strings.TrimRight(tui.SprintScreen(blockView(b), tui.WithWidth(70), tui.WithHeight(1)).Text(), "\n")
	if got != "── turn 3 · claude-x · 2 model calls · 12,345 tokens · $0.0421 ───────" {
		t.Fatalf("screen = %q", got)
	}
	styled := tui.Sprint(blockView(b), tui.WithWidth(70))
	for _, want := range []string{"\x1b[0;38;5;248m── turn 3 · ", "\x1b[0mclaude-x", "\x1b[0;38;5;108m$0.0421", "\x1b[0;38;5;248m───"} {
		if !strings.Contains(styled, want) {
			t.Fatalf("missing %q in %q", want, styled)
		}
	}
	f.UsageIncomplete = true
	if styled := tui.Sprint(blockView(b), tui.WithWidth(70)); !strings.Contains(styled, "33musage incomplete") {
		t.Fatalf("incomplete = %q", styled)
	}
}

func TestComposerHighlightsAKnownCommand(t *testing.T) {
	h := newScreenHarness(t, 0)
	fixtureResourceSkill(t, h.dir, "deploy", "Deploy it", "")
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scr.ctrl.commandNames() // fills the cache as a render's refresh would
	for _, tc := range []struct {
		draft string
		end   int
	}{
		{"/effort high", 7},
		{"/EFFORT", 7},
		{"/q", 2},
		{"/deploy prod", 7},
		{"/effort\nmore", 7},
		{"/eff", 0},
		{"/nope", 0},
		{"hello /effort", 0},
		{" /effort", 0},
		{"", 0},
	} {
		got := h.scr.highlightCommand(tc.draft)
		if tc.end == 0 {
			if got != nil {
				t.Errorf("%q highlighted %+v", tc.draft, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Start != 0 || got[0].End != tc.end || got[0].Style != commandHighlightStyle {
			t.Errorf("%q highlighted %+v, want bytes 0-%d", tc.draft, got, tc.end)
		}
	}

	h.scr.asking = &askPrompt{}
	if got := h.scr.highlightCommand("/effort"); got != nil {
		t.Errorf("an answer to a question highlighted %+v", got)
	}
	h.scr.asking = nil
}

// The composer draws the highlight. Its first render refreshes the command
// names in the background, so a later render shows the style.
func TestComposerDrawsTheCommandHighlight(t *testing.T) {
	h := newScreenHarness(t, 0)
	styled := func(draft string) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.scr.input = draft
		screen := tui.SprintScreen(h.scr.View(), tui.WithWidth(80), tui.WithHeight(24))
		for y := range 24 {
			if x := strings.Index(screen.Row(y), "❯ "+draft); x >= 0 {
				x = utf8.RuneCountInString(screen.Row(y)[:x]) + 2
				return screen.Cell(x, y).Style.Bold && screen.Cell(x+4, y).Style.Bold && !screen.Cell(x+6, y).Style.Bold
			}
		}
		t.Fatalf("draft %q not on screen:\n%s", draft, screen.Text())
		return false
	}
	deadline := time.Now().Add(3 * time.Second)
	for !styled("/help me") {
		if time.Now().After(deadline) {
			t.Fatal("/help was not highlighted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if styled("/hel me") {
		t.Fatal("/hel was highlighted")
	}
}
