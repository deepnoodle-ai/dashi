package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// lineUI is the UI for a plain terminal, a pipe, and tests. It prints each
// block as text, and it reads approval answers from the controller.
type lineUI struct {
	out io.Writer

	mu      sync.Mutex
	printed map[string]string
	midLine bool
	// live is true once the scrollback is shown. The user typed the lines
	// of a live run, so the terminal has echoed them already.
	live bool
	ctrl *Controller
	quit chan struct{}
	once sync.Once
}

func newLineUI(out io.Writer) *lineUI {
	return &lineUI{
		out:     out,
		printed: map[string]string{},
		quit:    make(chan struct{}),
	}
}

func (u *lineUI) Emit(b Block) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch b.Kind {
	case KindUser:
		if u.live {
			return
		}
	case KindAssistant:
		prev := u.printed[b.ID]
		switch {
		case b.Text == prev:
			return
		case strings.HasPrefix(b.Text, prev):
			fmt.Fprint(u.out, b.Text[len(prev):])
		case !b.Done:
			// A partial text that differs from the printed text waits for
			// the settled text.
			return
		default:
			// The settled answer is not what streamed. Show it whole.
			if u.midLine {
				fmt.Fprintln(u.out)
			}
			if b.Text == "" {
				fmt.Fprintln(u.out, "[Corrected: the model gave no answer text]")
			} else {
				fmt.Fprintln(u.out, "[Corrected answer]")
				fmt.Fprint(u.out, b.Text)
			}
		}
		u.printed[b.ID] = b.Text
		u.midLine = !strings.HasSuffix(b.Text, "\n")
		return
	case KindTool:
		if !b.Done {
			return
		}
	case KindSteer:
	}
	if u.midLine {
		fmt.Fprintln(u.out)
		u.midLine = false
	}
	fmt.Fprintln(u.out, FormatBlock(b))
}

// SetLive marks the end of the scrollback.
func (u *lineUI) SetLive() {
	u.mu.Lock()
	u.live = true
	u.mu.Unlock()
}

func (u *lineUI) Reset() {
	u.mu.Lock()
	u.live = false
	u.printed = map[string]string{}
	u.mu.Unlock()
}

func (u *lineUI) Approve(ctx context.Context, r ApprovalRequest) (Approval, error) {
	u.mu.Lock()
	if u.midLine {
		fmt.Fprintln(u.out)
		u.midLine = false
	}
	fmt.Fprintf(u.out, "%s\n", r.Summary)
	if r.Command != "" {
		fmt.Fprintf(u.out, "  $ %s\n", r.Command)
	}
	if r.Preview != "" {
		fmt.Fprintln(u.out, r.Preview)
	}
	fmt.Fprint(u.out, "Allow once / always / deny? [y/a/n] ")
	u.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return Approval{}, ctx.Err()
		case line := <-u.ctrl.Answers():
			if a, ok := parseAnswer(line); ok {
				return a, nil
			}
			fmt.Fprint(u.out, "Type y, a, or n: ")
		}
	}
}

// parseAnswer maps an approval answer to an Approval.
func parseAnswer(line string) (Approval, bool) {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "once":
		return Approval{Allow: true}, true
	case "a", "always":
		return Approval{Allow: true, Remember: true}, true
	case "n", "no", "deny":
		return Approval{Reason: declinedReason}, true
	}
	return Approval{}, false
}

func (u *lineUI) Ask(ctx context.Context, q Question) (string, error) {
	u.mu.Lock()
	fmt.Fprintf(u.out, "%s\n", q.Prompt)
	for i, c := range q.Choices {
		fmt.Fprintf(u.out, "  %d. %s\n", i+1, c.Label)
	}
	fmt.Fprint(u.out, "> ")
	u.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case line := <-u.ctrl.Answers():
		return line, nil
	}
}

func (u *lineUI) Quit() {
	u.once.Do(func() { close(u.quit) })
}

// serveLines reads lines from in and gives each to the controller, until in
// ends or the UI quits. It waits for running work before it returns. With
// sequential set, it waits for each line's work before it reads the next, so
// a script piped into dashi does not steer its own turns. A run that waits
// for an approval answer does not count as work: the next line is the answer.
func (u *lineUI) serveLines(in io.Reader, sequential bool) error {
	lines := make(chan string)
	errc := make(chan error, 1)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(in)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		errc <- sc.Err()
	}()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// No more input can come. A run that waits for an
				// approval answer gets a cancel, not an endless wait.
				u.ctrl.WaitForInput()
				u.ctrl.CancelUnanswered()
				u.ctrl.Wait()
				return <-errc
			}
			u.ctrl.Input(line)
			if sequential {
				// A run that waits for approval needs the next line.
				u.ctrl.WaitForInput()
			}
		case <-u.quit:
			u.ctrl.Wait()
			return nil
		}
	}
}

var errNoApprover = errors.New("no one can approve this call")

// printUI is the UI for -p. Only the answer goes to stdout; steps go to
// stderr. It approves every call when yes is set and denies them otherwise.
type printUI struct {
	err io.Writer
	yes bool
	mu  sync.Mutex
}

func (u *printUI) Emit(b Block) {
	switch b.Kind {
	case KindUser, KindAssistant:
		return
	case KindTool:
		if !b.Done {
			return
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintln(u.err, FormatBlock(b))
}

func (u *printUI) Reset() {}

func (u *printUI) Quit() {}

func (u *printUI) Approve(context.Context, ApprovalRequest) (Approval, error) {
	if u.yes {
		return Approval{Allow: true}, nil
	}
	return Approval{Reason: "No one can approve this call in a " +
		"non-interactive run. Do not retry it. Tell the user to run again " +
		"with --yes, or to use the interactive screen."}, nil
}

func (u *printUI) Ask(context.Context, Question) (string, error) {
	return "", errNoApprover
}
