package dashi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Steering is a source of messages a person sends while a turn runs, such
// as "use the staging database" or "stop, wrong file". Pass one to a run
// with WithSteering; Inbox is a ready-made in-memory source.
//
// The turn record is the authority on what was delivered. A source returns
// each steer from Steers until Dashi acknowledges it with Delivered, and
// Dashi skips a steer whose ID the turn already records, so a steer reaches
// a turn's record at most once even when a write fails or a worker crashes.
//
// Dashi calls Steers before each model call and at each candidate final
// reply, and nowhere else, so a steer never lands between a tool call and
// its result. Steers must be cheap: it runs before every model call. A
// source that also implements SteeringNotifier is asked again when it
// signals, so an urgent steer can cut a model call short. Dashi never calls
// one source's methods concurrently from one run.
type Steering interface {
	// Steers returns the steers waiting for turnID, oldest first. Each
	// steer's Item.ID is required.
	Steers(ctx context.Context, turnID string) ([]Steer, error)
	// Delivered acknowledges steers the turn's record now holds. It must be
	// idempotent: Dashi calls it again for a steer it finds already
	// recorded, and after an error.
	Delivered(ctx context.Context, turnID string, ids []string) error
}

// SteeringReleaser is implemented by sources that claim the steers they
// return, as Inbox does. Dashi calls Release once when a run that fetched
// steers ends. Unrecorded steers were never written and are free to go to
// any run again. In-doubt steers were in a write whose result is unknown:
// the record may hold them, so they may go only to a run of the same turn,
// where the record removes duplicates, until checked against the reloaded
// record.
type SteeringReleaser interface {
	Release(ctx context.Context, turnID string, unrecorded, inDoubt []string)
}

// SteeringNotifier is implemented by sources that can wake a run, as Inbox
// does. Ready is a coalesced hint that an urgent steer may be waiting, for
// the one run the source serves: a run with such a source watches Ready,
// calls Steers when it fires, and cuts short the model call in flight for a
// new urgent steer. Signal only after the steer is visible to Steers. A
// source without Ready still delivers urgent steers, at the next safe point.
type SteeringNotifier interface {
	// Ready returns the same channel on every call, or nil when the source
	// never signals.
	Ready() <-chan struct{}
}

// Steer is one message for a running turn.
type Steer struct {
	// Item is the message and its lifetime. Item.ID is required, and an
	// empty Origin means ContextUser.
	Item ContextItem
	// Urgent asks Dashi to cut short the model call in flight, when the
	// source is a SteeringNotifier and the call can be replaced within the
	// turn's limits; the next call sees the partial response and the steer.
	// Urgent is not recorded: the record shows its effect, a model call
	// finished with InterruptSteering. It never interrupts a running tool.
	Urgent bool
}

// WithSteering delivers steers from source into this run's turn. Use one
// source for one active run at a time.
func WithSteering(source Steering) RunOption {
	return func(o *runOptions) {
		o.steering = source
	}
}

// steeringTimeout bounds each acknowledgment and release, which run even
// after the run's context has ended.
const steeringTimeout = 5 * time.Second

// steerNotAccepted is the reason a submission superseded by a steer reports
// to the model.
const steerNotAccepted = "the user sent new input. Read it, then submit again."

// steerState tracks one run's steers: those it fetched (claimed), those the
// record now holds (recorded), and those in a write whose result is unknown.
type steerState struct {
	claimed  []string
	seen     map[string]bool
	recorded map[string]bool
	inDoubt  map[string]bool
	// warned holds the steers already warned about in this run, so a
	// durable source that keeps returning an invalid steer is reported
	// once, not before every model call.
	warned map[string]bool
	// sourceFailed records that a failing source was already reported.
	sourceFailed bool
	// urgentUnheard and declined record that the run already warned that
	// its source cannot signal urgent steers, or that a call could not be
	// cut short. Each is reported once per run.
	urgentUnheard bool
	declined      bool
	// skipDeclined records that the run already warned that urgent steers
	// did not skip tool calls because no further model call can happen.
	skipDeclined bool
}

func (st *steerState) claim(id string) {
	if st.seen == nil {
		st.seen = map[string]bool{}
	}
	if !st.seen[id] {
		st.seen[id] = true
		st.claimed = append(st.claimed, id)
	}
}

// waitingSteers fetches the waiting steers and returns those the turn does
// not yet record, in order. A steer already recorded is acknowledged again.
// A failing source or an invalid steer never fails the turn: it is
// recorded as a WarningSteering on the next step, and the steer stays with
// its source. Steers wait while a provider pause leaves a provider tool
// call unfinished, and reach the call after its continuation.
func (r *runtime) waitingSteers(s *replayState) []ContextItem {
	source := r.opts.steering
	if source == nil || s.pausedProviderTool {
		return nil
	}
	unlock := r.lockSource()
	steers, err := source.Steers(r.ctx, r.turn.ID)
	var fresh []Steer
	var again []string
	var invalid []steerProblem
	if err == nil {
		fresh, again, invalid = classifySteers(s.contextIDs, steers)
		r.listen.keepUrgent(fresh)
	}
	unlock()
	if err != nil {
		// Reported once per run: the source is asked before every model
		// call and at every candidate reply.
		if r.ctx.Err() == nil && !r.steers.sourceFailed {
			r.steers.sourceFailed = true
			r.warnSteering(fmt.Sprintf("steering source failed: %v", err))
		}
		return nil
	}
	for _, steer := range steers {
		if steer.Item.ID != "" {
			r.steers.claim(steer.Item.ID)
		}
	}
	for _, p := range invalid {
		r.warnSteerOnce(p.id, p.message)
	}
	if len(again) > 0 {
		r.steersDelivered(again)
	}
	items := make([]ContextItem, 0, len(fresh))
	for _, steer := range fresh {
		items = append(items, steer.Item)
		if steer.Urgent && r.listen == nil && !r.steers.urgentUnheard {
			r.steers.urgentUnheard = true
			r.warnSteering(
				"urgent steers wait for the next model call: the steering " +
					"source does not signal them (see SteeringNotifier)",
			)
		}
	}
	if len(items) == 0 {
		return nil
	}
	return items
}

// steerProblem is a steer that cannot be delivered, and why.
type steerProblem struct {
	id      string
	message string
}

// classifySteers sorts what a source returned against the turn's recorded
// context items: fresh steers the turn does not record, once each and in
// order; IDs the turn records with the same content (again); and steers
// that cannot be delivered (invalid). It is pure, so the run and the
// listener share one definition of a waiting steer.
func classifySteers(
	record map[string]ContextItem,
	steers []Steer,
) (fresh []Steer, again []string, invalid []steerProblem) {
	batch := map[string]bool{}
	for _, steer := range steers {
		item := cloneContextItem(steer.Item)
		if item.ID == "" {
			invalid = append(invalid, steerProblem{
				message: "steer skipped: it has no ID",
			})
			continue
		}
		if batch[item.ID] {
			continue
		}
		batch[item.ID] = true
		if err := item.Validate(); err != nil {
			invalid = append(invalid, steerProblem{
				id:      item.ID,
				message: fmt.Sprintf("steer %s skipped: %v", item.ID, err),
			})
			continue
		}
		if recorded, ok := record[item.ID]; ok {
			if canonjson.SameValue(recorded, item) {
				again = append(again, item.ID)
				continue
			}
			invalid = append(invalid, steerProblem{
				id: item.ID,
				message: fmt.Sprintf(
					"steer %s skipped: the turn records a different item "+
						"with its ID",
					item.ID,
				),
			})
			continue
		}
		fresh = append(fresh, Steer{Item: item, Urgent: steer.Urgent})
	}
	return fresh, again, invalid
}

// commitSteers records a step that carries steers and decides what happens
// to the steers from the commit's result.
func (r *runtime) commitSteers(step Step, steers []ContextItem) *RunResult {
	return r.afterSteerWrite(steers, r.commit(step))
}

// afterSteerWrite acknowledges steers the record now holds. When the write
// failed, a confirmed rejection leaves the steers unrecorded; any other
// failure leaves them in doubt, since the record may hold them.
func (r *runtime) afterSteerWrite(steers []ContextItem, o *RunResult) *RunResult {
	ids := make([]string, 0, len(steers))
	for _, item := range steers {
		ids = append(ids, item.ID)
	}
	if o == nil {
		r.steersDelivered(ids)
		return nil
	}
	if !writeRejected(r.err) {
		if r.steers.inDoubt == nil {
			r.steers.inDoubt = map[string]bool{}
		}
		for _, id := range ids {
			r.steers.inDoubt[id] = true
		}
	}
	return o
}

// writeRejected reports whether a failed commit is known not to have been
// saved.
func writeRejected(err error) bool {
	if errors.Is(err, ErrStaleTurn) || errors.Is(err, ErrCommandIDReused) {
		return true
	}
	var ce *CommitError
	return errors.As(err, &ce) && ce.NothingWritten
}

// steersDelivered acknowledges recorded steers to the source. It runs
// inline, after the run's context may have ended; an error becomes a
// warning on the next step, and the next run acknowledges again.
func (r *runtime) steersDelivered(ids []string) {
	if len(ids) == 0 {
		return
	}
	if r.steers.recorded == nil {
		r.steers.recorded = map[string]bool{}
	}
	for _, id := range ids {
		r.steers.recorded[id] = true
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(r.ctx),
		steeringTimeout,
	)
	defer cancel()
	unlock := r.lockSource()
	err := r.opts.steering.Delivered(ctx, r.turn.ID, ids)
	unlock()
	if err != nil {
		r.warnSteering(fmt.Sprintf("steering acknowledgment failed: %v", err))
	}
}

// releaseSteers hands a claiming source back the steers this run fetched
// but did not record.
func (r *runtime) releaseSteers() {
	releaser, ok := r.opts.steering.(SteeringReleaser)
	if !ok || len(r.steers.claimed) == 0 {
		return
	}
	var unrecorded, inDoubt []string
	for _, id := range r.steers.claimed {
		switch {
		case r.steers.recorded[id]:
		case r.steers.inDoubt[id]:
			inDoubt = append(inDoubt, id)
		default:
			unrecorded = append(unrecorded, id)
		}
	}
	if len(unrecorded) == 0 && len(inDoubt) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(r.ctx),
		steeringTimeout,
	)
	defer cancel()
	releaser.Release(ctx, r.turn.ID, unrecorded, inDoubt)
}

// warnSteerOnce warns about one steer, keyed by its ID, once per run.
func (r *runtime) warnSteerOnce(id, message string) {
	if r.steers.warned[id] {
		return
	}
	if r.steers.warned == nil {
		r.steers.warned = map[string]bool{}
	}
	r.steers.warned[id] = true
	r.warnSteering(message)
}

func (r *runtime) warnSteering(message string) {
	r.pendingWarnings = append(r.pendingWarnings, Warning{
		Code:    WarningSteering,
		Message: message,
	})
}

// steersWait records why waiting steers could not replace a candidate
// reply. They stay with their source for the next turn.
func (r *runtime) steersWait(s *replayState, n int) {
	r.warnSteering(fmt.Sprintf(
		"%d steer(s) could not supersede the answer: no further model call "+
			"can happen because of %s; they stay with their source",
		n,
		noCallCause(s),
	))
}

// noCallCause names why no further model call can happen.
func noCallCause(s *replayState) string {
	if s.roomForNormalCall() {
		return "a limit, the deadline, or a pause"
	}
	if s.Agent.Limits.NoReservedFinalReply {
		return fmt.Sprintf(
			"the model call limit (%d of %d used)",
			s.ModelCalls,
			s.modelCallCap,
		)
	}
	return fmt.Sprintf(
		"the model call limit (%d of %d used, with the last reserved "+
			"for a final answer)",
		s.ModelCalls,
		s.modelCallCap,
	)
}

// skipRecordedItems drops context items whose ID the turn already records.
// A repeat with the same content was already added; one with different
// content is dropped with a warning, since an ID names one item.
func (r *runtime) skipRecordedItems(items []ContextItem) []ContextItem {
	var recorded map[string]ContextItem
	out := items[:0:0]
	for _, item := range items {
		if item.ID == "" {
			out = append(out, item)
			continue
		}
		if recorded == nil {
			recorded = recordedItems(r.turn)
		}
		old, ok := recorded[item.ID]
		if !ok {
			recorded[item.ID] = item
			out = append(out, item)
			continue
		}
		if !canonjson.SameValue(old, item) {
			r.pendingWarnings = append(r.pendingWarnings, Warning{
				Code: WarningMessagesDropped,
				Message: fmt.Sprintf(
					"context item %s dropped: the turn records a different "+
						"item with its ID",
					item.ID,
				),
			})
		}
	}
	return out
}

// newMessageItems validates an AddMessages command's items and drops those
// the turn already records with the same content. An ID repeated with
// different content is an error, since an ID names one item.
func newMessageItems(s *replayState, items []ContextItem) ([]ContextItem, error) {
	out := make([]ContextItem, 0, len(items))
	batch := map[string]bool{}
	for i, item := range items {
		if err := item.Validate(); err != nil {
			return nil, fmt.Errorf("dashi: context item %d: %w", i+1, err)
		}
		if item.ID == "" {
			out = append(out, cloneContextItem(item))
			continue
		}
		if batch[item.ID] {
			return nil, fmt.Errorf("dashi: context item ID %q repeats", item.ID)
		}
		batch[item.ID] = true
		if old, ok := s.contextIDs[item.ID]; ok {
			if !canonjson.SameValue(old, item) {
				return nil, fmt.Errorf(
					"dashi: the turn records a different context item with ID %q",
					item.ID,
				)
			}
			continue
		}
		out = append(out, cloneContextItem(item))
	}
	return out, nil
}

// recordedItems maps the turn's context item IDs to their items.
func recordedItems(t Turn) map[string]ContextItem {
	out := map[string]ContextItem{}
	for _, step := range t.Steps {
		for _, item := range step.Items() {
			if item.ID != "" {
				out[item.ID] = item
			}
		}
	}
	return out
}

// pausedMidProviderToolCall reports whether result is a provider pause that
// left a provider tool call unfinished, as an Anthropic pause_turn during a
// web search does. Such a call is like a tool call awaiting its result: a
// steer may not land between them, and providers reject a request that does
// not end with the paused result.
func pausedMidProviderToolCall(result llm.Result) bool {
	if result.Stop.Kind != llm.StopPause {
		return false
	}
	open := map[string]bool{}
	for _, part := range result.Message.Content {
		switch {
		case part.ProviderToolUse != nil && part.ProviderToolUse.Status != "completed" &&
			part.ProviderToolUse.Status != "failed":
			open[part.ProviderToolUse.ID] = true
		case part.ProviderToolResult != nil:
			delete(open, part.ProviderToolResult.CallID)
		}
	}
	return len(open) > 0
}
