package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/wonton/tui"
)

func TestRuntimeActiveContextInterruptReopenContinue(t *testing.T) {
	cfg, c := activeFixtureConfig(t, true)
	h := newTranscriptRuntimeConfig(t, cfg)
	h.typeText(activeObjective)
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	for range 2 {
		h.wait(func(s transcriptState) bool { return s.approval })
		h.key(tui.KeyEvent{Rune: 'y'})
	}
	select {
	case <-c.started:
	case <-time.After(3 * time.Second):
		t.Fatal("active summary did not start")
	}
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Compacting this Turn") })
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "Press Enter to continue")
	})
	id := h.app.eng.Session().ID
	// /open reloads authoritative file-recorder steps and reconstructs markers.
	h.app.ctrl.openSession(id)
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "no replacement applied")
	})
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return s.mode == ModeRunning })
	h.app.ctrl.Wait()
	final := h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "Fixed the off-by-one")
	})
	if strings.Contains(final.text, activePrivateSummary) {
		t.Fatal("summary shown as answer")
	}
	turn := activeAssertCompleted(t, cfg, h.app, c)
	applied := 0
	for _, s := range turn.Steps {
		if s.Kind == dashi.StepContextCompacted {
			applied++
		}
	}
	if applied < 2 {
		t.Fatalf("applied=%d", applied)
	}
	c.mu.Lock()
	sends := c.sends
	c.mu.Unlock()
	h.app.ctrl.openSession(id)
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "context compacted:")
	})
	c.mu.Lock()
	after := c.sends
	c.mu.Unlock()
	if sends != after {
		t.Fatal("completed replay made model calls")
	}
	exportActiveRuntime(t, h, "interrupt-reopen-continue")
}
func TestRuntimeActiveContextDiscardKeepsFinishedEffects(t *testing.T) {
	cfg, c := activeFixtureConfig(t, true)
	h := newTranscriptRuntimeConfig(t, cfg)
	h.typeText(activeObjective)
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	for range 2 {
		h.wait(func(s transcriptState) bool { return s.approval })
		h.key(tui.KeyEvent{Rune: 'y'})
	}
	select {
	case <-c.started:
	case <-time.After(3 * time.Second):
		t.Fatal("active summary did not start")
	}
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Compacting this Turn") })
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "Press Enter to continue")
	})
	h.typeText("/discard")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	// Enter is queued on the terminal event loop. A probe acknowledges its
	// submission before Wait can race with the controller adding the work.
	h.wait(func(s transcriptState) bool { return s.input == "" })
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool { return s.mode == ModeIdle && strings.Contains(s.text, "canceled") })
	view, err := h.turn(1).View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Stop.Kind != dashi.StopCanceled || len(view.ContextCompactions) != 0 {
		t.Fatalf("partial summary applied or cancellation lost: %+v", view)
	}
	if got, err := os.ReadFile(filepath.Join(cfg.Dir, "checks.log")); err != nil || string(got) != "CHECK_OK\n" {
		t.Fatalf("finished check lost %q %v", got, err)
	}
	id := h.app.eng.Session().ID
	c.mu.Lock()
	before := c.sends
	c.mu.Unlock()
	h.app.ctrl.openSession(id)
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool { return s.mode == ModeIdle && strings.Contains(s.text, "canceled") })
	c.mu.Lock()
	after := c.sends
	c.mu.Unlock()
	if before != after {
		t.Fatal("canceled reopen dispatched work")
	}
	exportActiveRuntime(t, h, "discard-reopen")
}
func exportActiveRuntime(t *testing.T, h *runtimeFixture, name string) {
	t.Helper()
	dir := os.Getenv("DASHI_ACTIVE_CONTEXT_EVIDENCE")
	if dir == "" {
		return
	}
	// Reuse the renderer's actual Wonton 80x24 cells; frames are slowed 3s for review.
	t.Setenv("DASHI_TRANSCRIPT_EVIDENCE", dir)
	h.exportFrames()
	if err := os.WriteFile(filepath.Join(dir, name+"-frames.txt"), []byte(strings.Join(h.frames, "\n\n--- checkpoint ---\n\n")), 0600); err != nil {
		t.Fatal(err)
	}
}
