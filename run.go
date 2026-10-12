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
	// empty provider, API, and model. Run needs it for Start and Continue.
	// Record needs it only when the Start's agent leaves any of the three
	// empty, and Resolve, AddMessages, and Cancel record steps with a
	// Recorder alone. When set, it must match the turn's provider and API.
	Client llm.Client
	// Tools implements the tools a turn may call. It is resolved before each
	// model call, so a dynamic toolset can change between calls; the turn's
	// Agent.Tools and Agent.NoTools select from it by name.
	Tools tool.Toolset
	// Recorder saves steps. When nil, Run uses an in-memory recorder
	// for that call; pass the returned Turn to a later Continue.
	Recorder Recorder
	// Hooks decide and shape model calls, tool calls, and final replies.
	// With none, every bound local and external tool runs. Supply the same
	// hooks on every command for a turn; WithHooks adds per-command extras.
	Hooks Hooks
	// Observer receives every committed step, plus provisional deltas and
	// tool progress, which it may drop (see Observer). When it is set, Run
	// requests streamed model output and passes on tool.Report snapshots so
	// the Observer sees work as it happens; Stream always does.
	Observer Observer
	// OnContextDone controls whether an ended host context pauses or cancels.
	OnContextDone ContextDone
	// ContentLoader supplies the bytes of content.Ref sources. When nil, a
	// turn that holds a reference fails before its first model request.
	ContentLoader ContentLoader
	// ContentLimits bounds the bytes that references add to a model request.
	ContentLimits ContentLimits

	// Inbox holds commands for turns from outside their runs: steers,
	// Cancel, and the tool results, approvals, raises, and messages that
	// Advance applies. A run reads it unless WithInbox replaces it.
	Inbox Inbox

	// CacheKey is the fallback prompt cache key, such as a conversation ID,
	// so the turns of one conversation share the provider's cache. A model
	// call uses it when the agent sets no Settings.PromptCache.Key and the
	// model's catalog entry sets CacheKeyDefault. A session.Session sets it
	// to the session ID.
	CacheKey string
}

// RunOption adjusts one Run, Stream, or Record call. Record uses only
// WithOwner and WithHooks.
type RunOption func(*runOptions)

// runOptions holds per-call controls. The stream fields are set only by Run
// and Stream, so a streamed model request always has a consumer for its
// deltas: an Observer, a Stream range, or both. record is set only by
// Record.
type runOptions struct {
	record     bool
	owner      string
	pause      <-chan struct{}
	stream     bool
	streamSink func(Event)
	hooks      Hooks
	inbox      Inbox
	// inboxSet records a WithInbox, which wins over Runner.Inbox even when
	// nil.
	inboxSet bool
	// history is the Continue history Advance uses (WithHistory).
	history []llm.Message
	// historyMismatch is the OnHistoryMismatch Advance gives the Continues
	// it runs that leave it empty (WithHistoryMismatch).
	historyMismatch HistoryMismatch
	// fresh means the turn passed to the run was just loaded from the
	// Recorder, so apply need not load it again.
	fresh bool
	// delivered lists the inbox command the run applies, for the first
	// commit's CommitRequest.Delivered.
	delivered []DeliveredCommand
	// warnings ride on the run's first committed step.
	warnings []Warning
	// asking runs the command in memory to ask whether a held turn may
	// send again (see askSend).
	asking *sendAsk
}

// WithOwner forwards owner to recorder commits for host lease fencing.
func WithOwner(owner string) RunOption {
	return func(o *runOptions) {
		o.owner = owner
	}
}

// WithPause requests a stop before the next model or tool call once pause is
// closed. Closing it does not interrupt work already in flight. A pause
// before the turn's first model call cut no work short, so the Continue
// after it does not count toward Limits.Resumes. To save a Start without
// running it, use Runner.Record.
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
// from the history whose hash was recorded at Start.
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
// tool.Report snapshots. A fully read sequence ends with one RunResult event. A
// rejected command yields only an error; a write the recorder did not
// acknowledge yields the RunResult event and its *CommitError together.
// Breaking the range ends the context. By default, the turn records an
// interrupted stop; ContextCancel on the Runner makes the break a final
// cancel. Reload the turn by ID to inspect committed steps, since no final
// event follows a break. A second range yields ErrStreamConsumed instead of
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
		yield(Event{TurnID: out.Turn.ID, Result: &out}, err)
	}
}

// Run advances one turn from an explicit command. The RunResult is always the
// latest known state, and the error says whether the command applied:
//
//   - nil: every step was recorded. Execution failures, limits, and waits
//     are results, reported in RunResult.Stop with any partial work.
//   - ErrStaleTurn or ErrCommandIDReused (check with errors.Is, which
//     also matches them inside a *CommitError): another worker owns or
//     advanced the turn, or the command ID was reused. Reload the turn.
//   - *CommitError: the run executed, but CommitError.Step was not saved,
//     or may have been. The RunResult holds the steps this run knows were
//     saved; see CommitError.
//   - ErrRecordInvalid (which also matches ErrInvalidTurn): steps this run
//     recorded fail the record checks, a bug in the runner or recorder.
//     Something was recorded; the RunResult holds the steps without a
//     view.
//   - any other error: the command was rejected and nothing was recorded.
//     The RunResult is the caller's turn and its view, unchanged. An invalid
//     input turn gives ErrInvalidTurn alone.
//
// Run requests streamed model output only when the Runner has an Observer,
// which is then the only consumer of deltas.
func (r Runner) Run(
	ctx context.Context,
	turn Turn,
	command Command,
	opts ...RunOption,
) (RunResult, error) {
	o := newRunOptions(opts)
	o.stream = r.Observer != nil
	return r.run(ctx, turn, command, o)
}

// Record records start as the turn's first step without running it: it
// commits the Start step and returns. It prepares and sends no model request,
// runs no tool, reads no steering, and loads no content. It needs no Client
// when the Start's agent names its provider, API, and model. Of the
// RunOption values, only WithOwner and WithHooks apply. A host uses it to
// save work before any paid request runs, for example to queue the turn.
//
// A turn whose only step is its Start is ready: Next gives NextContinue,
// a later Continue runs it without counting a resume (Limits.Resumes), and
// Cancel ends it. A retry with the same CommandID returns the recorded
// result, as Run does; a Run of the same Start does too, without running
// it, so run the turn with Continue. The errors are those of Run.
func (r Runner) Record(
	ctx context.Context,
	turn Turn,
	start Start,
	opts ...RunOption,
) (RunResult, error) {
	o := newRunOptions(opts)
	o.record = true
	return r.run(ctx, turn, start, o)
}

// run applies a command. A command rejected before any commit returns the
// caller's turn and its view, so a loop that reassigns its RunResult keeps
// the turn.
func (r Runner) run(
	ctx context.Context,
	turn Turn,
	command Command,
	opts runOptions,
) (RunResult, error) {
	out, err := r.apply(ctx, turn, command, opts)
	if err != nil && !Recorded(err) {
		out = unchangedResult(turn)
	}
	return out, err
}

// Recorded reports whether err, returned by Run, Stream, or Record, came after the
// run began recording: a *CommitError with a Step, or ErrRecordInvalid. The
// RunResult then holds what was recorded, and Stream yields its RunResult event
// together with err. Any other error rejected the command before anything
// was recorded, and the RunResult holds the caller's turn, unchanged.
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
