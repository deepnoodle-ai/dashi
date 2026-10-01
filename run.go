package dashi

import (
	"context"
	"errors"
	"iter"
	"sync/atomic"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Runner runs turns with the long-lived services they need. It holds
// services only, so one value can serve many turns and agents: each turn's
// agent goes on its Start and is recorded, per-call controls are RunOption
// values, and each turn's state lives in its Turn.
type Runner struct {
	// Client prepares and sends model requests, and fills a Start agent's
	// empty provider, API, and model. It is required only for those: a Start
	// or Continue that would call the model, or a Start whose agent leaves
	// any of the three empty. A Start with an already-closed pause, whose
	// agent names all three, records the turn without one (see WithPause),
	// and Resolve, AddMessages, and Cancel record facts with a Recorder alone.
	// When set, it must match the turn's provider and API.
	Client llm.Client
	// Tools implements the tools a turn may call. It is resolved before each
	// model call, so a dynamic toolset can change between calls; the turn's
	// Agent.Tools and Agent.NoTools select from it by name.
	Tools tool.Toolset
	// Recorder acknowledges steps. When nil, Run uses an in-memory recorder
	// for that call; pass the returned Turn to a later Continue.
	Recorder Recorder
	// Hooks decide and shape model calls, tool calls, and final answers.
	// With none, every bound local and external tool runs. Supply the same
	// hooks on every command for a turn; WithHooks adds per-command extras.
	Hooks Hooks
	// Observer receives every committed step, plus provisional deltas and
	// tool progress, which it may drop (see Observer). When it is set, Run
	// requests streamed model output and delivers tool.Report snapshots so
	// the Observer sees work as it happens; Stream always does.
	Observer Observer
	// OnContextDone controls whether an ended host context pauses or cancels.
	OnContextDone ContextDone
	// ContentLoader supplies the bytes of content.Ref sources. When nil, a
	// turn that holds a reference fails before its first model request.
	ContentLoader ContentLoader
	// ContentLimits bounds the bytes that references add to a model request.
	ContentLimits ContentLimits

	// CacheKey is the fallback prompt cache key, such as a conversation ID,
	// so the turns of one conversation share the provider's cache. A model
	// call uses it when the agent sets no Settings.PromptCache.Key and the
	// model's catalog entry sets CacheKeyDefault. A session.Session sets it
	// to the session ID.
	CacheKey string
}

// RunOption adjusts one Run or Stream call.
type RunOption func(*runOptions)

// runOptions holds per-call controls. The stream fields are set only by Run
// and Stream, so a streamed model request always has a consumer for its
// deltas: an Observer, a Stream range, or both.
type runOptions struct {
	owner      string
	pause      <-chan struct{}
	stream     bool
	streamSink func(Event)
	hooks      Hooks
	steering   Steering
}

// WithOwner forwards owner to recorder commits for host lease fencing.
func WithOwner(owner string) RunOption {
	return func(o *runOptions) {
		o.owner = owner
	}
}

// WithPause requests a stop before the next model or tool call once pause is
// closed. Closing it does not interrupt work already in flight.
//
// A Start with an already-closed pause admits a turn without running it:
// the turn records its Start step and an interrupted stop ("pause
// requested"), and no model request is prepared or sent. It needs no Client
// when the agent names its provider, API, and model. Next then gives
// NextContinue; a later Continue runs the turn without counting an
// interruption, since none of its work was cut short, and a Cancel, which
// needs no Client, ends it.
// A host uses this to fix the admitted request in the record before any paid
// request runs.
func WithPause(pause <-chan struct{}) RunOption {
	return func(o *runOptions) {
		o.pause = pause
	}
}

// ContextDone selects the meaning of an ended Go context for one run.
type ContextDone string

const (
	// ContextInterrupt leaves the turn resumable and is the default.
	ContextInterrupt ContextDone = "interrupt"
	// ContextCancel ends the turn as an explicit Cancel command would.
	ContextCancel ContextDone = "cancel"
)

// HistoryMismatch selects how Continue treats prior history that differs
// from the history fingerprinted at Start.
type HistoryMismatch string

const (
	// HistoryMismatchWarn records WarningHistoryMismatch on the continued step
	// and runs with the supplied history. It is the default.
	HistoryMismatchWarn HistoryMismatch = "warn"
	// HistoryMismatchReject rejects the Continue with ErrHistoryMismatch before
	// any step is committed, so the turn stays resumable once the host supplies
	// the history it started with.
	HistoryMismatchReject HistoryMismatch = "reject"
)

// Stream advances a turn when iteration begins, requesting streamed model
// output and tool progress. Events carry committed steps, model deltas, and
// tool.Report snapshots. A fully read sequence ends with one Outcome event. A
// rejected command yields only an error; a write the recorder did not
// acknowledge yields the Outcome event and its *CommitError together.
// Breaking the range ends the context. By default, the turn records an
// interruption; ContextCancel on the Runner makes the break a final cancel.
// Reload the turn by ID to inspect committed facts, since no final event
// follows a break. A second range yields ErrStreamConsumed instead of
// repeating work.
func (r Runner) Stream(
	ctx context.Context,
	turn Turn,
	command Command,
	opts ...RunOption,
) iter.Seq2[Event, error] {
	var started atomic.Bool
	return func(yield func(Event, error) bool) {
		if !started.CompareAndSwap(false, true) {
			yield(Event{}, ErrStreamConsumed)
			return
		}
		streamCtx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		stopped := false
		o := newRunOptions(opts)
		o.stream = true
		o.streamSink = func(event Event) {
			if stopped {
				return
			}
			if !yield(event, nil) {
				stopped = true
				cancel(errors.New("dashi: stream consumer stopped"))
			}
		}
		out, err := r.run(streamCtx, turn, command, o)
		if stopped {
			return
		}
		if err != nil && !Recorded(err) {
			yield(Event{}, err)
			return
		}
		yield(Event{TurnID: out.Turn.ID, Outcome: &out}, err)
	}
}

// Run advances one turn from an explicit command. The Outcome is always the
// latest known state, and the error says whether the command applied:
//
//   - nil: every step was recorded. Execution failures, limits, and waits
//     are results, reported in Outcome.Stop with any partial work.
//   - ErrRevisionConflict or ErrCommandConflict (check with errors.Is, which
//     also matches them inside a *CommitError): another worker owns or
//     advanced the turn, or the command ID was reused. Reload the turn.
//   - *CommitError: the run executed, but CommitError.Step was not saved,
//     or may have been. The Outcome holds the steps this run knows were
//     saved; see CommitError.
//   - ErrRecordInvalid (which also matches ErrInvalidTurn): steps this run
//     recorded fail replay, a bug in the runner or recorder. Something was
//     recorded; the Outcome holds the steps without a view.
//   - any other error: the command was rejected and nothing was recorded.
//     The Outcome is the caller's turn and its view, unchanged. An invalid
//     input turn gives ErrInvalidTurn alone.
//
// Run requests streamed model output only when the Runner has an Observer,
// which is then the only consumer of deltas.
func (r Runner) Run(
	ctx context.Context,
	turn Turn,
	command Command,
	opts ...RunOption,
) (Outcome, error) {
	o := newRunOptions(opts)
	o.stream = r.Observer != nil
	return r.run(ctx, turn, command, o)
}

// run applies a command. A command rejected before any commit returns the
// caller's turn and its view, so a loop that reassigns its Outcome keeps
// the turn.
func (r Runner) run(
	ctx context.Context,
	turn Turn,
	command Command,
	opts runOptions,
) (Outcome, error) {
	out, err := r.apply(ctx, turn, command, opts)
	if err != nil && !Recorded(err) {
		out = restingOutcome(turn)
	}
	return out, err
}

// Recorded reports whether err, returned by Run or Stream, came after the
// run began recording: a *CommitError with a Step, or ErrRecordInvalid. The
// Outcome then holds what was recorded, and Stream yields its Outcome event
// together with err. Any other error rejected the command before anything
// was recorded, and the Outcome holds the caller's turn, unchanged.
func Recorded(err error) bool {
	var ce *CommitError
	return errors.As(err, &ce) && ce.Step != nil ||
		errors.Is(err, ErrRecordInvalid)
}

func newRunOptions(opts []RunOption) runOptions {
	var o runOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
