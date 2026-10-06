package main

// The --turn-events file lets another program, such as a terminal
// multiplexer that types prompts into dashi, know when a turn starts, when
// it ends, and when dashi waits for the person. Each event is one JSON line
// in the shape of a Claude Code hook input (hook_event_name, session_id,
// prompt_id, prompt, last_assistant_message, error_type, error_message,
// notification_type, message), so a reader of Claude Code hooks reads dashi
// unchanged. prompt_id is the Dashi turn ID.
//
// The end of a turn is held until dashi can take the next prompt, and the
// controller writes it under the lock that decides how input is read. A
// reader that sends a prompt after Stop therefore never reaches a running
// turn as a steer. When queued input runs as the next turn of the same run,
// the held end is written just before that turn's start. Each line is
// fsynced before the call returns.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
)

// turnEventTextLimit caps each text field of a record, so one line stays
// small for a reader that reads the file in chunks. A cut keeps the start
// of the text, where a sender's own prefix is.
const turnEventTextLimit = 32 << 10

// turnEvent is one line of the turn-event file. The names and values are
// those of Claude Code's hook inputs.
type turnEvent struct {
	SessionID            string `json:"session_id"`
	HookEventName        string `json:"hook_event_name"`
	Cwd                  string `json:"cwd,omitempty"`
	PromptID             string `json:"prompt_id,omitempty"`
	Prompt               string `json:"prompt,omitempty"`
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
	ErrorType            string `json:"error_type,omitempty"`
	ErrorMessage         string `json:"error_message,omitempty"`
	NotificationType     string `json:"notification_type,omitempty"`
	Message              string `json:"message,omitempty"`
}

// The hook_event_name values dashi writes.
const (
	turnEventUserPromptSubmit = "UserPromptSubmit"
	turnEventStop             = "Stop"
	turnEventStopFailure      = "StopFailure"
	turnEventNotification     = "Notification"
)

// notificationPermissionPrompt is the notification_type dashi writes when
// a tool call waits for approval or the model asks the person a question.
// Both need the person before the turn can go on, and readers of Claude
// Code hooks treat this value as "waiting on you"; the message says which.
const notificationPermissionPrompt = "permission_prompt"

// turnEventLog appends turn events to one file. A nil log writes nothing,
// so callers need no check.
type turnEventLog struct {
	mu  sync.Mutex
	f   *os.File
	dir string
	// session returns the ID of the open session.
	session func() string
	// open is the ID of the turn whose start was written and whose end was
	// not.
	open string
	// held is the end of the last turn, written when dashi can take the
	// next prompt or when the next turn starts.
	held *turnEvent
	// warned is set once a write failure has been shown.
	warned bool
}

// failure returns the warning to show for a failed write: the first one
// only, so a full disk does not flood the transcript. It returns empty for
// a nil err and for every later failure.
func (l *turnEventLog) failure(err error) string {
	if l == nil || err == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.warned {
		return ""
	}
	l.warned = true
	return "The --turn-events file was not written: " + err.Error() + ". Later failures are not shown."
}

// openTurnEvents opens path for appending, creating it with mode 0600.
// The file holds prompts and replies, so an existing file must be a
// regular file, not a symlink, and is restricted to mode 0600 before the
// first line is written. A file dashi cannot chmod is refused.
func openTurnEvents(path, dir string) (*turnEventLog, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|turnEventsOpenFlags, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err == nil && info.Mode().Perm() != 0o600 {
		if err = f.Chmod(0o600); err != nil {
			err = fmt.Errorf("restrict %s to mode 0600: %w", path, err)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return &turnEventLog{f: f, dir: dir}, nil
}

// Close closes the file.
func (l *turnEventLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// started writes UserPromptSubmit for a new turn. A compaction turn is not
// a prompt and writes nothing.
func (l *turnEventLog) started(turnID string, s *dashi.StartFact) error {
	if l == nil || s == nil || s.Origin == dashi.StartCompaction {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.flushLocked()
	l.open = turnID
	return errors.Join(err, l.writeLocked(turnEvent{
		HookEventName: turnEventUserPromptSubmit,
		PromptID:      turnID,
		Prompt:        joinMessages(s.Input),
	}))
}

// ended holds the end of a turn: Stop for a completed turn with its reply,
// and StopFailure with the stop kind and reason for any other stop.
func (l *turnEventLog) ended(out dashi.RunResult) {
	if l == nil || out.Origin == dashi.StartCompaction || out.Stop.Kind == "" {
		return
	}
	e := turnEvent{PromptID: out.Turn.ID}
	if out.Stop.Kind == dashi.StopCompleted {
		e.HookEventName, e.LastAssistantMessage = turnEventStop, out.Reply
	} else {
		e.HookEventName, e.ErrorType = turnEventStopFailure, string(out.Stop.Kind)
		e.ErrorMessage = (&LastTurn{View: out.TurnView, Next: out.Next()}).StopText()
		if out.Stop.Reason != "" {
			e.ErrorMessage = out.Stop.Reason
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open, l.held = "", &e
}

// steered writes UserPromptSubmit for text that reached the running turn
// as a steer, with that turn's ID. A sender whose prompt arrives while a
// turn runs, such as a task notice turn that began just after the last
// Stop, then sees its text taken, and the turn's end answers it. A steer
// that missed the turn goes with the next turn's start instead.
func (l *turnEventLog) steered(text string) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open == "" {
		return nil
	}
	return l.writeLocked(turnEvent{
		HookEventName: turnEventUserPromptSubmit,
		PromptID:      l.open,
		Prompt:        text,
	})
}

// waiting writes Notification when dashi waits for the person.
func (l *turnEventLog) waiting(kind, message string) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.flushLocked()
	return errors.Join(err, l.writeLocked(turnEvent{
		HookEventName:    turnEventNotification,
		NotificationType: kind,
		Message:          message,
	}))
}

// idle writes the held end once a run is over and dashi can take the next
// prompt. A run that started a turn and recorded no end, or that failed
// before any turn started, writes StopFailure with err, so a reader never
// waits for an end that will not come. The caller holds the controller's
// lock, so input that arrives meanwhile waits for the write.
func (l *turnEventLog) idle(err error) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil && (l.open != "" || err != nil && !errors.Is(err, context.Canceled)) {
		e := turnEvent{
			HookEventName: turnEventStopFailure,
			PromptID:      l.open,
			ErrorType:     string(dashi.StopFailed),
		}
		switch {
		case err == nil || errors.Is(err, context.Canceled):
			e.ErrorType, e.ErrorMessage = string(dashi.StopInterrupted), "the turn was interrupted"
		default:
			e.ErrorMessage = err.Error()
		}
		l.held = &e
	}
	l.open = ""
	return l.flushLocked()
}

// flushLocked writes the held end, if any.
func (l *turnEventLog) flushLocked() error {
	if l.held == nil {
		return nil
	}
	e := *l.held
	l.held = nil
	return l.writeLocked(e)
}

// writeLocked appends e as one line and fsyncs the file.
func (l *turnEventLog) writeLocked(e turnEvent) error {
	if l.session != nil {
		e.SessionID = l.session()
	}
	e.Cwd = l.dir
	e.Prompt = capText(e.Prompt)
	e.LastAssistantMessage = capText(e.LastAssistantMessage)
	e.ErrorMessage = capText(e.ErrorMessage)
	e.Message = capText(e.Message)
	// Without HTML escaping, <, > and & keep one byte each, so a capped
	// field stays near its cap once encoded. The encoder adds the newline.
	var line bytes.Buffer
	enc := json.NewEncoder(&line)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return err
	}
	if _, err := l.f.Write(line.Bytes()); err != nil {
		return err
	}
	return l.f.Sync()
}

// capText cuts s to turnEventTextLimit bytes at a character boundary.
func capText(s string) string {
	if len(s) <= turnEventTextLimit {
		return s
	}
	for i := turnEventTextLimit; i > turnEventTextLimit-utf8.UTFMax; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return s[:turnEventTextLimit]
}
