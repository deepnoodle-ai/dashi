package dashi

import (
	"context"
	"errors"
	"fmt"
	"time"
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
// answer, and nowhere else, so a steer never lands between a tool call and
// its result. Steers must be cheap: it runs before every model call.
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
// any run again. In-doubt steers were in a write whose outcome is unknown:
// the record may hold them, so they may go only to a run of the same turn,
// where the record removes duplicates, until reconciled with the reloaded
// record.
type SteeringReleaser interface {
	Release(ctx context.Context, turnID string, unrecorded, inDoubt []string)
}

// Steer is one message for a running turn.
type Steer struct {
	// Item is the message and its lifetime. Item.ID is required, and an
	// empty Origin means ContextUser.
	Item ContextItem
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
// record now holds (recorded), and those in a write of unknown outcome.
type steerState struct {
	claimed  []string
	seen     map[string]bool
	recorded map[string]bool
	inDoubt  map[string]bool
	// warned holds the steers already warned about in this run, so a
	// durable source that keeps returning an invalid steer is reported
	// once, not before every model call.
	warned map[string]bool
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
// its source.
func (r *runtime) waitingSteers(s *replayState) []ContextItem {
	source := r.opts.steering
	if source == nil {
		return nil
	}
	steers, err := source.Steers(r.ctx, r.turn.ID)
	if err != nil {
		if r.ctx.Err() == nil {
			r.warnSteering(fmt.Sprintf("steering source failed: %v", err))
		}
		return nil
	}
	var fresh []ContextItem
	var again []string
	batch := map[string]bool{}
	for _, steer := range steers {
		item := cloneContextItem(steer.Item)
		if item.ID == "" {
			r.warnSteerOnce("", "steer skipped: it has no ID")
			continue
		}
		r.steers.claim(item.ID)
		if batch[item.ID] {
			continue
		}
		batch[item.ID] = true
		if err := validateContext(item); err != nil {
			r.warnSteerOnce(
				item.ID,
				fmt.Sprintf("steer %s skipped: %v", item.ID, err),
			)
			continue
		}
		if recorded, ok := s.contextIDs[item.ID]; ok {
			if sameJSONValue(recorded, item) {
				again = append(again, item.ID)
				continue
			}
			r.warnSteerOnce(item.ID, fmt.Sprintf(
				"steer %s skipped: the turn records a different item with its ID",
				item.ID,
			))
			continue
		}
		fresh = append(fresh, item)
	}
	if len(again) > 0 {
		r.steersDelivered(again)
	}
	return fresh
}

// commitSteers records a step that carries steers and settles the steers'
// fate from the commit's result.
func (r *runtime) commitSteers(step Step, steers []ContextItem) *Outcome {
	return r.afterSteerWrite(steers, r.commit(step))
}

// afterSteerWrite acknowledges steers the record now holds. When the write
// failed, a confirmed rejection leaves the steers unrecorded; any other
// failure leaves them in doubt, since the record may hold them.
func (r *runtime) afterSteerWrite(steers []ContextItem, o *Outcome) *Outcome {
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
	if errors.Is(err, ErrRevisionConflict) || errors.Is(err, ErrCommandConflict) {
		return true
	}
	var ce *CommitError
	return errors.As(err, &ce) && ce.Persistence == PersistenceRejected
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
	if err := r.opts.steering.Delivered(ctx, r.turn.ID, ids); err != nil {
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

// steersWait records why waiting steers could not supersede a candidate
// answer. They stay with their source for the next turn.
func (r *runtime) steersWait(s *replayState, n int) {
	why := "a limit, the deadline, or a pause"
	if !s.roomForNormalCall() {
		why = fmt.Sprintf(
			"the model attempt limit (%d of %d used, with the last reserved "+
				"for a final answer)",
			s.ModelAttempts,
			s.Agent.Limits.ModelAttempts,
		)
		if s.Agent.Limits.NoReservedFinalAnswer {
			why = fmt.Sprintf(
				"the model attempt limit (%d of %d used)",
				s.ModelAttempts,
				s.Agent.Limits.ModelAttempts,
			)
		}
	}
	r.warnSteering(fmt.Sprintf(
		"%d steer(s) could not supersede the answer: no further model call "+
			"can happen because of %s; they stay with their source",
		n,
		why,
	))
}

// skipRecordedItems drops context items whose ID the turn already records.
// A repeat with the same content was already delivered; one with different
// content is dropped with a warning, since an ID names one delivery.
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
		if !sameJSONValue(old, item) {
			r.pendingWarnings = append(r.pendingWarnings, Warning{
				Code: WarningDeliveryDropped,
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

// newDeliveries validates a Deliver command's items and drops those the
// turn already records with the same content. An ID repeated with different
// content is an error, since an ID names one delivery.
func newDeliveries(s *replayState, items []ContextItem) ([]ContextItem, error) {
	out := make([]ContextItem, 0, len(items))
	batch := map[string]bool{}
	for i, item := range items {
		if err := validateContext(item); err != nil {
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
			if !sameJSONValue(old, item) {
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
