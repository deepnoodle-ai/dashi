package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/wonton/tui"
)

func TestCompactBusyRecoveryInstructions(t *testing.T) {
	for _, tc := range []struct {
		next dashi.Next
		want string
	}{
		{dashi.NextContinue, "Press Enter to continue"},
		{dashi.NextResume, "Press Enter to continue"},
		{dashi.NextRaiseAttempts, "Type /continue"},
	} {
		got := busyText(&session.TurnBusyError{TurnID: "session-2", Next: tc.next})
		compactionMustHave(t, got, tc.want, "/discard")
	}
	started := make(chan struct{})
	h := newScreenHarness(t, 0, blockUntilDone(started), say("Resumed."))
	h.submit("ordinary prompt")
	<-started
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.app.ctrl.Wait()
	h.submit("/compact")
	h.app.ctrl.Wait()
	compactionMustHave(t, h.render(), "Press Enter to continue", "/discard")
	if h.turns() != 1 {
		t.Fatal("busy compaction replaced the open ordinary turn")
	}
}

func TestModelSwitchRefreshesAutomaticCompactionWindow(t *testing.T) {
	selected := func(r reply) reply {
		return func(ctx context.Context, call llm.Call) (llm.Result, error) {
			result, err := r(ctx, call)
			result.Requested = llm.ModelRef{Provider: "test", Model: call.Request.Snapshot().Input.Model}
			return result, err
		}
	}
	h := newHarness(t, fullWindow, selected(fullWindow),
		selected(say("<summary>Preserved project fact.</summary>")), selected(say("Small-window answer.")))
	compactWindow(h.client)
	h.input(compactionSeed())
	models := modelNames()
	if len(models) < 2 {
		t.Fatal("fixture needs two catalog models")
	}
	h.client.mu.Lock()
	h.client.window = 128000
	h.client.mu.Unlock()
	h.input("/model " + models[0])
	h.input("large-window prompt")
	if h.turns() != 2 || h.client.inputAt(1).Model != models[0] {
		t.Fatal("model switch retained the old compaction threshold")
	}
	compactWindow(h.client)
	h.input("/model " + models[1])
	h.input("small-window prompt")
	if h.turns() != 4 || h.client.inputAt(2).Model != models[1] ||
		h.client.inputAt(3).Model != models[1] {
		t.Fatalf("turns=%d summary model=%q prompt model=%q selected=%q notices=%q", h.turns(), h.client.inputAt(2).Model, h.app.eng.Model(), models[1], h.ui.text()[max(0, len(h.ui.text())-800):])
	}
	compactionMustHave(t, h.ui.text(), "Compaction completed.", "Small-window answer.")
}

func TestUnknownCompactionWindowWarnsOncePerModel(t *testing.T) {
	h := newHarness(t, say("One."), say("Two."), say("Three."))
	h.client.mu.Lock()
	h.client.window = -1
	h.client.mu.Unlock()
	h.input("first")
	h.input("/compact")
	h.input("second")
	if got := strings.Count(h.ui.text(), "has no known context window"); got != 1 {
		t.Fatalf("fallback warning repeated: %d", got)
	}
	h.input("/model " + modelNames()[0])
	h.input("third")
	if got := strings.Count(h.ui.text(), "has no known context window"); got != 2 {
		t.Fatalf("new model did not get one warning: %d", got)
	}
	compactionMustHave(t, h.ui.text(), "128,000 tokens")
}

func TestCompactionPrintAndPlainOutputBoundaries(t *testing.T) {
	for _, mode := range []string{"print", "plain"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig(t, fullWindow,
				say("<summary>PRIVATE SUMMARY</summary>"), say("Visible answer."))
			compactWindow(cfg.Client.(*fakeClient))
			seed, err := buildApp(t.Context(), cfg, newRecorderUI())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := seed.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
				t.Fatal(err)
			}
			if _, err := seed.eng.Prompt(t.Context(), compactionSeed()); err != nil {
				t.Fatal(err)
			}
			seed.eng.Close()
			var out, diagnostics bytes.Buffer
			opts := Options{Resume: true}
			var code int
			if mode == "print" {
				opts.Print, opts.PrintSet = "actual request", true
				code = runPrint(t.Context(), cfg, opts, &out, &diagnostics)
				if out.String() != "Visible answer.\n" {
					t.Fatalf("print stdout contains internal output: %q", out.String())
				}
				compactionMustHave(t, diagnostics.String(), "Compacting history", "Compaction completed.")
			} else {
				opts.Plain = true
				code = runLines(t.Context(), cfg, opts,
					strings.NewReader("actual request\n"), &out, &diagnostics)
				compactionMustHave(t, out.String(), "Compacting history", "Compaction completed.", "Visible answer.")
			}
			if code != 0 || strings.Contains(out.String()+diagnostics.String(), "PRIVATE SUMMARY") ||
				strings.Contains(out.String()+diagnostics.String(), "<conversation>") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), diagnostics.String())
			}
		})
	}
}
