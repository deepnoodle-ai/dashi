package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/workspace"
)

func automationRedirected(in io.Reader) bool {
	if in == nil {
		return false
	}
	if f, ok := in.(*os.File); ok {
		terminal, err := automationProcessTerminal(f)
		return err != nil || !terminal
	}
	return true
}
func automationOneShot(o Options, in io.Reader) bool {
	return o.PrintSet || o.OutputFormatSet || !o.Plain && automationRedirected(in)
}

// Live setup stays lazy. Completed and blocked saved-response reads use no
// environment files, preferences, clients, tools, MCP, or attachment resolver.
func runAutomation(ctx context.Context, o Options, stdin io.Reader, stdout, stderr io.Writer, dir, home string, store *session.Store, live func() (appConfig, error)) (code int) {
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var signalCode atomic.Int32
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	signalDone := make(chan struct{})
	defer close(signalDone)
	go func() {
		for {
			select {
			case s := <-signals:
				c := int32(130)
				if s == syscall.SIGTERM {
					c = 143
				}
				signalCode.CompareAndSwap(0, c)
				cancel()
			case <-signalDone:
				return
			}
		}
	}()
	defer func() {
		if c := signalCode.Load(); c != 0 {
			code = int(c)
		}
	}()
	format := o.OutputFormat
	if format == "" {
		format = "text"
	}
	adaptedOut := stdout
	var closeOut io.Closer
	if f, ok := stdout.(*os.File); ok {
		var err error
		adaptedOut, closeOut, err = prepareAutomationProcessOutput(f)
		if err != nil {
			fmt.Fprintln(stderr, "dashi: stdout is unusable; no work was started.")
			return 1
		}
		if closeOut != nil {
			defer closeOut.Close()
		}
	}
	// Output outlives execution cancellation, so recorded partial content can
	// still be delivered after a signal under the checked write deadline.
	output, err := newAutomationOutput(context.Background(), format, adaptedOut)
	if err != nil {
		fmt.Fprintln(stderr, "dashi: stdout is unusable; no work was started.")
		return 1
	}
	if f, ok := stdout.(*os.File); ok {
		output.writer.terminal, _ = automationProcessTerminal(f)
	}
	var saved *session.Session
	var unlock func()
	var closeRecords func()
	var releaseApp func()
	defer func() {
		if err := output.Finish(); err != nil {
			fmt.Fprintln(stderr, "dashi: response delivery failed or is incomplete. Saved work may remain; reopen the saved session and inspect exact targets before recovery. Do not repeat effects to reproduce output.")
			code = automationCode(int(signalCode.Load()), true, false, nil)
		}
		if releaseApp != nil {
			releaseApp()
		}
		if closeRecords != nil {
			closeRecords()
		}
		if unlock != nil {
			unlock()
		}
	}()
	diagnostic := func(text string) {
		fmt.Fprintln(stderr, workspace.PrintableText("dashi: "+sanitizeAutomationDiagnostic(text)))
	}
	input := stdin
	var closeIn io.Closer
	if f, ok := stdin.(*os.File); ok {
		input, closeIn, err = prepareAutomationProcessInput(f)
		if err != nil {
			diagnostic(err.Error())
			return 2
		}
		if closeIn != nil {
			defer closeIn.Close()
		}
	}
	literal, err := readAutomationStdin(execCtx, input, closeIn)
	if err != nil {
		diagnostic(err.Error())
		if execCtx.Err() != nil {
			return 4
		}
		return 2
	}
	payload, err := combineAutomationInput(o.Print, literal, o.Resume)
	if err != nil {
		diagnostic(err.Error())
		return 2
	}
	if strings.HasPrefix(strings.TrimSpace(o.Print), "/") {
		c := &Controller{eng: NewEngine(EngineConfig{Dir: dir}), ui: &printUI{err: stderr}}
		commandOut := stdout
		if format != "text" {
			commandOut = stderr
		}
		if handled, code := c.printCommand(o.Print, commandOut, stderr); handled {
			return code
		}
	}
	var snap automationSnapshot
	if o.Resume {
		var reader interface {
			dashi.Loader
			dashi.SessionIndex
			Close() error
		}
		var openErr error
		s, r, u, openErr := openAutomationSaved(execCtx, store, dir, o)
		saved = s
		if openErr != nil {
			diagnostic(openErr.Error())
			return 1
		}
		reader, unlock = r, u
		closeRecords = func() { reader.Close() }
		snap, err = loadAutomationSnapshot(execCtx, reader, saved.ID)
		if err != nil {
			diagnostic(err.Error())
			return 1
		}
		ready := snap.canContinue()
		if payload == "" && (!ready || snap.latest.Next() == dashi.NextNone && !snap.missingNotice && len(snap.affected) == 0) {
			output.source(func(ctx context.Context, n int) (dashi.TurnView, error) {
				turn, err := reader.Load(ctx, dashi.TurnID(saved.ID, n))
				if err != nil {
					return dashi.TurnView{}, err
				}
				return dashi.Replay(turn)
			}, cancel)
			if snap.answer.ID != "" {
				output.selectTurn(turnNumber(snap.answer.ID))
			}
			printAutomationAttention(stderr, snap.affected, snap.latest.ID)
			return automationCode(0, false, false, snap.affected)
		}
		if payload != "" && snap.blocksNew() {
			printAutomationAttention(stderr, snap.affected, snap.latest.ID)
			diagnostic("previous parent/child work blocks a new request. Reopen and inspect /tasks and the exact parent before recovery.")
			if len(snap.affected) == 0 {
				return 3
			}
			return automationCode(0, false, false, snap.affected)
		}
	}

	if execCtx.Err() != nil {
		diagnostic("request canceled before admission")
		return 4
	}
	var selected *submittedInput
	if len(o.AttachFiles) > 0 || len(attachmentPaths(o.Print)) > 0 {
		selected, err = printAttachmentInput(execCtx, dir, o.Print, o.AttachFiles)
		if err != nil {
			diagnostic(err.Error())
			return 1
		}
		if literal != "" {
			suffix := literal
			if selected.displayText != "" {
				suffix = "\n\n" + literal
			}
			selected.displayText += suffix
			if len(selected.message.Content) == 0 {
				selected.message.Content = []llm.Content{llm.Text(selected.displayText)}
			} else {
				selected.message.Content[0] = llm.Text(selected.displayText)
			}
		}
	}
	cfg, err := live()
	if err != nil {
		diagnostic(err.Error())
		return 1
	}
	cfg.parentStopped = output.selectTurn

	ui := &printUI{err: &automationDiagnosticWriter{stderr}, yes: o.Yes}
	a, err := buildApp(execCtx, cfg, ui)
	if err != nil {
		diagnostic(err.Error())
		return 1
	}
	a.ctrl.mu.Lock()
	a.ctrl.autoTaskDelivery = false
	a.ctrl.mu.Unlock()
	defer func() {
		cancel()
		if err := a.eng.closeTasks(); err != nil {
			diagnostic(err.Error())
			code = automationCode(int(signalCode.Load()), true, false, nil)
			return
		}

	}()
	releaseApp = func() {
		a.eng.Close()
		if a.wiring.Close != nil {
			if err := a.wiring.Close(); err != nil {
				diagnostic("tool shutdown failed")
				code = automationCode(int(signalCode.Load()), true, false, nil)
			}
		}
	}
	if saved != nil {
		held := unlock
		unlock = nil
		err = a.eng.openOwned(saved, held)
	} else {
		var notice string
		notice, err = a.openSession(store, dir, o)
		if notice != "" {
			diagnostic(notice)
		}
	}
	if err != nil {
		diagnostic(err.Error())
		return 1
	}
	s := a.eng.Session()
	loader := a.eng.handle().Runner.Recorder.(dashi.Loader)
	output.source(func(ctx context.Context, n int) (dashi.TurnView, error) {
		turn, err := loader.Load(ctx, dashi.TurnID(s.ID, n))
		if err != nil {
			return dashi.TurnView{}, err
		}
		return dashi.Replay(turn)
	}, cancel)
	if err = a.initializeMCP(execCtx, false); err != nil {
		diagnostic("MCP initialization canceled or failed; no work was started")
		return 1
	}
	var result Result
	if payload == "" {
		last := a.eng.Last(execCtx)
		if last != nil && last.Next == dashi.NextNone {
			if err = a.eng.recoverTasks(execCtx); err == nil {
				result = Result{Outcome: dashi.Outcome{TurnView: last.View}}
			}
		} else if last != nil && (last.Next == dashi.NextContinue || last.Next == dashi.NextResume && last.View.ReadyToContinue) {
			result, err = a.eng.continueLast(execCtx, dashi.Continue{}, last)
		} else {
			diagnostic("recorded work does not permit automatic continuation")
			return 3
		}

	} else if selected != nil {
		result, err = a.eng.PromptInput(execCtx, selected)
	} else {
		result, err = a.eng.Prompt(execCtx, payload)
	}
	runErr := err
	if err == nil {
		result, err = a.eng.quietAutomationTasks(execCtx, result)
	}
	if err != nil {
		diagnostic(err.Error())
	}
	if closeErr := a.eng.closeTasks(); closeErr != nil {
		diagnostic(closeErr.Error())
		return 1
	}
	scanCtx, scanCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scanCancel()
	final, scanErr := loadAutomationSnapshot(scanCtx, a.eng.handle().Runner.Recorder.(interface {
		dashi.Loader
		dashi.SessionIndex
	}), s.ID)
	if scanErr != nil {
		diagnostic(scanErr.Error())
		return 1
	}
	original := result.Outcome.ID
	if original == "" {
		original = final.latest.ID
	}
	printAutomationAttention(stderr, final.affected, original)
	if err != nil || len(final.affected) > 0 {
		diagnostic("Session " + s.ID + " retained. Resume with dashi --resume " + s.ID + "; inspect /tasks and exact effects before recovery.")
	}
	var commitErr *dashi.CommitError
	persistenceFailure := a.eng.persistenceFailed || errors.As(runErr, &commitErr) || errors.As(err, &commitErr)
	return automationCode(int(signalCode.Load()), persistenceFailure || runErr != nil && len(final.affected) == 0, false, final.affected)
}

type automationDiagnosticWriter struct{ io.Writer }

func (w *automationDiagnosticWriter) Write(data []byte) (int, error) {
	_, err := io.WriteString(w.Writer, workspace.PrintableText(sanitizeAutomationDiagnostic(string(data))))
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func (e *Engine) quietAutomationTasks(ctx context.Context, result Result) (Result, error) {
	host := e.taskHost()
	if host == nil {
		return result, nil
	}
	host.mu.Lock()
	recoveryErr := host.recoveryErr
	host.mu.Unlock()
	if recoveryErr != nil {
		return result, recoveryErr
	}
	for {
		if result.Outcome.ID != "" && result.Outcome.Next() != dashi.NextNone {
			return result, fmt.Errorf("parent %s needs %s before task delivery", result.Outcome.ID, result.Outcome.Next())
		}
		waitCtx, cancel := context.WithCancel(ctx)
		type waitResult struct {
			idle bool
			err  error
		}
		done := make(chan waitResult, 1)
		go func() { idle, err := host.tasks.Wait(waitCtx); done <- waitResult{idle, err} }()
		tick := time.NewTicker(20 * time.Millisecond)
		var waited waitResult
		waiting := true
		for waiting {
			select {
			case waited = <-done:
				waiting = false
			case <-tick.C:
				for _, task := range host.tasks.List() {
					if task.Running() || taskNoticeReady(dashi.TurnView{Stop: dashi.Stop{Kind: dashi.StopKind(task.State)}}) {
						continue
					}
					cancel()
					<-done
					tick.Stop()
					return result, fmt.Errorf("task %s Turn %s needs attention before delivery", task.ID, task.TurnID)
				}
			case <-ctx.Done():
				cancel()
				waited = <-done
				waiting = false
			}
		}
		cancel()
		tick.Stop()
		if waited.err != nil {
			return result, waited.err
		}
		next, err := e.DriveTaskNotices(ctx)
		if err != nil {
			return result, err
		}
		if next.Outcome.ID != "" {
			result = next
		}
		host.mu.Lock()
		pending := len(host.notices) > 0 || host.rescan || host.recoveryErr != nil
		host.mu.Unlock()
		if waited.idle && !pending && e.Queued() == 0 && !e.HasPending() {
			return result, nil
		}
	}
}

// Valid machine flags still produce an empty response document on a failed
// workspace/home preflight. No input, saved record, or live setup is touched.
func automationPreflightFailure(o Options, stdout, stderr io.Writer, cause error) int {
	fmt.Fprintln(stderr, workspace.PrintableText("dashi: "+sanitizeAutomationDiagnostic(cause.Error())))
	format := o.OutputFormat
	if format == "" {
		format = "text"
	}
	out := stdout
	if f, ok := stdout.(*os.File); ok {
		adapted, closeOut, err := prepareAutomationProcessOutput(f)
		if err != nil {
			return 1
		}
		out = adapted
		if closeOut != nil {
			defer closeOut.Close()
		}
	}
	output, err := newAutomationOutput(context.Background(), format, out)
	if err != nil {
		return 1
	}
	if err := output.Finish(); err != nil {
		fmt.Fprintln(stderr, "dashi: response delivery failed or is incomplete; no work was started.")
	}
	return 1
}
