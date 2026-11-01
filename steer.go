package dashi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Inbox holds the commands that reach a turn from outside its run: tool
// results and approvals (Resolve), raises (Continue), AddMessages, Cancel,
// and steers, which a person sends while a turn runs, such as "use the
// staging database" or "stop, wrong file". Set Runner.Inbox, or pass one
// to a run with WithInbox; MemoryInbox is a ready-made in-memory inbox.
//
// The turn record is the authority on what was delivered. An inbox returns
// each command from Waiting until Dashi acknowledges it with Delivered, and
// Dashi skips a command whose ID the turn already records, so a command
// reaches a turn's record at most once even when a write fails or a worker
// crashes. A steer's command ID is its Item.ID. Command IDs that start
// with "continue:" are reserved for the Continue commands Runner.Advance
// derives, and Advance refuses them.
//
// During a run, Dashi calls Waiting before each model call and at each
// candidate final reply, and nowhere else, so a steer never lands between a
// tool call and its result. A Cancel ends the run at once, unless the
// turn's output is recorded, when it is refused; other commands wait for
// Runner.Advance. Waiting must be cheap: it runs before every model call.
// An inbox that also implements InboxNotifier is asked again
// when it signals, so an urgent steer can cut a model call short. Dashi
// never calls one inbox's methods concurrently from one run.
type Inbox interface {
	// Waiting returns the commands waiting for turnID, oldest first. Each
	// needs a command ID.
	Waiting(ctx context.Context, turnID string) ([]Command, error)
	// Delivered acknowledges commands the turn's record now holds, or that
	// the turn refused, by command ID. It must be idempotent: Dashi calls
	// it again for a command it finds already recorded, and after an error.
	Delivered(ctx context.Context, turnID string, ids []string) error
}

// InboxReleaser is implemented by inboxes that claim the steers they
// return, as MemoryInbox does. Dashi calls Release once when a run that
// fetched steers ends. Unrecorded steers were never written and are free to
// go to any run again. In-doubt steers were in a write whose result is
// unknown: the record may hold them, so they may go only to a run of the
// same turn, where the record removes duplicates, until checked against the
// reloaded record.
type InboxReleaser interface {
	Release(ctx context.Context, turnID string, unrecorded, inDoubt []string)
}

// InboxNotifier is implemented by inboxes that can wake a turn's run or
// its Drive, as MemoryInbox does. Ready(turnID) is a coalesced hint that
// means "check again": a command for the turn may be waiting. A run
// watches it, calls Waiting when it fires, and cuts short the model call in
// flight for a new urgent steer or ends the run for a Cancel. Drive waits
// on it between passes. Signal only after the command is visible to
// Waiting. An inbox without Ready still delivers, at the next safe point or
// the next poll.
type InboxNotifier interface {
	// Ready returns the turn's channel, or a nil channel when the inbox
	// never signals, and an unwatch func, which must not be nil. Dashi
	// calls unwatch exactly once per Ready call, even for a nil channel,
	// when it stops watching: at the end of each run and each Drive. The
	// run and Drive of one turn may watch at the same time, so an inbox
	// that keeps one channel per turn counts its watchers and drops the
	// channel when the count reaches zero. A watcher calls Waiting before
	// it waits, so a signal sent while no one watched is not needed.
	Ready(turnID string) (ready <-chan struct{}, unwatch func())
}

// InboxRefuser is implemented by inboxes that tell senders when a turn
// does not take their command: the turn has ended (ErrTurnEnded), or the
// command does not apply, such as a result for an unknown call or a raise
// on a turn not stopped at a limit. Runner.Advance calls Refused before it
// acknowledges such a command with Delivered, so the host can tell its
// caller the command came too late or was wrong.
type InboxRefuser interface {
	Refused(ctx context.Context, turnID string, id string, reason error)
}

// Steer is one message for a running turn. It is the mid-run form of
// AddMessages, and a Command whose ID is Item.ID; it reaches a turn only
// through an Inbox.
type Steer struct {
	// Item is the message and its lifetime. Item.ID is required, and an
	// empty Origin means ContextUser.
	Item ContextItem `json:"item,omitzero"`
	// Urgent asks Dashi to cut short the model call in flight, when the
	// inbox is an InboxNotifier and the call can be replaced within the
	// turn's limits; the next call sees the partial response and the steer.
	// Urgent is not recorded: the record shows its effect, a model call
	// finished with InterruptSteering. It never interrupts a running tool.
	Urgent bool `json:"urgent,omitempty"`
}

func (s Steer) commandID() string {
	return s.Item.ID
}

// WithInbox delivers commands from inbox into this run's turn, in place of
// Runner.Inbox. WithInbox(nil) turns the inbox off for the run. Use one
// inbox for one active run of a turn at a time.
func WithInbox(inbox Inbox) RunOption {
	return func(o *runOptions) {
		o.inbox = inbox
		o.inboxSet = true
	}
}

// inboxTimeout bounds each acknowledgment and release, which run even
// after the run's context has ended.
const inboxTimeout = 5 * time.Second

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
	// durable inbox that keeps returning an invalid steer is reported
	// once, not before every model call.
	warned map[string]bool
	// sourceFailed records that a failing inbox was already reported.
	sourceFailed bool
	// urgentUnheard and declined record that the run already warned that
	// its inbox cannot signal urgent steers, or that a call could not be
	// cut short. Each is reported once per run.
	urgentUnheard bool
	declined      bool
	// skipDeclined records that the run already warned that urgent steers
	// did not skip tool calls because no further model call can happen.
	skipDeclined bool
	// refused holds the inbox Cancels this run already refused.
	refused map[string]bool
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
// A failing inbox or an invalid steer never fails the turn: it is
// recorded as a WarningInbox on the next step, and the steer stays in the
// inbox. A waiting Cancel ends the run (see cancelFromInbox). Steers wait
// while a provider pause leaves a provider tool call unfinished, and reach
// the call after its continuation.
func (r *runtime) waitingSteers(s *replayState) []ContextItem {
	source := r.opts.inbox
	if source == nil || s.pausedProviderTool {
		return nil
	}
	unlock := r.lockSource()
	commands, err := source.Waiting(r.ctx, r.turn.ID)
	steers, cancel := splitWaiting(commands)
	var fresh []Steer
	var again []string
	var invalid []steerProblem
	if err == nil {
		fresh, again, invalid = classifySteers(s.contextIDs, steers)
		r.listen.keepUrgent(fresh)
	}
	unlock()
	if err != nil {
		// Reported once per run: the inbox is asked before every model
		// call and at every candidate reply.
		if r.ctx.Err() == nil && !r.steers.sourceFailed {
			r.steers.sourceFailed = true
			r.warnInbox(fmt.Sprintf("inbox failed: %v", err))
		}
		return nil
	}
	for _, steer := range steers {
		if steer.Item.ID != "" {
			r.steers.claim(steer.Item.ID)
		}
	}
	if cancel != nil {
		r.cancelFromInbox(s, *cancel)
	}
	// A run that ended, by this Cancel or otherwise, delivers no steers:
	// they stay in the inbox for the next turn.
	if r.ctx.Err() != nil {
		return nil
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
					"inbox does not signal them (see InboxNotifier)",
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

// classifySteers sorts what an inbox returned against the turn's recorded
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
	r.markDelivered(steers)
	return r.afterSteerWrite(steers, r.commit(step))
}

// markDelivered names steers the next commit records, for
// CommitRequest.Delivered.
func (r *runtime) markDelivered(steers []ContextItem) {
	for _, item := range steers {
		r.pendingDelivered = append(r.pendingDelivered, DeliveredCommand{
			ID:   item.ID,
			Kind: CommandSteer,
		})
	}
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

// steersDelivered acknowledges recorded steers to the inbox. It runs
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
	r.inboxDelivered(ids)
}

// inboxDelivered acknowledges recorded commands to the inbox.
func (r *runtime) inboxDelivered(ids []string) {
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(r.ctx),
		inboxTimeout,
	)
	defer cancel()
	unlock := r.lockSource()
	err := r.opts.inbox.Delivered(ctx, r.turn.ID, ids)
	unlock()
	if err != nil {
		r.warnInbox(fmt.Sprintf("inbox acknowledgment failed: %v", err))
	}
}

// releaseSteers hands a claiming inbox back the steers this run fetched
// but did not record.
func (r *runtime) releaseSteers() {
	releaser, ok := r.opts.inbox.(InboxReleaser)
	if !ok || len(r.steers.claimed) == 0 {
		return
	}
	var unrecorded, inDoubt []string
	for _, id := range r.steers.claimed {
		switch {
		case r.steers.recorded[id]:
		case r.steers.inDoubt[id]:
			inDoubt = append(inDoubt, id)
			if r.opts.inDoubt != nil {
				r.opts.inDoubt[id] = true
			}
		default:
			unrecorded = append(unrecorded, id)
		}
	}
	if len(unrecorded) == 0 && len(inDoubt) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(r.ctx),
		inboxTimeout,
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
	r.warnInbox(message)
}

func (r *runtime) warnInbox(message string) {
	r.pendingWarnings = append(r.pendingWarnings, Warning{
		Code:    WarningInbox,
		Message: message,
	})
}

// splitWaiting separates an inbox's waiting commands into its steers, in
// order, and its first Cancel. The run takes no other command: they wait
// for Runner.Advance.
func splitWaiting(commands []Command) ([]Steer, *Cancel) {
	var steers []Steer
	var cancel *Cancel
	for _, c := range commands {
		switch v := c.(type) {
		case Steer:
			steers = append(steers, v)
		case Cancel:
			if cancel == nil {
				cancel = &v
			}
		}
	}
	return steers, cancel
}

// inboxCancel is the cause a run's context ends with when its inbox holds
// a Cancel. contextStop records it as the Cancel command would.
type inboxCancel struct {
	command Cancel
}

func (e *inboxCancel) Error() string {
	return "dashi: the inbox holds a Cancel for the turn"
}

// errCancelAfterOutput refuses a Cancel for a turn whose output is
// recorded: the turn completes instead.
var errCancelAfterOutput = errors.New("dashi: turn cannot cancel: its output is recorded")

// cancelFromInbox ends the run for a Cancel found in its inbox. The run
// records the canceled stop under the Cancel's ID once it reaches a safe
// point, as with ContextCancel. A Cancel without an ID cannot be recorded
// and is warned about once; a Cancel after the output is refused.
func (r *runtime) cancelFromInbox(s *replayState, c Cancel) {
	switch {
	case r.cancelRun == nil:
	case c.CommandID == "":
		r.warnSteerOnce("", "cancel skipped: it has no command ID")
	case s.Output != nil:
		r.refuseCancel(c)
	default:
		r.cancelRun(&inboxCancel{command: c})
	}
}

// refuseCancel refuses an inbox Cancel that arrived after the output was
// recorded: it warns, tells an InboxRefuser, and acknowledges the Cancel,
// once per run.
func (r *runtime) refuseCancel(c Cancel) {
	if r.steers.refused[c.CommandID] {
		return
	}
	if r.steers.refused == nil {
		r.steers.refused = map[string]bool{}
	}
	r.steers.refused[c.CommandID] = true
	r.warnInbox(fmt.Sprintf(
		"inbox command %s not applied: %v", c.CommandID, errCancelAfterOutput,
	))
	if refuser, ok := r.opts.inbox.(InboxRefuser); ok {
		ctx, cancel := context.WithTimeout(
			context.WithoutCancel(r.ctx),
			inboxTimeout,
		)
		unlock := r.lockSource()
		refuser.Refused(ctx, r.turn.ID, c.CommandID, errCancelAfterOutput)
		unlock()
		cancel()
	}
	r.inboxDelivered([]string{c.CommandID})
}

// cancelStop records the stop for a Cancel from the inbox and acknowledges
// it. A turn whose output was recorded before the Cancel took effect
// cannot be canceled: the Cancel is refused and the turn completes.
func (r *runtime) cancelStop(c Cancel) RunResult {
	state, err := r.replay()
	if err == nil && state.Output != nil {
		r.refuseCancel(c)
		return r.completeReply(state)
	}
	hash, err := commandHash(c)
	if err != nil {
		return r.stop(StopCanceled, c.Reason)
	}
	r.pendingDelivered = append(r.pendingDelivered, DeliveredCommand{
		ID:   c.CommandID,
		Kind: CommandCancel,
	})
	o := r.commit(Step{
		Kind:        StepTurnStopped,
		CommandID:   c.CommandID,
		CommandHash: hash,
		Stop:        &Stop{Kind: StopCanceled, Reason: c.Reason},
	})
	if o != nil {
		return *o
	}
	r.inboxDelivered([]string{c.CommandID})
	return r.current()
}

func (r *runtime) warnSteering(message string) {
	r.pendingWarnings = append(r.pendingWarnings, Warning{
		Code:    WarningSteering,
		Message: message,
	})
}

// steersWait records why waiting steers could not replace a candidate
// reply. They stay in the inbox for the next turn.
func (r *runtime) steersWait(s *replayState, n int) {
	r.warnSteering(fmt.Sprintf(
		"%d steer(s) could not supersede the answer: no further model call "+
			"can happen because of %s; they stay in the inbox",
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
