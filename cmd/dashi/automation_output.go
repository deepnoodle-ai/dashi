package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/workspace"
)

const (
	automationTextChunk       = 4 << 10
	automationWriteInactivity = 10 * time.Second
)

type responseMessage struct {
	Content string `json:"content"`
}
type responseMessages struct {
	Messages []responseMessage `json:"messages"`
}

// Only finite in-memory writers, regular files, or deadline-capable writers
// qualify. There is no goroutine fallback for an uninterruptible Write.
type automationWriter struct {
	out      io.Writer
	terminal bool
	deadline func(time.Time) error
	flush    func() error
}

func qualifyAutomationWriter(out io.Writer) (automationWriter, error) {
	w := automationWriter{out: out}
	if out == io.Discard {
		return w, nil
	}
	if f, ok := out.(*os.File); ok {
		info, err := f.Stat()
		if err != nil {
			return automationWriter{}, errors.New("cannot inspect stdout")
		}
		if info.Mode().IsRegular() {
			return w, nil
		}
	}
	if d, ok := out.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := d.SetWriteDeadline(time.Now().Add(automationWriteInactivity)); err != nil {
			return automationWriter{}, errors.New("stdout does not support cancellable writes")
		}
		if err := d.SetWriteDeadline(time.Time{}); err != nil {
			return automationWriter{}, errors.New("cannot clear stdout deadline")
		}
		w.deadline = d.SetWriteDeadline
	} else {
		switch out.(type) {
		case *bytes.Buffer, *strings.Builder, automationNullOutput:
		default:
			return automationWriter{}, errors.New("stdout does not support cancellable writes")
		}
	}
	if f, ok := out.(interface{ Flush() error }); ok {
		w.flush = f.Flush
	}
	return w, nil
}

func (w automationWriter) write(ctx context.Context, data []byte, inactivity time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The cancellation callback and deadline renewal share a mutex. Otherwise
	// a renewal can overwrite cancellation's immediate deadline and block.
	var deadlineMu sync.Mutex
	if w.deadline != nil {
		done := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			deadlineMu.Lock()
			_ = w.deadline(time.Now())
			deadlineMu.Unlock()
			close(done)
		})
		defer func() {
			if !stop() {
				<-done
			}
			_ = w.deadline(time.Time{})
		}()
	}
	for len(data) > 0 {
		deadlineMu.Lock()
		err := ctx.Err()
		if err == nil && w.deadline != nil {
			err = w.deadline(time.Now().Add(inactivity))
		}
		deadlineMu.Unlock()
		if err != nil {
			return err
		}
		// Small writes permit inactivity renewal when bytes advance. A partial
		// write advances once; a failed write never repeats its written prefix.
		n, err := w.out.Write(data[:min(len(data), automationTextChunk)])
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return errors.New("cannot write stdout")
		}
		if n <= 0 {
			return io.ErrNoProgress
		}
		data = data[n:]
	}
	if w.flush != nil {
		if err := w.flush(); err != nil {
			return errors.New("cannot flush stdout")
		}
	}
	return nil
}

// The sole writer reloads one acknowledged parent response at a time. A
// monotone numbered boundary coalesces scheduling hints without a copied
// reply queue. Slow stdout never blocks source recording or drops responses.
type automationOutput struct {
	format      string
	writer      automationWriter
	ctx         context.Context
	cancel      context.CancelFunc
	inactivity  time.Duration
	mu          sync.Mutex
	load        func(context.Context, int) (dashi.TurnView, error)
	first, last int
	finished    bool
	err         error
	failed      func()
	wake        chan struct{}
	done        chan struct{}
}

func newAutomationOutput(ctx context.Context, format string, out io.Writer) (*automationOutput, error) {
	if format != "text" && format != "json" && format != "jsonl" {
		return nil, errors.New("unsupported output format")
	}
	writer, err := qualifyAutomationWriter(out)
	if err != nil {
		return nil, err
	}
	return startAutomationOutput(ctx, format, writer, automationWriteInactivity), nil
}

func startAutomationOutput(ctx context.Context, format string, writer automationWriter, inactivity time.Duration) *automationOutput {
	ctx, cancel := context.WithCancel(ctx)
	o := &automationOutput{format: format, writer: writer, ctx: ctx, cancel: cancel, inactivity: inactivity, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go o.run()
	return o
}

func (o *automationOutput) source(load func(context.Context, int) (dashi.TurnView, error), failed func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.load, o.failed = load, failed
}

func (o *automationOutput) selectTurn(n int) {
	if n <= 0 {
		return
	}
	o.mu.Lock()
	if !o.finished && o.err == nil {
		if o.first == 0 {
			o.first = n
		}
		o.last = max(o.last, n)
	}
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *automationOutput) Finish() error {
	o.mu.Lock()
	if o.finished {
		o.mu.Unlock()
		return errors.New("responses were already submitted")
	}
	o.finished = true
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
	<-o.done
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *automationOutput) Abort() error {
	o.cancel()
	<-o.done
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *automationOutput) run() {
	defer close(o.done)
	defer o.cancel()
	fail := func(err error) {
		o.mu.Lock()
		o.err = err
		failed := o.failed
		o.mu.Unlock()
		if failed != nil {
			failed()
		}
	}
	write := func(data []byte) error { return o.writer.write(o.ctx, data, o.inactivity) }
	cursor, count := 0, 0
	var lastAnswer string
	for {
		o.mu.Lock()
		first, last, finished, load := o.first, o.last, o.finished, o.load
		o.mu.Unlock()
		if o.format != "jsonl" && !finished {
			last = 0
		}
		if finished && o.format == "json" && cursor == 0 {
			if err := write([]byte(`{"messages":[`)); err != nil {
				fail(err)
				return
			}
		}
		if cursor == 0 && first > 0 && (o.format == "jsonl" || finished) {
			cursor = first
		}
		for cursor > 0 && cursor <= last {
			loadCtx, loadCancel := context.WithTimeout(o.ctx, automationWriteInactivity)
			view, err := load(loadCtx, cursor)
			loadCancel()
			if err != nil {
				fail(errors.New("cannot reload selected response"))
				return
			}
			cursor++
			if view.Origin == dashi.StartCompaction || view.Parent != nil {
				continue
			}
			lastAnswer = view.Reply // An empty last reply must stay empty.
			if view.Reply == "" {
				continue
			}
			if o.format == "text" {
				continue
			}
			data, err := json.Marshal(responseMessage{Content: view.Reply})
			if err != nil {
				fail(errors.New("cannot encode response"))
				return
			}
			if o.format == "jsonl" {
				data = append(data, '\n')
			} else if count > 0 {
				data = append([]byte(","), data...)
			}
			if err := write(data); err != nil {
				fail(err)
				return
			}
			count++
		}
		if finished {
			var data []byte
			switch o.format {
			case "json":
				data = []byte("]}\n")
			case "text":
				if lastAnswer != "" {
					if o.writer.terminal {
						lastAnswer = workspace.PrintableText(lastAnswer)
					}
					if f, ok := o.writer.out.(*os.File); ok && isTerminal(f) {
						lastAnswer = workspace.PrintableText(lastAnswer)
					}
					data = []byte(lastAnswer + "\n")
				}
			}
			if len(data) > 0 {
				if err := write(data); err != nil {
					fail(err)
					return
				}
			}
			return
		}
		select {
		case <-o.wake:
		case <-o.ctx.Done():
			fail(o.ctx.Err())
			return
		}
	}
}
