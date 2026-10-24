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

	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/decide/defaultclient"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/tui"
)

func main() {
	os.Exit(run(
		context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr,
	))
}

// run is main without the process: it returns the exit code. A command-line
// error exits 2.
func run(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	code := 0
	app := newApp(stdout, stderr, func(o Options) error {
		code = runOptions(ctx, o, stdin, stdout, stderr)
		return nil
	})
	if err := app.ExecuteContext(ctx, args); err != nil && !cli.IsHelpRequested(err) {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 2
	}
	return code
}

// runOptions runs the command that o describes and returns its exit code.
func runOptions(
	ctx context.Context,
	o Options,
	stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	dir, err := workDir()
	if err != nil {
		if automationOneShot(o, stdin) {
			return automationPreflightFailure(o, stdout, stderr, err)
		}
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		if automationOneShot(o, stdin) {
			return automationPreflightFailure(o, stdout, stderr, err)
		}
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	if o.Models {
		return runModels(o, home, dir, stdout, stderr)
	}
	invocation := dir
	dir, err = canonicalWorkspace(dir, o.Workspace)
	if err != nil {
		if automationOneShot(o, stdin) {
			return automationPreflightFailure(o, stdout, stderr, err)
		}
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	if automationOneShot(o, stdin) {
		if o.TurnEvents != "" {
			// Redirected input makes one request, which takes no next
			// prompt, so the file would get no events.
			return automationPreflightFailure(o, stdout, stderr, errors.New("--turn-events needs an interactive session; with redirected input, add --plain"))
		}
		return runAutomation(ctx, o, stdin, stdout, stderr, dir, home, sessionstore.NewStore(filepath.Join(home, ".dashi", "sessions")), func() (appConfig, error) {
			return loadCLIConfig(o, home, dir, invocation, stdin, stderr)
		})
	}
	cfg, err := loadCLIConfig(o, home, dir, invocation, stdin, stderr)
	if err != nil {
		fmt.Fprintln(stderr, workspace.PrintableText("dashi: "+sanitizeAutomationDiagnostic(err.Error())))
		return 1
	}

	if o.TurnEvents != "" {
		cfg.TurnEvents, err = openTurnEvents(o.TurnEvents, dir)
		if err != nil {
			fmt.Fprintf(stderr, "dashi: --turn-events: %v\n", err)
			return 1
		}
		defer cfg.TurnEvents.Close()
	}
	if o.Plain || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return runLines(ctx, cfg, o, stdin, stdout, stderr)
	}
	return runScreen(ctx, cfg, o, stdout, stderr)
}

// runPrint runs the run command against a prepared configuration, for tests.
func runPrint(ctx context.Context, cfg appConfig, o Options, stdout, stderr io.Writer) int {
	return runAutomation(ctx, o, nil, stdout, stderr, cfg.Dir, cfg.HomeDir, cfg.Store, func() (appConfig, error) { return cfg, nil })
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
	ui.attachmentErrors = stderr
	a, err := buildApp(ctx, cfg, ui)
	if err != nil {
		fmt.Fprintf(stderr, "dashi: %v\n", err)
		return 1
	}
	defer func() {
		if err := a.eng.closeTasks(); err != nil {
			a.eng.warn("%v", err)
			return
		}
		a.eng.Close()
		if a.wiring.Close != nil {
			_ = a.wiring.Close()
		}
	}()
	ui.ctrl = a.ctrl
	notice, err := a.openSession(cfg.Store, cfg.Dir, o)
	if err != nil {
		if errors.Is(err, sessionstore.ErrBusy) {
			fmt.Fprintf(stderr, "dashi: this session is open in another dashi.\n")
		} else {
			fmt.Fprintf(stderr, "dashi: %v\n", err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "dashi  %s  %s\n", workspace.PrintableText(a.modelLabel()), workspace.PrintableText(cfg.Dir))
	if notice != "" {
		fmt.Fprintln(stdout, workspace.PrintableText(notice))
	}
	if w := cfg.DecideSetup.warning(); w != "" {
		a.ctrl.emit(warnBlock("%s", w))
	}
	a.ctrl.Open()
	ui.SetLive()
	startupCtx, cancelStartup := context.WithCancel(ctx)
	defer cancelStartup()
	signalDone := make(chan struct{})
	defer close(signalDone)
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	defer signal.Stop(sigc)
	go func() {
		for {
			select {
			case <-signalDone:
				return
			case <-sigc:
				cancelStartup()
				if a.ctrl.Interrupt() {
					ui.Quit()
				}
			}
		}
	}()
	interactive := isTerminal(os.Stdin)
	if interactive {
		a.startMCP(true)
	} else if err := a.initializeMCP(startupCtx, false); err != nil {
		fmt.Fprintln(stderr, "dashi: MCP initialization canceled or failed; no work was started.")
		return 1
	}
	if !interactive && startupCtx.Err() != nil {
		fmt.Fprintln(stderr, "dashi: MCP initialization canceled; no work was started.")
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
	defer func() {
		if err := a.eng.closeTasks(); err != nil {
			a.eng.warn("%v", err)
			return
		}
		a.eng.Close()
		if a.wiring.Close != nil {
			_ = a.wiring.Close()
		}
	}()
	scr.bind(a)
	scr.noColor = !colorFor(stdout)
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
		if errors.Is(err, sessionstore.ErrBusy) {
			fmt.Fprintf(stderr, "dashi: this session is open in another dashi.\n")
		} else {
			fmt.Fprintf(stderr, "dashi: %v\n", err)
		}
		return 1
	}
	scr.Emit(startupBlock(cfg.Dir))
	if notice != "" {
		scr.Emit(Block{Kind: KindNotice, Text: notice, Done: true})
	}
	if w := cfg.DecideSetup.warning(); w != "" {
		scr.Emit(warnBlock("%s", w))
	}
	a.ctrl.Open()
	var inline *inlineScreen
	if o.Inline {
		inline = newInlineScreen(scr, stdout)
		err = inline.runner.Run(inline)
	} else {
		err = tui.Run(scr, tui.WithBracketedPaste(true), tui.WithMouseDrag(true), tui.WithFPS(30))
	}
	scr.cancelAttachmentSelections()
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

func loadCLIConfig(o Options, home, dir, invocation string, stdin io.Reader, stderr io.Writer) (appConfig, error) {
	origins, err := loadNamedEnvironment(invocation, o.EnvFiles, os.LookupEnv, os.Setenv)
	if err != nil {
		return appConfig{}, err
	}
	user, project, registry, trusted, err := loadDestinationFiles(home, dir)
	if err != nil {
		return appConfig{}, err
	}
	warnPreferenceSources(stderr, user, project)
	warnEnvironmentSettings(stderr, origins)
	envDecide, envDecideSet := os.LookupEnv("DASHI_DECIDE")
	decideOn, err := resolveDecide(stderr, o.Decide, o.DecideSet, envDecide, envDecideSet, user)
	if err != nil {
		return appConfig{}, err
	}
	warnDecideSettings(stderr, project)
	unattended := automationOneShot(o, stdin)
	decideReady, err := loadDecideSetup(decideOn, unattended, o.Yes, defaultclient.Load)
	if err != nil {
		return appConfig{}, err
	}
	if w := decideReady.warning(); w != "" && unattended {
		// An interactive session shows it in the transcript instead.
		fmt.Fprintf(stderr, "dashi: warning: %s\n", w)
	}
	store := sessionstore.NewStore(filepath.Join(home, ".dashi", "sessions"))
	reopened, err := loadReopenedPreferences(store, dir, o)
	if err != nil {
		return appConfig{}, err
	}
	launch, err := resolveLaunchPreferences(o, registry, trusted, user, project, reopened, os.LookupEnv)
	if err != nil {
		return appConfig{}, err
	}
	if launch.SelectionError == "" {
		registry[launch.Selection.Destination.ID] = launch.Selection.Destination
	}
	preferences := &cliPreferences{launch: launch, routes: newRouteClients(registry, trusted, os.LookupEnv), client: newTurnClient(nil), writer: userPreferenceWriter{path: filepath.Join(home, ".dashi", "settings.json")}, interactive: !o.Run && !automationRedirected(stdin), guidance: o.SystemPrompt, guidanceSet: o.SystemPrompt != "", environmentOrigins: origins, environmentFiles: namedEnvironmentPaths(invocation, o.EnvFiles)}
	preferences.resolveSession = func(saved preferenceFile) (launchPreferences, error) {
		return resolveLaunchPreferences(o, registry, trusted, user, project, saved, os.LookupEnv)
	}
	cfg := appConfig{Dir: dir, HomeDir: home, InvocationDir: invocation, MCPFiles: o.MCPFiles, Model: launch.Selection.Model, Client: preferences.client, Store: store, Preferences: preferences, ApproveAll: o.Yes, CompactionThreshold: o.CompactionThreshold, Decide: decideOn, DecideSetup: decideReady}
	return cfg, nil
}
