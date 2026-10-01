package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/wonton/tui"
)

// Probes run on the actual Wonton event loop, after its focused input has
// handled keys. Tests never call screen.submit or its widget callback.
type runtimeProbe struct{ reply chan runtimeScreenState }

func (runtimeProbe) Timestamp() time.Time { return time.Now() }

type runtimeScreenState struct {
	mode    Mode
	pending bool
	input   string
	text    string
}

type compactionRuntimeScreen struct {
	*screen
	rt    *tui.Runtime
	ready chan struct{}
	once  sync.Once
}

func (s *compactionRuntimeScreen) SetRuntime(rt *tui.Runtime) {
	s.rt = rt
	s.screen.SetRuntime(rt)
}

func (s *compactionRuntimeScreen) View() tui.View {
	view := s.screen.View()
	s.once.Do(func() { close(s.ready) })
	return view
}

func (s *compactionRuntimeScreen) HandleEvent(event tui.Event) []tui.Cmd {
	if p, ok := event.(runtimeProbe); ok {
		var text strings.Builder
		for y := range 24 {
			for x := range 80 {
				text.WriteRune(s.rt.Terminal().Cell(x, y).Char)
			}
			text.WriteByte('\n')
		}
		p.reply <- runtimeScreenState{
			mode: s.ctrl.Mode(), pending: s.eng.HasPending(),
			input: s.input, text: text.String(),
		}
		return nil
	}
	return s.screen.HandleEvent(event)
}

type idleRuntimeInput struct{ done chan struct{} }

func (i *idleRuntimeInput) ReadEvent() (tui.Event, error) { <-i.done; return nil, io.EOF }
func (*idleRuntimeInput) SetPasteTabWidth(int)            {}

func TestRuntimeEnterResumesCompactionAndOriginalOnce(t *testing.T) {
	started, delivered := make(chan struct{}), make(chan struct{})
	cfg := testConfig(t, fullWindow, blockUntilDone(started),
		say("<summary>PRIVATE SUMMARY</summary>"),
		func(ctx context.Context, call llm.Call) (llm.Result, error) {
			close(delivered)
			return say("Original answered.")(ctx, call)
		})
	compactWindow(cfg.Client.(*fakeClient))
	scr := &compactionRuntimeScreen{screen: newScreen(cfg.Dir), ready: make(chan struct{})}
	a, err := buildApp(t.Context(), cfg, scr)
	if err != nil {
		t.Fatal(err)
	}
	scr.bind(a)
	if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.eng.Close)
	if _, err := a.eng.Prompt(t.Context(), compactionSeed()); err != nil {
		t.Fatal(err)
	}
	scr.Reset() // Keep the durable seed while showing the recovery workflow.
	rt := tui.NewRuntime(tui.NewTestTerminal(80, 24, io.Discard), scr, 60)
	input := &idleRuntimeInput{done: make(chan struct{})}
	rt.SetInputSource(input)
	done := make(chan error, 1)
	go func() { done <- rt.Run() }()
	t.Cleanup(func() {
		a.ctrl.Interrupt()
		a.ctrl.Wait()
		rt.Stop()
		close(input.done)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runtime: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("runtime did not stop")
		}
	})
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("runtime workflow timed out")
		}
	}
	wait(scr.ready)
	var frames []string
	t.Cleanup(func() {
		if dir := os.Getenv("DASHI_COMPACTION_EVIDENCE"); dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Error(err)
				return
			}
			var cast strings.Builder
			cast.WriteString("{\"version\":2,\"width\":80,\"height\":24,\"title\":\"Dashi Wonton compaction fixture (slowed checkpoints)\"}\n")
			for i, frame := range frames {
				row, err := json.Marshal([]any{i * 3, "o", "\x1b[2J\x1b[H" + frame})
				if err != nil {
					t.Error(err)
					return
				}
				cast.Write(row)
				cast.WriteByte('\n')
			}
			if err := os.WriteFile(filepath.Join(dir, "runtime-enter.cast"), []byte(cast.String()), 0644); err != nil {
				t.Error(err)
			}
		}
	})
	probe := func(want func(runtimeScreenState) bool) runtimeScreenState {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			reply := make(chan runtimeScreenState, 1)
			rt.SendEvent(runtimeProbe{reply: reply})
			select {
			case got := <-reply:
				if want(got) {
					frames = append(frames, got.text)
					return got
				}
			case <-deadline.C:
				t.Fatal("runtime probe timed out")
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				t.Fatal("runtime state did not arrive")
			}
		}
	}
	probe(func(s runtimeScreenState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "Type a message...")
	})
	for _, r := range "original prompt" {
		rt.SendEvent(tui.KeyEvent{Key: tui.KeyUnknown, Rune: r})
	}
	rt.SendEvent(tui.KeyEvent{Key: tui.KeyEnter})
	wait(started)
	probe(func(s runtimeScreenState) bool {
		return strings.Contains(s.text, "new text is queued") && strings.Contains(s.text, "pending prompt: original prompt")
	})
	rt.SendEvent(tui.KeyEvent{Key: tui.KeyCtrlC})
	probe(func(s runtimeScreenState) bool {
		return s.mode == ModeIdle && s.pending && strings.Contains(s.text, "Press Enter to continue")
	})
	rt.SendEvent(tui.KeyEvent{Key: tui.KeyEnter})
	wait(delivered)
	a.ctrl.Wait()
	final := probe(func(s runtimeScreenState) bool {
		return s.mode == ModeIdle && !s.pending && strings.Contains(s.text, "Type a message...") && !strings.Contains(s.text, "Working.")
	})
	if final.input != "" || strings.Contains(final.text, "PRIVATE SUMMARY") ||
		strings.Contains(final.text, "Type to steer") {
		t.Fatalf("wrong resumed input/render state:\n%s", final.text)
	}
	h := &harness{t: t, app: a, dir: cfg.Dir, store: cfg.Store}
	if h.turns() != 3 || joinMessages(h.turn(3).Steps[0].Started.Input) != "original prompt" {
		t.Fatal("Enter lost or duplicated the retained prompt")
	}
}
