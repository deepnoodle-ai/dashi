package dashi

import (
	"context"
	"errors"
	"sync"

	"github.com/deepnoodle-ai/dashi/tool"
)

// errSteered is the private cause a model call's context ends with when an
// urgent steer cuts it short. The settlement tells it apart from the run's
// own context ending, which stops the run.
var errSteered = errors.New(
	"dashi: an urgent steer interrupted the model call",
)

// steeringInterruptedText is the reminder that follows a cut-off call's
// partial response in the next request, so the model reads the partial as
// unfinished rather than as its answer.
const steeringInterruptedText = "The user interrupted your previous response."

// steerListener is one run's watcher of a SteeringNotifier. Its goroutine
// is the only reader of Ready. The listener writes no run state and no
// warnings.
//
// Two locks keep a slow source from stalling the run. sourceMu serializes
// every call the run and the listener make to the source (U-1). mu guards
// the state they share (U-2) and is never held while calling the source,
// so commits never wait for a fetch. The lock order is sourceMu, then mu.
type steerListener struct {
	sourceMu sync.Mutex
	mu       sync.Mutex
	// recorded is the listener's snapshot of the turn's context items by
	// ID, refreshed after each commit, so it never interrupts for a steer
	// the record already holds.
	recorded map[string]ContextItem
	// urgent holds fresh urgent steers not yet recorded. FR-6 removes an ID
	// when it records the steer or when the source no longer returns it.
	urgent map[string]bool
	// interrupted holds the IDs that already cut a call short: each
	// interrupts at most once.
	interrupted map[string]bool
	// loopBack holds the IDs that already sent a request back to FR-6
	// before dispatch, so a steer the source drops loops back only once.
	loopBack map[string]bool
	// seen lists the IDs the listener's Steers calls claimed; the run
	// merges them into its claims after the join, before Release.
	seen    []string
	seenSet map[string]bool
	call    *steerableCall
	// fetchCancel ends the listener's Steers call in flight. The run calls
	// it when a model call settles and whenever it needs the source or the
	// listener's decision itself, so a slow source can delay a cut but
	// never stall the run.
	fetchCancel context.CancelFunc
	stop        context.CancelFunc
	finished    chan struct{}
}

// steerableCall is a model call the listener may cut short. It is
// registered after request preparation and before BeforeSend.
type steerableCall struct {
	cancel context.CancelCauseFunc
	// never is the reason the call can never be cut once handed off, such
	// as a hosted tool unsafe to repeat; empty when it can. Before the
	// hand-off nothing was sent, so a cut is always safe.
	never string
	// handedOff is set just before the request is handed to the client.
	// Until then a cut costs nothing, so it needs no budget check.
	handedOff bool
	// fits reports why a replacement call would not fit the turn's limits,
	// given the extra input it would carry, or "" when it fits. It is set
	// with handedOff and reads only state captured for the listener.
	fits func(extraInputTokens int) string
	// cut is set when the listener cancelled the call.
	cut bool
	// declined is the reason the listener last left the call running.
	declined string
}

// startSteering starts the listener when the run's source can signal.
func (r *runtime) startSteering(s *replayState) {
	notifier, ok := r.opts.steering.(SteeringNotifier)
	if !ok || r.listen != nil {
		return
	}
	ready := notifier.Ready()
	if ready == nil {
		return
	}
	ctx, stop := context.WithCancel(r.ctx)
	l := &steerListener{
		recorded:    map[string]ContextItem{},
		urgent:      map[string]bool{},
		interrupted: map[string]bool{},
		loopBack:    map[string]bool{},
		seenSet:     map[string]bool{},
		stop:        stop,
		finished:    make(chan struct{}),
	}
	for id, item := range s.contextIDs {
		l.recorded[id] = item
	}
	r.listen = l
	go l.run(ctx, r.opts.steering, r.turn.ID, ready)
}

// endSteering stops and joins the listener, merges its claims into the
// run's, and releases what the run did not record. It runs on every exit.
func (r *runtime) endSteering() {
	if l := r.listen; l != nil {
		l.stop()
		<-l.finished
		l.mu.Lock()
		for _, id := range l.seen {
			r.steers.claim(id)
		}
		l.mu.Unlock()
	}
	r.releaseSteers()
}

// lockSource serializes a run's call to the source with the listener's.
// It cuts the listener's fetch in flight short rather than wait for it.
func (r *runtime) lockSource() func() {
	l := r.listen
	if l == nil {
		return func() {}
	}
	l.lockSource()
	return l.sourceMu.Unlock
}

// registerCall makes a prepared model call one the listener may cut, and
// applies U-5: when an urgent steer arrived since FR-6 ran, it reports that
// the run should go back to FR-6 instead of sending, once per steer.
func (r *runtime) registerCall(
	cancel context.CancelCauseFunc,
	never string,
) (*steerableCall, bool) {
	l := r.listen
	if l == nil {
		return nil, false
	}
	l.lockSource()
	defer l.sourceMu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	back := false
	for id := range l.urgent {
		if !l.loopBack[id] {
			l.loopBack[id] = true
			back = true
		}
	}
	if back {
		return nil, true
	}
	c := &steerableCall{cancel: cancel, never: never}
	l.call = c
	return c, false
}

// cutBeforeHandOff reports whether the listener cut c before the request
// was handed to the client. Otherwise it marks c handed off, with fits as
// its replacement check from then on.
func (r *runtime) cutBeforeHandOff(
	c *steerableCall,
	fits func(int) string,
) bool {
	if c == nil {
		return false
	}
	l := r.listen
	l.lockSource()
	defer l.sourceMu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if c.cut {
		return true
	}
	c.handedOff = true
	c.fits = fits
	return false
}

// wasCut reports whether the listener cut c short.
func (r *runtime) wasCut(c *steerableCall) bool {
	if c == nil {
		return false
	}
	l := r.listen
	l.lockSource()
	defer l.sourceMu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	return c.cut
}

// unregisterCall ends the listener's hold on c once the call settles. The
// first reason the listener gave for leaving a call running becomes a
// warning on the next step, which is the call's settlement or the stop
// that ends the run. A run that is ending gets no warning, since no next
// model call of this run will deliver the steer.
func (r *runtime) unregisterCall(c *steerableCall) {
	if c == nil {
		return
	}
	why := r.listen.release(c)
	if why != "" && !r.steers.declined && r.ctx.Err() == nil {
		r.steers.declined = true
		r.warnSteering("an urgent steer did not interrupt the model call " +
			"because " + why + "; it waits for the next model call")
	}
}

// dropCall ends the listener's hold on c without a warning, for exits that
// never reach the call's settlement.
func (r *runtime) dropCall(c *steerableCall) {
	if c == nil {
		return
	}
	r.listen.release(c)
}

func (l *steerListener) run(
	ctx context.Context,
	source Steering,
	turnID string,
	ready <-chan struct{},
) {
	defer close(l.finished)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ready:
		}
		// A fetch the run cut short is tried again, so the signal is not
		// lost; the retry waits until the run is done with the source.
		for l.check(ctx, source, turnID) {
		}
	}
}

// check handles one signal: it fetches the waiting steers and, for a fresh
// urgent one, cuts the registered call short when that is allowed. It
// reports whether the run cut the fetch short, so it should be tried again.
func (l *steerListener) check(
	ctx context.Context,
	source Steering,
	turnID string,
) bool {
	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The cancel is published before the fetch can hold sourceMu, so any
	// fetch the run might wait for is one it can cut short.
	l.mu.Lock()
	l.fetchCancel = cancel
	l.mu.Unlock()
	l.sourceMu.Lock()
	defer l.sourceMu.Unlock()
	steers, err := source.Steers(fetchCtx, turnID)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fetchCancel = nil
	if err != nil {
		// FR-6 reports a failing source; the listener writes no warnings.
		return fetchCtx.Err() != nil && ctx.Err() == nil
	}
	for _, steer := range steers {
		if id := steer.Item.ID; id != "" && !l.seenSet[id] {
			l.seenSet[id] = true
			l.seen = append(l.seen, id)
		}
	}
	fresh, _, _ := classifySteers(l.recorded, steers)
	var ids []string
	var items []ContextItem
	for _, steer := range fresh {
		items = append(items, steer.Item)
		if !steer.Urgent {
			continue
		}
		l.urgent[steer.Item.ID] = true
		if !l.interrupted[steer.Item.ID] {
			ids = append(ids, steer.Item.ID)
		}
	}
	c := l.call
	if len(ids) == 0 || c == nil || c.cut {
		return false
	}
	why := ""
	if c.handedOff {
		why = c.never
		if why == "" {
			why = c.fits(estimatedItemTokens(items) + reminderTokens())
		}
	}
	if why != "" {
		c.declined = why
		return false
	}
	c.cut = true
	for _, id := range ids {
		l.interrupted[id] = true
	}
	c.cancel(errSteered)
	return false
}

// lockSource cuts the listener's fetch in flight short, then takes
// sourceMu. The caller unlocks sourceMu.
func (l *steerListener) lockSource() {
	l.mu.Lock()
	if l.fetchCancel != nil {
		l.fetchCancel()
	}
	l.mu.Unlock()
	l.sourceMu.Lock()
}

// release ends the listener's hold on c, cuts a fetch in flight short, and
// returns the reason the listener last declined to cut c.
func (l *steerListener) release(c *steerableCall) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.call == c {
		l.call = nil
	}
	if l.fetchCancel != nil {
		l.fetchCancel()
	}
	why := c.declined
	c.declined = ""
	return why
}

// keepUrgent drops from the urgent set the IDs the source no longer
// returns as fresh. The caller holds sourceMu, so no listener fetch lands
// between the run's fetch and this update.
func (l *steerListener) keepUrgent(fresh []Steer) {
	if l == nil {
		return
	}
	waiting := map[string]bool{}
	for _, steer := range fresh {
		waiting[steer.Item.ID] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for id := range l.urgent {
		if !waiting[id] {
			delete(l.urgent, id)
		}
	}
}

// committed refreshes the snapshot with a step's recorded items and drops
// them from the urgent set.
func (l *steerListener) committed(step Step) {
	if l == nil || step.Context == nil || len(step.Context.Items) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, item := range step.Context.Items {
		if item.ID == "" {
			continue
		}
		l.recorded[item.ID] = cloneContextItem(item)
		delete(l.urgent, item.ID)
	}
}

// reminderTokens estimates the reminder a cut-off call adds to the next
// request.
func reminderTokens() int {
	return estimatedItemTokens([]ContextItem{{
		Message: steeringReminder(),
	}})
}

// reasonSteering is the reason recorded on a call an urgent steer kept
// from starting, and steerSkipText is the result the model sees for it.
const (
	reasonSteering = "steering"
	steerSkipText  = "Not run: the user sent new input."
)

// steerSkips reports whether call id must not start because of an urgent
// steer. Once a call of a batch is skipped, no later call of that batch
// starts, so the record alone decides a resumed batch the same way.
// Otherwise the urgent set decides. Earlier calls, such as one awaiting
// approval, are not held back by the record. Only a requested or allowed
// call can be skipped: a started call may have taken effect.
func (r *runtime) steerSkips(s *replayState, id string) bool {
	if status := s.Tools[id].Status; status != ToolRequested &&
		status != ToolAllowed {
		return false
	}
	attempt := s.callAttempts[id]
	for _, other := range s.callOrder {
		if other == id {
			break
		}
		if s.callAttempts[other] == attempt && s.Tools[other].steered {
			return true
		}
	}
	return r.urgentWaiting()
}

// urgentWaiting reports whether the urgent set holds a steer. Like
// registerCall, it takes the source lock, so a steer the listener has
// fetched is decided before the run looks.
func (r *runtime) urgentWaiting() bool {
	l := r.listen
	if l == nil {
		return false
	}
	l.lockSource()
	defer l.sourceMu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.urgent) > 0
}

// skipForSteer settles a requested or allowed call as not executed, so the
// steer reaches the model before the batch goes on.
func (r *runtime) skipForSteer(call tool.Call) *Outcome {
	out := tool.Outcome{
		State:  tool.StateNotExecuted,
		Output: new(tool.TextOutput(steerSkipText)),
	}
	return r.commit(Step{
		Kind: StepToolSettled,
		Tool: &ToolFact{Call: call, Outcome: &out, Reason: reasonSteering},
	})
}
