package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
)

func testConfig(t *testing.T, script ...reply) appConfig {
	t.Helper()
	dir, err := canonicalWorkspace(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewStore(t.TempDir())
	store.DisableSync()
	return appConfig{
		Dir:     dir,
		HomeDir: t.TempDir(),
		Model:   "fixture",
		Client:  &fakeClient{script: script},
		Store:   store,
	}
}

// -p prints only the answer to stdout. The steps go to stderr.
func TestPrintModePrintsOnlyTheAnswer(t *testing.T) {
	cfg := testConfig(t,
		callTool("c1", "list_directory", `{}`),
		say("The answer."),
	)
	var stdout, stderr bytes.Buffer
	code := runPrint(t.Context(), cfg, Options{Print: "go", PrintSet: true}, &stdout, &stderr)
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
	runPrint(t.Context(), cfg, Options{Print: "go", PrintSet: true}, &stdout, &stderr)
	if runs.Load() != 0 || !strings.Contains(stderr.String(), "denied") {
		t.Fatalf("runs=%d stderr=%q", runs.Load(), stderr.String())
	}

	cfg = testConfig(t, script...)
	stdout.Reset()
	stderr.Reset()
	runPrint(t.Context(), cfg, Options{Print: "go", PrintSet: true, Yes: true}, &stdout, &stderr)
	if runs.Load() != 1 {
		t.Fatalf("--yes did not approve: runs=%d", runs.Load())
	}
}

func TestPrintModeExitsNonzeroOnLimit(t *testing.T) {
	cfg := testConfig(t, callTool("c1", "list_directory", `{}`), say("Partial."))
	cfg.ModelAttempts = 2
	var stdout, stderr bytes.Buffer
	code := runPrint(t.Context(), cfg, Options{Print: "go", PrintSet: true}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "limited") {
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
				Print: input, PrintSet: true, Resume: true, ResumeID: saved.ID,
			}, &stdout, &stderr)
			if code != 0 || stderr.Len() != 0 || cfg.Client.(*fakeClient).sends != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if input == "/help" || input == "/?" {
				if !strings.Contains(stdout.String(), "Commands:") {
					t.Fatalf("help missing: %q", stdout.String())
				}
			} else if stdout.Len() != 0 {
				t.Fatalf("quit printed output: %q", stdout.String())
			}
			if release, err := cfg.Store.Lock(saved); !errors.Is(err, session.ErrBusy) {
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
	for _, want := range []string{"Hello back.", "[turn 1 | fixture", "Commands:"} {
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

func TestParseArgs(t *testing.T) {
	tests := []struct {
		args []string
		want Options
	}{
		{nil, Options{}},
		{[]string{"-c"}, Options{Resume: true}},
		{[]string{"--resume"}, Options{Resume: true}},
		{[]string{"--resume", "abc"}, Options{Resume: true, ResumeID: "abc"}},
		{[]string{"--resume=abc"}, Options{Resume: true, ResumeID: "abc"}},
		{[]string{"--resume", "--model", "m"}, Options{Resume: true, Model: "m"}},
		{[]string{"-p", "hi there", "--yes"}, Options{Print: "hi there", PrintSet: true, Yes: true}},
		{[]string{"--model=gpt-6-luna"}, Options{Model: "gpt-6-luna"}},
		{[]string{"--plain", "-h"}, Options{Plain: true, Help: true}},
		{[]string{"--inline"}, Options{Inline: true}},
	}
	for _, tt := range tests {
		got, err := parseArgs(tt.args)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseArgs(%v)=%+v, %v; want %+v", tt.args, got, err, tt.want)
		}
	}
	for _, bad := range [][]string{{"--nope"}, {"-p"}, {"--model"}, {"--plain", "--inline"}} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("parseArgs(%v) succeeded", bad)
		}
	}
}

func TestModelChoiceOrder(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	s := Settings{Model: "from-settings"}
	tests := []struct {
		flag string
		env  map[string]string
		s    Settings
		want string
	}{
		{"flag", map[string]string{"DASHI_MODEL": "env"}, s, "flag"},
		{"", map[string]string{"DASHI_MODEL": "env"}, s, "env"},
		{"", nil, s, "from-settings"},
		{"", nil, Settings{}, "gpt-6-luna"},
	}
	for _, tt := range tests {
		if got := chooseModel(tt.flag, env(tt.env), tt.s); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}

func TestSettingsRepositoryFileWins(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	write := func(root, body string) {
		p := filepath.Join(root, ".dashi")
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(home, `{"model":"user"}`)
	s, warns := loadSettings(home, dir)
	if s.Model != "user" || len(warns) != 0 {
		t.Fatalf("s=%+v warns=%v", s, warns)
	}
	write(dir, `{"model":"repo"}`)
	if s, _ := loadSettings(home, dir); s.Model != "repo" {
		t.Fatalf("s=%+v", s)
	}
	write(dir, `{bad json`)
	s, warns = loadSettings(home, dir)
	if s.Model != "user" || len(warns) != 1 {
		t.Fatalf("s=%+v warns=%v", s, warns)
	}
}
