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
	out              io.Writer
	attachmentErrors io.Writer

	mu         sync.Mutex
	printed    map[string]string
	midLine    bool
	inputEnded bool
	// live is true once the scrollback is shown. The user typed the lines
	// of a live run, so the terminal has echoed them already.
	live            bool
	ctrl            *Controller
	attachmentMu    sync.Mutex
	attachments     attachmentDraft
	attachmentInput *submittedInput
	receiptMu       sync.Mutex
	receiptQueue    []attachmentEvent
	receiptsRunning bool
	quit            chan struct{}
	once            sync.Once
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
	case KindPending:
		if b.Done {
			return
		}
	case KindUser:
		if u.live {
			return
		}
	case KindReasoning:
		if !b.Done || b.Text == "" {
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
			// the finished text.
			return
		default:
			// The finished reply is not what streamed. Show it whole.
			if u.midLine {
				fmt.Fprintln(u.out)
			}
			if b.Text == "" {
				fmt.Fprintln(u.out, "[Corrected: the model gave no reply text]")
			} else {
				fmt.Fprintln(u.out, "[Corrected reply]")
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
	case KindCompaction:
		text := FormatBlock(b)
		if u.printed[b.ID] == text {
			return
		}
		u.printed[b.ID] = text
	case KindNotice, KindHint:
		if b.ID == "task-delivery" {
			if u.printed[b.ID] == b.Text {
				return
			}
			u.printed[b.ID] = b.Text
		}
	case KindTaskStatus:
		// A line per change of state. Counts change with every child step,
		// and the tool line already says that the task started.
		if b.State == StateRunning || u.printed[b.ID] == b.Name {
			return
		}
		u.printed[b.ID] = b.Name
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
	u.attachmentMu.Lock()
	u.attachments.clear()
	u.attachmentInput = nil
	u.attachmentMu.Unlock()
	u.mu.Lock()
	u.live = false
	u.printed = map[string]string{}
	u.mu.Unlock()
}

func (u *lineUI) Approve(ctx context.Context, a toolApproval) (Approval, error) {
	u.mu.Lock()
	if u.inputEnded {
		u.mu.Unlock()
		return Approval{}, errNoApprover
	}
	if u.midLine {
		fmt.Fprintln(u.out)
		u.midLine = false
	}
	fmt.Fprint(u.out, approvalText(a))
	if line := a.checkLine(); line != "" {
		fmt.Fprintln(u.out, "  "+line)
	}
	fmt.Fprint(u.out, approvalQuestion(a))
	u.mu.Unlock()
	// A check that is still running prints its answer on a new line when it
	// comes, then asks again.
	var checked <-chan struct{}
	if _, done := a.Check.Result(); a.Check != nil && !done {
		checked = a.Check.done
	}
	for {
		select {
		case <-ctx.Done():
			return Approval{}, ctx.Err()
		case <-checked:
			checked = nil
			u.mu.Lock()
			fmt.Fprintf(u.out, "\n  %s\n%s", a.checkLine(), approvalQuestion(a))
			u.mu.Unlock()
		case line := <-u.ctrl.Answers():
			if answer, ok := parseAnswer(line); ok && (!answer.Remember || a.offersRemember()) {
				return answer, nil
			}
			u.mu.Lock()
			if a.offersRemember() {
				fmt.Fprint(u.out, "Type y, a, or n REASON: ")
			} else {
				fmt.Fprint(u.out, "Type y, or n REASON: ")
			}
			u.mu.Unlock()
			u.ctrl.answerPending.Store(false)
		}
	}
}

// approvalQuestion is line mode's question for a. A command decide flagged
// or held offers allow once and deny only.
func approvalQuestion(a toolApproval) string {
	if !a.offersRemember() {
		return "Allow once / deny? [y/n or n REASON] "
	}
	return "Allow once / allow for this Dashi run / deny? [y/a/n or n REASON] "
}

// parseAnswer maps an approval answer to an Approval.
func parseAnswer(line string) (Approval, bool) {
	if name, reason, ok := strings.Cut(strings.TrimSpace(line), " "); ok && (strings.EqualFold(name, "n") || strings.EqualFold(name, "deny")) {
		if strings.TrimSpace(reason) != "" {
			return Approval{Reason: strings.TrimSpace(reason)}, true
		}
	}
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

func (u *lineUI) Ask(ctx context.Context, q taskQuestion) (questionAnswer, error) {
	u.mu.Lock()
	if u.inputEnded {
		u.mu.Unlock()
		return questionAnswer{}, errNoApprover
	}
	fmt.Fprintf(u.out, "%s\n", q.Prompt)
	if q.Task != "" {
		fmt.Fprintf(u.out, "  %s\n", taskOrigin(q.Task))
	}
	for i, c := range q.Choices {
		fmt.Fprintf(u.out, "  %d. %s\n", i+1, c.Label)
		if c.Description != "" {
			fmt.Fprintf(u.out, "     %s\n", c.Description)
		}
	}
	fmt.Fprint(u.out, "> ")
	if q.Multiple {
		fmt.Fprintln(u.out, "Choose numbers separated by commas, or other: YOUR ANSWER.")
	}
	u.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return questionAnswer{}, ctx.Err()
		case line := <-u.ctrl.Answers():
			a, err := parseQuestionAnswer(q.Question, line)
			if err == nil {
				return a, nil
			}
			u.mu.Lock()
			fmt.Fprintf(u.out, "%v\n> ", err)
			u.mu.Unlock()
			u.ctrl.answerPending.Store(false)
		}
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
				u.mu.Lock()
				u.inputEnded = true
				u.mu.Unlock()
				u.ctrl.mu.Lock()
				u.ctrl.autoTaskDelivery = false
				u.ctrl.mu.Unlock()
				// No more input can come. A run that waits for an
				// approval answer gets a cancel, not an endless wait.
				u.ctrl.WaitForInput()
				u.ctrl.CancelUnanswered()
				u.ctrl.Wait()
				if u.AttachmentUnsaved() {
					u.AttachmentWarning("Input ended with an unsubmitted attachment draft. Its source snapshots were not submitted; published recovery content is retained.")
				}
				if err := <-errc; err != nil {
					return err
				}
				if _, err := u.ctrl.eng.QuietTasks(u.ctrl.base, Result{}); err != nil {
					return fmt.Errorf("tasks need explicit action; inspect /tasks or reopen this session: %w", err)
				}
				return nil
			}
			select {
			case <-u.quit:
				u.ctrl.Wait()
				return nil
			default:
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

// printUI is the UI for the run command. Only the reply goes to stdout; steps go to
// stderr. It approves every call when yes is set and denies them otherwise.
type printUI struct {
	err io.Writer
	yes bool
	mu  sync.Mutex
	// taskPrinted holds the last state printed for each task status line.
	taskPrinted       map[string]string
	compactionPrinted map[string]string
}

func (u *printUI) Emit(b Block) {
	switch b.Kind {
	case KindPending:
		if b.Done {
			return
		}
	case KindReasoning:
		if !b.Done || b.Text == "" {
			return
		}
	case KindUser, KindAssistant:
		return
	case KindTool:
		if !b.Done {
			return
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if b.Kind == KindCompaction {
		text := FormatBlock(b)
		if u.compactionPrinted[b.ID] == text {
			return
		}
		if u.compactionPrinted == nil {
			u.compactionPrinted = map[string]string{}
		}
		u.compactionPrinted[b.ID] = text
	}
	if b.Kind == KindTaskStatus {
		// Only the end of a task, once.
		if !b.Done || u.taskPrinted[b.ID] == b.Name {
			return
		}
		if u.taskPrinted == nil {
			u.taskPrinted = map[string]string{}
		}
		u.taskPrinted[b.ID] = b.Name
	}
	fmt.Fprintln(u.err, FormatBlock(b))
}

func (u *printUI) Reset() {}

func (u *printUI) Quit() {}

func (u *printUI) Approve(context.Context, toolApproval) (Approval, error) {
	if u.yes {
		return Approval{Allow: true}, nil
	}
	return Approval{Reason: "No one can approve this call in a " +
		"non-interactive run. Do not retry it. Tell the user to run again " +
		"with dashi run --yes, or to use the interactive screen."}, nil
}

func (u *printUI) Ask(context.Context, taskQuestion) (questionAnswer, error) {
	return questionAnswer{}, errNoApprover
}
