package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
)

type pendingPrompt struct {
	start dashi.Start
	input *submittedInput
	// resultUnknown names an ordinary Start whose recording acknowledgment
	// was lost. No later caller Start can evict its idempotency window.
	resultUnknown string
	attempted     bool
	summary       bool
}

type pendingLine struct {
	id   string
	text string
}

// configureChat follows the selected model only for new work. Continued
// turns keep their recorded model, input, and limits.
func (e *Engine) configureChat() *session.Session {
	e.mu.Lock()
	chat := e.chat
	if chat == nil {
		e.mu.Unlock()
		return nil
	}
	if err := e.cfg.Preferences.selectionReady(); err != nil {
		chat.Agent, chat.Compactor = dashi.Agent{}, nil
		e.mu.Unlock()
		return chat
	}
	chat.Agent = e.agent()
	info, _ := e.cfg.Client.ModelInfo(e.model)
	if p := e.cfg.Preferences; p != nil {
		agent := p.agent()
		d := p.routes.registry[agent.Provider]
		info, _ = d.modelInfo(agent.Model)
	}
	policy := e.compactionPolicy(info)
	chat.Compactor = compaction.New(policy)
	if p := e.cfg.Preferences; p != nil {
		chat.Compactor = &routedCompactor{dashiCompactor: compaction.New(policy), agent: p.agent()}
	}
	window, known := policy.Window()
	warn := !known && !e.windowWarned[e.model]
	if warn {
		if e.windowWarned == nil {
			e.windowWarned = map[string]bool{}
		}
		e.windowWarned[e.model] = true
	}
	threshold := e.cfg.CompactionThreshold
	warnThreshold := threshold > window && !e.thresholdWarned[e.model]
	if warnThreshold {
		if e.thresholdWarned == nil {
			e.thresholdWarned = map[string]bool{}
		}
		e.thresholdWarned[e.model] = true
	}
	model := e.model
	e.mu.Unlock()
	if warn {
		e.warn("Model %s has no known context window; between-Turn history compaction uses 128,000 tokens. Active-Turn protection needs a known window.", model)
	}
	if warnThreshold {
		e.warn("--compaction-threshold %d is above the %d-token window of model %s; history compacts at %d tokens.", threshold, window, model, window)
	}
	return chat
}

// compactionPolicy is the compaction policy for a model. A
// --compaction-threshold becomes the share of the model's window it is. A
// threshold above the window counts as the window, because no model call can
// use more; configureChat warns about it.
func (e *Engine) compactionPolicy(info llm.ModelInfo) compaction.Policy {
	policy := compaction.Policy{Model: info}
	if t := e.cfg.CompactionThreshold; t > 0 {
		window, _ := policy.Window()
		policy.Fraction = float64(t) / float64(window)
	}
	return policy
}

// Compact summarizes only. It cannot authorize execution of queued prompts.
func (e *Engine) Compact(ctx context.Context) (Result, error) {
	e.driverMu.Lock()
	defer e.driverMu.Unlock()
	if err := e.cfg.Preferences.selectionReady(); err != nil {
		return Result{}, err
	}
	if !e.cfg.MCP.Ready() {
		return Result{}, errors.New("MCP startup has not completed; no work was started")
	}
	chat := e.configureChat()
	if p := e.cfg.Preferences; p != nil {
		if err := p.bind(ctx, p.agent()); err != nil {
			return Result{}, err
		}
	}
	return e.stream(ctx, chat.StreamCompact(ctx, nil), false)
}

func (e *Engine) HasPending() bool {
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	return e.pending != nil
}

func (e *Engine) runPending(ctx context.Context) (Result, error) {
	e.inputMu.Lock()
	p := e.pending
	e.inputMu.Unlock()
	if p == nil {
		return Result{}, errors.New("there is no pending prompt")
	}
	if err := e.resolvePrompt(ctx, p); err != nil {
		return Result{}, err
	}
	res, err := e.run(ctx, p.start)
	// Ordinary refusals still require the user to resubmit after recovery.
	// Retain only a summary interruption or a Start whose recording is unknown.
	e.inputMu.Lock()
	// A rejected ordinary attachment Start has no recorded owner. Release it
	// together with the editable draft, so corrections cannot retry old bytes.
	var ce *dashi.CommitError
	rejectedInput := p.input != nil && !p.summary && errors.As(err, &ce) &&
		ce.NothingWritten && ce.Step != nil &&
		ce.Step.Kind == dashi.StepTurnStarted && ce.Step.Started != nil &&
		ce.Step.Started.Origin != dashi.StartCompaction && ce.Step.CommandID == p.start.CommandID
	if e.pending == p && p.resultUnknown == "" && (!p.attempted || rejectedInput) && !p.summary &&
		res.Outcome.Origin != dashi.StartCompaction {
		e.pending = nil
		e.inputReceipt(p, inputPreRecordFailure)
		e.cfg.Emit(Block{ID: "pending:" + p.start.CommandID, Kind: KindPending, Done: true})
	}
	if e.pending == p && p.input != nil {
		if p.resultUnknown != "" {
			e.inputReceipt(p, inputPendingUnknown)
		} else if !p.summary && res.Outcome.Origin != dashi.StartCompaction {
			e.inputReceipt(p, inputPreRecordFailure)
		}
	}
	e.inputMu.Unlock()
	return res, err
}

func (e *Engine) resumePending(ctx context.Context) (Result, error) {
	e.inputMu.Lock()
	p := e.pending
	e.inputMu.Unlock()
	if p == nil {
		return Result{}, errors.New("there is no pending prompt")
	}
	if err := e.resolvePrompt(ctx, p); err != nil {
		return Result{}, err
	}
	last := e.Last(ctx)
	if !e.HasPending() {
		if last.CanGoOn() {
			return e.Resume(ctx)
		}
		return e.run(ctx, p.start)
	}
	if last == nil || last.View.Origin != dashi.StartCompaction {
		return e.runPending(ctx)
	}
	if !last.CanGoOn() {
		if !last.Open() {
			return e.runPending(ctx)
		}
		return Result{}, errors.New("continue or discard the open compaction before sending the pending prompt")
	}
	var cmd dashi.Command = dashi.Continue{}
	if last.LimitStopped() {
		cmd = dashi.Continue{ModelCalls: int(last.View.Stop.Limit.Limit) * 2}
	}
	chat := e.configureChat()
	// The RunResult is yielded after Session.finish. Breaking here prevents
	// Continue's queue dispatch from putting older queued items ahead of the
	// caller's retained Start in a different turn.
	if err := e.bindCommand(ctx, cmd); err != nil {
		return Result{}, err
	}
	e.driverMu.Lock()
	res, err := e.stream(ctx, chat.Stream(ctx, cmd), true)
	e.driverMu.Unlock()
	if err != nil || res.Outcome.Stop.Kind != dashi.StopCompleted ||
		compactiontext.Summary(res.Outcome.Reply) == "" {
		return res, err
	}
	return e.runPending(ctx)
}

func (e *Engine) rememberPromptError(err error) {
	var ce *dashi.CommitError
	if !errors.As(err, &ce) || ce.Step == nil ||
		ce.Step.Kind != dashi.StepTurnStarted || ce.Step.Started == nil {
		return
	}
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	if e.pending != nil && (e.pending.start.CommandID == "" || ce.Step.CommandID == e.pending.start.CommandID) {
		e.pending.start.CommandID = ce.Step.CommandID
		e.pending.attempted = true
		if !ce.NothingWritten {
			e.pending.resultUnknown = ce.Step.TurnID
		}
	}
}

func (e *Engine) resolvePrompt(ctx context.Context, p *pendingPrompt) error {
	e.inputMu.Lock()
	id := p.resultUnknown
	e.inputMu.Unlock()
	if id == "" {
		return nil
	}
	loader, ok := e.handle().Runner.Recorder.(dashi.Loader)
	if !ok {
		return errors.New("pending prompt cannot be checked: recorder cannot load turns")
	}
	turn, err := loader.Load(ctx, id)
	if errors.Is(err, dashi.ErrTurnNotFound) || err == nil && len(turn.Steps) == 0 {
		e.inputMu.Lock()
		p.resultUnknown = ""
		e.inputReceipt(p, inputConfirmedAbsent)
		e.inputMu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("pending prompt waits until turn %s can be read: %w", id, err)
	}
	step := turn.Steps[0]
	if step.Kind != dashi.StepTurnStarted || step.Started == nil ||
		step.CommandID != p.start.CommandID ||
		len(step.Started.Input) < len(p.start.Input) ||
		!reflect.DeepEqual(step.Started.Input[len(step.Started.Input)-len(p.start.Input):], p.start.Input) {
		return fmt.Errorf("pending prompt conflicts with recorded turn %s: %w", id, dashi.ErrCommandIDReused)
	}
	if err := e.acknowledgePrompt(&step); err != nil {
		return err
	}
	if err := e.record(&step); err != nil {
		e.warn("The session file was not saved: %v.", err)
	}
	return nil
}

// acknowledgePrompt moves later input only after the ordinary Start commits.
// Accepting input uses the same lock, so a new queue write cannot overtake the FIFO.
func (e *Engine) acknowledgePrompt(step *dashi.Step) error {
	if step == nil || step.Kind != dashi.StepTurnStarted || step.Started == nil ||
		step.Started.Origin == dashi.StartCompaction {
		return nil
	}
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	if e.pending == nil || e.pending.start.CommandID != "" && step.CommandID != e.pending.start.CommandID {
		return nil
	}
	if e.pending.input != nil && (len(step.Started.Input) < len(e.pending.start.Input) || !reflect.DeepEqual(step.Started.Input[len(step.Started.Input)-len(e.pending.start.Input):], e.pending.start.Input)) {
		return nil
	}
	key := e.pending.start.CommandID
	e.inputReceipt(e.pending, inputRecorded)
	e.pending = nil
	e.cfg.Emit(Block{ID: "pending:" + key, Kind: KindPending, Done: true})
	return e.flushLaterLocked()
}

func (e *Engine) flushLaterLocked() error {
	for len(e.later) > 0 {
		line := e.later[0]
		_, err := e.handle().EnqueueItem(context.Background(), dashi.ContextItem{
			ID:      line.id,
			Origin:  dashi.ContextUser,
			Message: dashi.Say(line.text).Input[0],
		})
		if err != nil {
			return fmt.Errorf("queued input %q was retained: %w", line.text, err)
		}
		e.later = e.later[1:]
	}
	return nil
}

func (e *Engine) holdLineLocked(text string) {
	id := dashi.NewID()
	e.later = append(e.later, pendingLine{id: id, text: text})
	e.cfg.Emit(Block{ID: "s:" + id, Kind: KindSteer, Text: text, State: SteerNext})
}

func (e *Engine) showPending() {
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	if e.pending == nil {
		return
	}
	e.cfg.Emit(Block{
		ID:   "pending:" + e.pending.start.CommandID,
		Kind: KindPending,
		Text: joinMessages(e.pending.start.Input),
	})
}
