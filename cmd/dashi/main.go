// Command dashi is a terminal coding agent built on the Dashi library.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
)

func main() {
	os.Exit(run(
		context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr,
	))
}

// run is main without the process: it returns the exit code.
func run(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	o, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n\n%s", err, usage)
		return 2
	}
	if o.Help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	dir, err := workDir()
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	settings, warnings := loadSettings(home, dir)
	for _, w := range warnings {
		fmt.Fprintf(stderr, "dashi: warning: %s\n", w)
	}
	model := chooseModel(o.Model, os.Getenv, settings)
	if !knownModel(model) {
		fmt.Fprintf(stderr,
			"dashi: warning: model %q is not in the catalog; using %s\n",
			model, defaultModel)
		model = defaultModel
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		fmt.Fprintln(stderr, "dashi: set OPENAI_API_KEY to talk to the model.")
		return 1
	}
	client, err := responses.New(responses.Config{})
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	store := session.NewStore(filepath.Join(home, ".dashi", "sessions"))
	cfg := appConfig{Dir: dir, Model: model, Client: client, Store: store}
	if o.PrintSet {
		return runPrint(ctx, cfg, o, stdout, stderr)
	}
	if o.Plain || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return runLines(ctx, cfg, o, stdin, stdout, stderr)
	}
	return runScreen(ctx, cfg, o, stdout, stderr)
}

// runPrint runs one turn without a UI and prints the answer to stdout. Steps
// go to stderr.
func runPrint(
	ctx context.Context,
	cfg appConfig,
	o Options,
	stdout, stderr io.Writer,
) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	ui := &printUI{err: stderr, yes: o.Yes}
	a, err := buildApp(ctx, cfg, ui)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	defer a.eng.Close()
	if a.wiring.Close != nil {
		defer a.wiring.Close()
	}
	if _, err := a.openSession(cfg.Store, cfg.Dir, o); err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	res, err := a.eng.Prompt(ctx, o.Print)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	return printAnswer(res.Outcome, stdout, stderr)
}

// printAnswer writes the answer of a settled turn to stdout. Any other stop
// gives the exit code 1 and its reason on stderr.
func printAnswer(out dashi.Outcome, stdout, stderr io.Writer) int {
	if out.Answer != "" {
		answer := out.Answer
		if f, ok := stdout.(*os.File); ok && isTerminal(f) {
			// The answer is the model's text. On a terminal it must not carry
			// escape sequences.
			answer = workspace.PrintableText(answer)
		}
		fmt.Fprintln(stdout, answer)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		fmt.Fprintf(stderr, "dashi: the turn stopped: %s", out.Stop.Kind)
		if out.Stop.Reason != "" {
			fmt.Fprintf(stderr, " (%s)", out.Stop.Reason)
		}
		fmt.Fprintln(stderr)
		return 1
	}
	return 0
}

// runLines runs the session in line mode.
func runLines(
	ctx context.Context,
	cfg appConfig,
	o Options,
	stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	ui := newLineUI(stdout)
	a, err := buildApp(ctx, cfg, ui)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	defer a.eng.Close()
	if a.wiring.Close != nil {
		defer a.wiring.Close()
	}
	ui.ctrl = a.ctrl
	notice, err := a.openSession(cfg.Store, cfg.Dir, o)
	if err != nil {
		if errors.Is(err, session.ErrBusy) {
			fmt.Fprintf(stderr, "dashi: this session is open in another dashi.\n")
		} else {
			fmt.Fprintf(stderr, "dashi: %v\n", err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "dashi  %s  %s\n", a.eng.Model(), cfg.Dir)
	if notice != "" {
		fmt.Fprintln(stdout, notice)
	}
	a.ctrl.Open()
	ui.SetLive()
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	defer signal.Stop(sigc)
	go func() {
		for range sigc {
			if a.ctrl.Interrupt() {
				ui.Quit()
			}
		}
	}()
	if err := ui.serveLines(stdin, !isTerminal(os.Stdin)); err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	if s := a.eng.Session(); len(s.Turns) > 0 {
		fmt.Fprintf(stdout, "Session %s saved. Resume with: dashi --resume %s\n",
			s.ID, s.ID)
	}
	return 0
}

// runScreen runs the session in the full-screen UI.
func runScreen(
	ctx context.Context,
	cfg appConfig,
	o Options,
	stdout, stderr io.Writer,
) int {
	scr := newScreen(cfg.Dir)
	a, err := buildApp(ctx, cfg, scr)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	defer a.eng.Close()
	if a.wiring.Close != nil {
		defer a.wiring.Close()
	}
	scr.bind(a)
	notice, err := a.openSession(cfg.Store, cfg.Dir, o)
	if err != nil {
		if errors.Is(err, session.ErrBusy) {
			fmt.Fprintf(stderr, "dashi: this session is open in another dashi.\n")
		} else {
			fmt.Fprintf(stderr, "dashi: %v\n", err)
		}
		return 1
	}
	scr.Emit(Block{
		Kind: KindNotice,
		Text: fmt.Sprintf("dashi  %s  %s", a.eng.Model(), cfg.Dir),
		Done: true,
	})
	if notice != "" {
		scr.Emit(Block{Kind: KindNotice, Text: notice, Done: true})
	}
	a.ctrl.Open()
	err = tui.Run(scr, tui.WithBracketedPaste(true))
	// Stop work that is still running, so the turn records an interruption.
	a.ctrl.Interrupt()
	a.ctrl.Wait()
	scr.drain()
	scr.dumpTranscript(stdout)
	if s := a.eng.Session(); len(s.Turns) > 0 {
		fmt.Fprintf(stdout, "\nSession %s saved. Resume with: dashi --resume %s\n",
			s.ID, s.ID)
	}
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	return 0
}

// isTerminal reports whether f is a terminal. A variable so tests can change
// it.
var isTerminal = func(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
