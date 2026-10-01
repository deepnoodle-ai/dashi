package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

const (
	automationProgressEntries = 64
	automationProgressBytes   = 256 << 10
	automationTextChunk       = 4 << 10
	automationWriteInactivity = 10 * time.Second
)

type automationProgress struct {
	Version int    `json:"version"`
	Type    string `json:"type"`
	Order   uint64 `json:"order"`
	automationTarget
	Anchor      int64  `json:"anchor"`
	Kind        string `json:"kind"`
	Provisional bool   `json:"provisional"`
	Text        string `json:"text,omitempty"`
}

type automationGap struct {
	Version        int                `json:"version"`
	Type           string             `json:"type"`
	Order          uint64             `json:"order"`
	Targets        []automationTarget `json:"targets"`
	Dropped        uint64             `json:"dropped"`
	TargetsOmitted uint64             `json:"targets_omitted,omitempty"`
}

// Only finite in-memory writers, regular files, or deadline-capable writers
// qualify. There is no goroutine fallback for an uninterruptible Write.
type automationWriter struct {
	out      io.Writer
	deadline func(time.Time) error
	flush    func() error
}

func qualifyAutomationWriter(out io.Writer) (automationWriter, error) {
	w := automationWriter{out: out}
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

type automationQueuedProgress struct {
	value automationProgress
	bytes int
}

// One worker owns stdout. Producers only copy an allowlisted observation into
// a bounded queue. Finish reserves final delivery outside that queue.
type automationOutput struct {
	format      string
	writer      automationWriter
	ctx         context.Context
	cancel      context.CancelFunc
	inactivity  time.Duration
	mu          sync.Mutex
	queue       []automationQueuedProgress
	queuedBytes int
	gap         automationGap
	final       []byte
	finished    bool
	err         error
	wake        chan struct{}
	done        chan struct{}
}

func newAutomationOutput(ctx context.Context, format string, out io.Writer) (*automationOutput, error) {
	if format != "json" && format != "jsonl" {
		return nil, errors.New("unsupported machine output format")
	}
	writer, err := qualifyAutomationWriter(out)
	if err != nil {
		return nil, err
	}
	return startAutomationOutput(ctx, format, writer, automationWriteInactivity), nil
}

// Tests supply a shorter inactivity duration without changing production's
// ten-second contract or sleeping through that full interval.
func startAutomationOutput(ctx context.Context, format string, writer automationWriter, inactivity time.Duration) *automationOutput {
	ctx, cancel := context.WithCancel(ctx)
	o := &automationOutput{format: format, writer: writer, ctx: ctx, cancel: cancel, inactivity: inactivity,
		wake: make(chan struct{}, 1), done: make(chan struct{})}
	go o.run()
	return o
}

func (o *automationOutput) notify() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Observe excludes reasoning, arguments, content snapshots, raw outcomes,
// prepared requests, and tool-output/progress payloads synchronously.
func (o *automationOutput) Observe(event dashi.Event, taskID, callID string) {
	if o.format != "jsonl" {
		return
	}
	p := automationProgress{Version: 1, Type: "progress", automationTarget: automationTarget{event.TurnID, taskID, callID}, Anchor: event.Seq}
	switch {
	case event.Gap:
		o.mu.Lock()
		if !o.finished && o.err == nil {
			o.dropLocked(p.automationTarget)
		}
		o.mu.Unlock()
		o.notify()
		return
	case event.Delta != nil && event.Delta.Kind == llm.DeltaText:
		p.Kind, p.Provisional = "text", true
		text := event.Delta.Text
		if !utf8.ValidString(text) {
			o.drop(p.automationTarget)
			return
		}
		for len(text) > 0 {
			n := min(len(text), automationTextChunk)
			for n > 0 && !utf8.RuneStart(text[n-1]) && n < len(text) {
				n--
			}
			// Find the largest complete UTF-8 prefix, including an ASCII end.
			for n > 0 && !utf8.ValidString(text[:n]) {
				n--
			}
			p.Text = strings.Clone(text[:n])
			if !o.enqueue(p) {
				return
			}
			text = text[n:]
		}
		return
	case event.Step != nil:
		// Step kind and target identity are safe lifecycle data. No step
		// body or model/tool fact crosses the projection boundary.
		switch event.Step.Kind {
		case dashi.StepStarted, dashi.StepDelivered, dashi.StepContinued, dashi.StepLoopContinued,
			dashi.StepModelIntent, dashi.StepContextCompacted, dashi.StepModelSettled, dashi.StepModelReconciled,
			dashi.StepToolDecision, dashi.StepToolStarted, dashi.StepToolWaiting, dashi.StepToolSettled,
			dashi.StepApprovalSettled, dashi.StepHostedApprovalSettled, dashi.StepOutputRecorded, dashi.StepStopped:
			p.Kind = string(event.Step.Kind)
		default:
			return
		}
	default:
		return
	}
	o.enqueue(p)
}

func (o *automationOutput) drop(target automationTarget) {
	o.mu.Lock()
	if !o.finished && o.err == nil {
		o.dropLocked(target)
	}
	o.mu.Unlock()
	o.notify()
}

func (o *automationOutput) dropLocked(target automationTarget) {
	o.gap.Dropped++
	if len(target.TurnID)+len(target.TaskID)+len(target.CallID) > automationTextChunk {
		o.gap.TargetsOmitted++
		return
	}
	if slices.Contains(o.gap.Targets, target) {
		return
	}
	if len(o.gap.Targets) < automationProgressEntries {
		o.gap.Targets = append(o.gap.Targets, target)
	} else {
		o.gap.TargetsOmitted++
	}
}

func (o *automationOutput) enqueue(value automationProgress) bool {
	if len(value.TurnID)+len(value.TaskID)+len(value.CallID) > automationTextChunk {
		o.drop(value.automationTarget)
		return false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		o.drop(value.automationTarget)
		return false
	}
	size := len(encoded) + 32 // Room for the writer-assigned order and LF.
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || o.err != nil {
		return false
	}
	if len(o.queue) >= automationProgressEntries || o.queuedBytes+size > automationProgressBytes {
		o.dropLocked(value.automationTarget)
		o.notify()
		return false
	}
	o.queue = append(o.queue, automationQueuedProgress{value, size})
	o.queuedBytes += size
	o.notify()
	return true
}

// Finish serializes a detached result before handing it to the sole writer.
// There is exactly one attempt, including after an encoding or write failure.
func (o *automationOutput) Finish(result automationResult) error {
	o.mu.Lock()
	if o.finished {
		o.mu.Unlock()
		return errors.New("result was already submitted")
	}
	o.finished = true
	result.Version, result.Type = 1, "result"
	if result.Turns == nil {
		result.Turns = []automationTurn{}
	}
	if result.Tasks == nil {
		result.Tasks = []automationTask{}
	}
	if result.Affected == nil {
		result.Affected = []automationAffected{}
	}
	if result.Warnings == nil {
		result.Warnings = []string{}
	}
	data, err := json.Marshal(result)
	if err != nil {
		o.err = errors.New("cannot encode result")
		o.cancel()
	} else {
		o.final = append(data, '\n')
	}
	o.mu.Unlock()
	o.notify()
	<-o.done
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// Abort cancels a stalled output writer and joins it. It does not retry a
// result or modify persistence. The controller settles owned work separately.
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
	var order uint64
	for {
		o.mu.Lock()
		var data []byte
		var err error
		last := false
		switch {
		case len(o.queue) > 0:
			entry := o.queue[0]
			o.queue[0] = automationQueuedProgress{}
			o.queue = o.queue[1:]
			o.queuedBytes -= entry.bytes
			order++
			entry.value.Order = order
			data, err = json.Marshal(entry.value)
		case o.gap.Dropped > 0:
			order++
			o.gap.Version, o.gap.Type, o.gap.Order = 1, "gap", order
			data, err = json.Marshal(o.gap)
			o.gap = automationGap{}
		case o.finished:
			data, last = o.final, true
			err = o.err
		}
		o.mu.Unlock()
		if len(data) == 0 && err == nil && !last {
			select {
			case <-o.wake:
				continue
			case <-o.ctx.Done():
				err = o.ctx.Err()
			}
		}
		if err == nil && len(data) > 0 {
			if !last {
				data = append(data, '\n')
			}
			err = o.writer.write(o.ctx, data, o.inactivity)
		}
		if err != nil {
			o.mu.Lock()
			if o.err == nil {
				o.err = err
			}
			o.queue, o.final = nil, nil
			o.queuedBytes = 0
			o.mu.Unlock()
			return
		}
		if last {
			return
		}
	}
}
