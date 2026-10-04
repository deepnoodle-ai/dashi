package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
)

func testConfig(t *testing.T, script ...reply) appConfig {
	t.Helper()
	dir, err := canonicalWorkspace(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	store := sessionstore.NewStore(t.TempDir())
	store.DisableSync()
	return appConfig{
		Dir:     dir,
		HomeDir: t.TempDir(),
		Model:   "fixture",
		Client:  &fakeClient{script: script},
		Store:   store,
	}
}

// The run command prints only the answer to stdout. The steps go to stderr.
func TestPrintModePrintsOnlyTheAnswer(t *testing.T) {
	cfg := testConfig(t,
		callTool("c1", "list_directory", `{}`),
		say("The answer."),
	)
	var stdout, stderr bytes.Buffer
	code := runPrint(t.Context(), cfg, Options{Prompt: "go", Run: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if stdout.String() != "The answer.\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "list_directory") ||
		strings.Contains(stderr.String(), "The answer.") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestPrintModeDeniesApprovalUnlessYes(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	script := []reply{
		callTool("c1", "write_note", `{"text":"x"}`),
		say("Done."),
	}
	cfg := testConfig(t, script...)
	var stdout, stderr bytes.Buffer
	runPrint(t.Context(), cfg, Options{Prompt: "go", Run: true}, &stdout, &stderr)
	if runs.Load() != 0 || !strings.Contains(stderr.String(), "denied") {
		t.Fatalf("runs=%d stderr=%q", runs.Load(), stderr.String())
	}

	cfg = testConfig(t, script...)
	stdout.Reset()
	stderr.Reset()
	runPrint(t.Context(), cfg, Options{Prompt: "go", Run: true, Yes: true}, &stdout, &stderr)
	if runs.Load() != 1 {
		t.Fatalf("--yes did not approve: runs=%d", runs.Load())
	}
}

func TestPrintModeExitsNonzeroOnLimit(t *testing.T) {
	cfg := testConfig(t, callTool("c1", "list_directory", `{}`), say("Partial."))
	cfg.ModelCalls = 2
	var stdout, stderr bytes.Buffer
	code := runPrint(t.Context(), cfg, Options{Prompt: "go", Run: true}, &stdout, &stderr)
	if code != 4 || !strings.Contains(stderr.String(), "limited") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestPrintLocalCommandsIgnoreBusyResumeSession(t *testing.T) {
	for _, input := range []string{"/help", "/?", "/quit", "/q", "/exit"} {
		t.Run(input, func(t *testing.T) {
			cfg := testConfig(t)
			saved, err := cfg.Store.Create(cfg.Dir, cfg.Model)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.Store.Save(saved); err != nil {
				t.Fatal(err)
			}
			unlock, err := cfg.Store.Lock(saved)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			var stdout, stderr bytes.Buffer
			code := runPrint(t.Context(), cfg, Options{
				Prompt: input, Run: true, Resume: true, ResumeID: saved.ID,
			}, &stdout, &stderr)
			if code != 0 || stderr.Len() != 0 || cfg.Client.(*fakeClient).sends != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if input == "/help" || input == "/?" {
				if !strings.Contains(stdout.String(), "also works while a turn runs") {
					t.Fatalf("help missing: %q", stdout.String())
				}
			} else if stdout.Len() != 0 {
				t.Fatalf("quit printed output: %q", stdout.String())
			}
			if release, err := cfg.Store.Lock(saved); !errors.Is(err, sessionstore.ErrBusy) {
				if release != nil {
					release()
				}
				t.Fatalf("local command released the existing lock: %v", err)
			}
		})
	}
}

func TestLineModeRunsCommandsFromInput(t *testing.T) {
	cfg := testConfig(t, say("Hello back."))
	var out bytes.Buffer
	ui := newLineUI(&out)
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	ui.ctrl = a.ctrl
	if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	a.ctrl.Open()
	ui.SetLive()
	if err := ui.serveLines(strings.NewReader("hello\n/help\n"), true); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Hello back.", "turn 1 · fixture", "also works while a turn runs"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// The user's own line is not echoed back.
	if strings.Contains(got, "> hello") {
		t.Fatalf("echoed the user line:\n%s", got)
	}
}

func TestLineModeApprovalReadsAnswerLine(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	cfg := testConfig(t,
		callTool("c1", "write_note", `{"text":"x"}`),
		say("Noted."),
	)
	var out bytes.Buffer
	ui := newLineUI(&out)
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	ui.ctrl = a.ctrl
	if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	ui.SetLive()
	pr, pw, _ := os.Pipe()
	go func() {
		pw.WriteString("write a note\n")
		// Wait until the prompt shows before answering.
		for !strings.Contains(safeString(&out, ui), "Allow once") {
		}
		pw.WriteString("y\n")
		pw.Close()
	}()
	if err := ui.serveLines(pr, false); err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 1 {
		t.Fatalf("runs=%d output:\n%s", runs.Load(), out.String())
	}
}

func TestPipedInputAnswersApprovals(t *testing.T) {
	for _, tt := range []struct {
		answer string
		runs   int64
	}{{"y", 1}, {"n", 0}} {
		t.Run(tt.answer, func(t *testing.T) {
			var runs atomicCounter
			withNoteTool(t, &runs)
			cfg := testConfig(t,
				callTool("c1", "write_note", `{"text":"x"}`),
				say("Noted."),
			)
			var out bytes.Buffer
			ui := newLineUI(&out)
			a, err := buildApp(t.Context(), cfg, ui)
			if err != nil {
				t.Fatal(err)
			}
			defer a.eng.Close()
			ui.ctrl = a.ctrl
			if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
				t.Fatal(err)
			}
			ui.SetLive()
			in := strings.NewReader("write a note\n" + tt.answer + "\n")
			if err := ui.serveLines(in, true); err != nil {
				t.Fatal(err)
			}
			if got := int64(runs.Load()); got != tt.runs {
				t.Fatalf("runs=%d output:\n%s", got, out.String())
			}
		})
	}
}

func TestPipedEOFDuringApprovalCancelsRun(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	cfg := testConfig(t,
		callTool("c1", "write_note", `{"text":"x"}`),
		say("Noted."),
	)
	var out bytes.Buffer
	ui := newLineUI(&out)
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	ui.ctrl = a.ctrl
	if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	ui.SetLive()
	done := make(chan error, 1)
	go func() {
		done <- ui.serveLines(strings.NewReader("write a note\n"), true)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveLines hangs at EOF during an approval")
	}
	if runs.Load() != 0 {
		t.Fatalf("the tool ran without approval; output:\n%s", out.String())
	}
}

func safeString(b *bytes.Buffer, ui *lineUI) string {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	return b.String()
}
