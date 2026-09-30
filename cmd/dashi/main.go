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
	if o.Models {
		return runModels(o, home, dir, stdout, stderr)
	}
	invocation := dir
	dir, err = canonicalWorkspace(dir, o.Workspace)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	origins, err := loadNamedEnvironment(invocation, o.EnvFiles, os.LookupEnv, os.Setenv)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	user, project, registry, trusted, err := loadDestinationFiles(home, dir)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	store := session.NewStore(filepath.Join(home, ".dashi", "sessions"))
	reopened, err := loadReopenedPreferences(store, dir, o)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	launch, err := resolveLaunchPreferences(o, registry, trusted, user, project, reopened, os.LookupEnv, origins)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	registry[launch.Selection.Destination.ID] = launch.Selection.Destination
	preferences := &cliPreferences{launch: launch, routes: newRouteClients(registry, trusted, os.LookupEnv), client: newTurnClient(nil), writer: userPreferenceWriter{path: filepath.Join(home, ".dashi", "settings.json")}, interactive: !o.PrintSet && isTerminal(os.Stdin), guidance: o.SystemPrompt, guidanceSet: o.SystemPromptSet, environmentOrigins: origins, environmentFiles: namedEnvironmentPaths(invocation, o.EnvFiles)}
	preferences.resolveSession = func(saved preferenceFile) (launchPreferences, error) {
		return resolveLaunchPreferences(o, registry, trusted, user, project, saved, os.LookupEnv, origins)
	}
	cfg := appConfig{Dir: dir, HomeDir: home, InvocationDir: invocation, MCPFiles: o.MCPFiles, Model: launch.Selection.Model, Client: preferences.client, Store: store, Preferences: preferences}
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
	if handled, code := a.ctrl.printCommand(o.Print, stdout, stderr); handled {
		return code
	}
	notice, err := a.openSession(cfg.Store, cfg.Dir, o)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	if notice != "" {
		fmt.Fprintln(stderr, workspace.PrintableText(notice))
	}
	if err := a.initializeMCP(ctx, false); err != nil {
		fmt.Fprintln(stderr, "dashi: MCP initialization canceled or failed; no work was started.")
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
	fmt.Fprintf(stdout, "dashi  %s  %s\n", workspace.PrintableText(a.eng.Model()), workspace.PrintableText(cfg.Dir))
	if notice != "" {
		fmt.Fprintln(stdout, workspace.PrintableText(notice))
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
	interactive := isTerminal(os.Stdin)
	if interactive {
		a.startMCP(true)
	} else if err := a.initializeMCP(ctx, false); err != nil {
		fmt.Fprintln(stderr, "dashi: MCP initialization canceled or failed; no work was started.")
		return 1
	}
	if err := ui.serveLines(stdin, !interactive); err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	if s := a.eng.Session(); a.eng.Saved() {
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
	home := cfg.HomeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	history, entries := openInputHistory(filepath.Join(home, ".dashi", "history.json"), func(err error) { scr.Emit(warnBlock("%v", err)) })
	scr.historyStore, scr.history = history, entries
	scr.pathCompletion = indexWorkspace(cfg.Dir)
	defer history.close()
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
		Text: fmt.Sprintf("dashi  %s  %s", workspace.PrintableText(a.eng.Model()), workspace.PrintableText(cfg.Dir)),
		Done: true,
	})
	if notice != "" {
		scr.Emit(Block{Kind: KindNotice, Text: notice, Done: true})
	}
	a.ctrl.Open()
	var inline *inlineScreen
	if o.Inline {
		inline = newInlineScreen(scr, stdout)
		err = inline.runner.Run(inline)
	} else {
		err = tui.Run(scr, tui.WithBracketedPaste(true), tui.WithMouseDrag(true), tui.WithFPS(30))
	}
	// Stop work that is still running, so the turn records an interruption.
	a.ctrl.Interrupt()
	a.ctrl.Wait()
	history.close()
	scr.drain()
	if inline != nil {
		inline.flushExit(stdout)
	} else {
		scr.dumpTranscript(stdout)
	}
	if s := a.eng.Session(); a.eng.Saved() {
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
