package dashi

import (
	"context"
	"errors"
	"sync"
)

// errSteered is the private cause a model call's context ends with when an
// urgent steer cuts it short. The settlement tells it apart from the run's
// own context ending, which stops the run.
var errSteered = errors.New("dashi: an urgent steer interrupted the model call")

// steeringInterruptedText is the reminder that follows a cut-off call's
// partial response in the next request, so the model reads the partial as
// unfinished rather than as its answer.
const steeringInterruptedText = "The user interrupted your previous response."

// steerListener is one run's watcher of a SteeringNotifier. Its goroutine
// is the only reader of Ready. mu serializes every call the run and the
// listener make to the source, and guards the state they share; the
// listener writes no run state and no warnings.
type steerListener struct {
	mu sync.Mutex
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
	seen     []string
	seenSet  map[string]bool
	call     *steerableCall
	stop     context.CancelFunc
	finished chan struct{}
}

// steerableCall is a model call the listener may cut short. It is
// registered after request preparation and before BeforeSend.
type steerableCall struct {
	cancel context.CancelCauseFunc
	// never is the reason the call can never be cut, such as a hosted tool
	// unsafe to repeat; empty when it can.
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

// lockSteering serializes a call to the source with the listener's.
func (r *runtime) lockSteering() func() {
	if r.listen == nil {
		return func() {}
	}
	r.listen.mu.Lock()
	return r.listen.mu.Unlock
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
		l.check(ctx, source, turnID)
	}
}

// check handles one signal: it fetches the waiting steers and, for a fresh
// urgent one, cuts the registered call short when that is allowed.
func (l *steerListener) check(
	ctx context.Context,
	source Steering,
	turnID string,
) {
	l.mu.Lock()
	defer l.mu.Unlock()
	steers, err := source.Steers(ctx, turnID)
	if err != nil {
		// FR-6 reports a failing source; the listener writes no warnings.
		return
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
		return
	}
	why := c.never
	if why == "" && c.handedOff {
		why = c.fits(estimatedItemTokens(items) + reminderTokens())
	}
	if why != "" {
		c.declined = why
		return
	}
	c.cut = true
	for _, id := range ids {
		l.interrupted[id] = true
	}
	c.cancel(errSteered)
}

// keepUrgent drops from the urgent set the IDs the source no longer
// returns as fresh. The caller holds mu.
func (l *steerListener) keepUrgent(fresh []Steer) {
	if l == nil {
		return
	}
	waiting := map[string]bool{}
	for _, steer := range fresh {
		waiting[steer.Item.ID] = true
	}
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
	r.listen.mu.Lock()
	defer r.listen.mu.Unlock()
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
	r.listen.mu.Lock()
	defer r.listen.mu.Unlock()
	return c.cut
}

// unregisterCall ends the listener's hold on c. The first reason the
// listener gave for leaving a call running becomes a warning on the next
// step, which is the call's settlement or the stop that ends the run.
func (r *runtime) unregisterCall(c *steerableCall) {
	if c == nil {
		return
	}
	r.listen.mu.Lock()
	if r.listen.call == c {
		r.listen.call = nil
	}
	why := c.declined
	c.declined = ""
	r.listen.mu.Unlock()
	if why != "" && !r.steers.declined {
		r.steers.declined = true
		r.warnSteering("an urgent steer did not interrupt the model call " +
			"because " + why + "; it waits for the next model call")
	}
}

// reminderTokens estimates the reminder a cut-off call adds to the next
// request.
func reminderTokens() int {
	return estimatedItemTokens([]ContextItem{{
		Message: steeringReminder(),
	}})
}

// dropCall ends the listener's hold on c without a warning, for exits that
// never reach the call's settlement.
func (r *runtime) dropCall(c *steerableCall) {
	if c == nil {
		return
	}
	r.listen.mu.Lock()
	defer r.listen.mu.Unlock()
	if r.listen.call == c {
		r.listen.call = nil
	}
}
