package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Queued reports where Steer, SteerItem, Enqueue, or EnqueueItem put an
// item.
type Queued struct {
	// ID is the item's ID, generated when the item had none.
	ID string
	// TurnID is the running turn a steer reached; empty when the item went
	// to the queue for the next turn.
	TurnID string
	// Warnings holds WarningMemoryOnly when the item was queued under a
	// durable recorder: the queue lives only in this handle.
	Warnings []dashi.Warning
}

// queuedItem is one item of the handle's queue. A reserved item keeps its
// place until the session knows whether the turn that carries it started.
type queuedItem struct {
	item     dashi.ContextItem
	reserved *reservation
}

// reservation marks queued items a Start carries: the Start's command ID
// and turn number. It is open while that Start runs; once it has ended
// with an unknown result, the next command finishes it by loading the turn.
type reservation struct {
	commandID string
	n         int
	open      bool
}

// errStreamStopped marks a turn whose Stream consumer broke the range, so
// the session cannot tell from the result whether it recorded anything.
var errStreamStopped = errors.New("session: stream consumer stopped")

// Steer sends text from the user to the turn this handle is running, which
// sees it at its next model call. When the handle runs no turn, the text
// goes to the queue for the next turn instead, and Queued.TurnID is empty.
// A steer that misses its turn is not lost: when the turn ends, or a new
// Start closes it, the steer moves to the queue.
//
// Steer sends to the handle's own inbox, or to Session.Steering when it is
// a *dashi.Inbox; with another source it returns an error, and the host sends
// through its own system.
func (s *Session) Steer(ctx context.Context, text string) (Queued, error) {
	if strings.TrimSpace(text) == "" {
		return Queued{}, errors.New("session: steer text is empty")
	}
	return s.SteerItem(ctx, dashi.Steer{Item: userItem(text)})
}

// SteerItem is Steer for any steer: an urgent one, which may cut short the
// model call in flight, or an item of another origin, such as a background
// task's notice. An empty Item.ID is generated and an empty origin means
// dashi.ContextUser. An ID the queue or inbox already holds is an error.
func (s *Session) SteerItem(ctx context.Context, steer dashi.Steer) (Queued, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.steering().(*dashi.Inbox)
	if !ok {
		return Queued{}, errors.New(
			"session: Session.Steering is not an *Inbox; send steers through it",
		)
	}
	if s.compacting != "" {
		return Queued{}, &TurnBusyError{
			TurnID: s.compacting,
			Next:   dashi.NextRunning,
			Origin: dashi.StartCompaction,
		}
	}
	item, err := s.newItem(steer.Item)
	if err != nil {
		return Queued{}, err
	}
	if s.driving == "" {
		return s.enqueue(item), nil
	}
	steer.Item = item
	if _, err := in.Send(steer); err != nil {
		return Queued{}, err
	}
	return Queued{ID: item.ID, TurnID: s.driving}, nil
}

// Enqueue holds text from the user for the next turn. The queue runs when
// the session goes idle: after a turn this handle drives ends with NextNone,
// in the same Run or Stream call, or with RunQueue. A caller's Start on an
// idle session takes queued user items first, ahead of its own input.
//
// The queue lives in this handle only. Under a durable recorder, Queued
// carries a WarningMemoryOnly: a server that builds a handle per request
// loses what it queued when the request ends.
func (s *Session) Enqueue(ctx context.Context, text string) (Queued, error) {
	if strings.TrimSpace(text) == "" {
		return Queued{}, errors.New("session: queued text is empty")
	}
	return s.EnqueueItem(ctx, userItem(text))
}

// EnqueueItem is Enqueue for any context item, such as a background task's
// notice. An empty ID is generated and an empty origin means dashi.ContextUser;
// an ID the queue or inbox already holds is an error. Items that did not
// come from the user do not join a caller's Start: they run as a queue turn
// of their own, whose command ID records their IDs.
func (s *Session) EnqueueItem(
	ctx context.Context,
	item dashi.ContextItem,
) (Queued, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.newItem(item)
	if err != nil {
		return Queued{}, err
	}
	return s.enqueue(item), nil
}

// History returns the conversation history: every message of every turn,
// in order, including tool calls and results. Compaction turns are not
// conversation, so it never includes them; Turns does. A turn that cannot
// be read is left out; Turns reports its error.
func (s *Session) History(ctx context.Context) ([]llm.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ID == "" {
		return nil, nil
	}
	last, err := s.findLast(ctx)
	if err != nil {
		return nil, err
	}
	return s.conversation(ctx, last), nil
}

// NextHistory returns what the next new turn sends as Start.History,
// unless a compaction turn runs first: the replacement history of the
// newest completed compaction turn, then the messages of the turns after
// it. Without compaction turns it equals History. A turn that cannot be
// read is left out.
func (s *Session) NextHistory(ctx context.Context) ([]llm.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ID == "" {
		return nil, nil
	}
	last, err := s.findLast(ctx)
	if err != nil {
		return nil, err
	}
	history, _ := s.history(ctx, last)
	return history, nil
}

// Turns yields every turn record of the session, in order, from a copy.
// A turn that cannot be read yields a Turn holding only its ID, with the
// error, and the iteration goes on. A turn the session closed has a
// canceled stop with the reason CloseReason.
func (s *Session) Turns(ctx context.Context) iter.Seq2[dashi.Turn, error] {
	return func(yield func(dashi.Turn, error) bool) {
		s.mu.Lock()
		last, err := 0, error(nil)
		if s.ID != "" {
			last, err = s.findLast(ctx)
		}
		s.mu.Unlock()
		if err != nil {
			yield(dashi.Turn{}, err)
			return
		}
		for n := 1; n <= last; n++ {
			s.mu.Lock()
			t, err := s.load(ctx, n)
			turn := dashi.Turn{ID: TurnID(s.ID, n)}
			if err == nil {
				turn = t.turn.Clone()
			}
			s.mu.Unlock()
			if !yield(turn, err) {
				return
			}
		}
	}
}

// steering returns the source every turn of this handle gets. The caller
// holds mu, or the source was fixed by an earlier call.
func (s *Session) steering() dashi.Steering {
	if s.Steering != nil {
		return s.Steering
	}
	if s.inbox == nil {
		s.inbox = dashi.NewInbox()
	}
	return s.inbox
}

func userItem(text string) dashi.ContextItem {
	return dashi.ContextItem{
		Origin:  dashi.ContextUser,
		Message: llm.UserMessage(llm.Text(text)),
	}
}

// newItem fills and checks an item for the queue or inbox. The caller holds
// mu.
func (s *Session) newItem(item dashi.ContextItem) (dashi.ContextItem, error) {
	item.Message = item.Message.Clone()
	if item.ID == "" {
		item.ID = dashi.NewID()
	}
	if item.Origin == "" {
		item.Origin = dashi.ContextUser
	}
	if err := item.Validate(); err != nil {
		return dashi.ContextItem{}, fmt.Errorf("session: invalid item: %w", err)
	}
	if item.Message.Role != llm.RoleUser {
		return dashi.ContextItem{}, errors.New(
			"session: invalid item: queued input must use the user role",
		)
	}
	taken := slices.ContainsFunc(s.queue, func(q *queuedItem) bool {
		return q.item.ID == item.ID
	})
	if in, ok := s.steering().(*dashi.Inbox); ok && !taken {
		taken = in.Has(item.ID)
	}
	if taken {
		return dashi.ContextItem{}, fmt.Errorf("session: item ID %q is already queued", item.ID)
	}
	return item, nil
}

// enqueue appends an item. The caller holds mu.
func (s *Session) enqueue(item dashi.ContextItem) Queued {
	s.queue = append(s.queue, &queuedItem{item: item})
	q := Queued{ID: item.ID}
	if r := s.Runner.Recorder; r != nil {
		if _, ok := r.(*dashi.MemoryRecorder); !ok {
			q.Warnings = []dashi.Warning{{
				Code: WarningMemoryOnly,
				Message: "the queued item lives only in this session handle " +
					"and is lost when the handle is",
			}}
		}
	}
	return q
}

// moveLeftovers appends to the queue, in order, the steers the inbox holds
// that turn never recorded, once the turn has ended or was closed. A host
// source releases its own. The caller holds mu.
func (s *Session) moveLeftovers(turn dashi.Turn) {
	in, ok := s.steering().(*dashi.Inbox)
	if !ok {
		return
	}
	left, err := in.Leftovers(turn)
	if err != nil {
		return
	}
	for _, steer := range left {
		s.queue = append(s.queue, &queuedItem{item: steer.Item})
	}
}

func fromUser(item dashi.ContextItem) bool {
	return item.Origin == dashi.ContextUser
}

// free returns the queued items no Start holds, in order, that match keep
// (all of them when keep is nil). The caller holds mu.
func (s *Session) free(keep func(dashi.ContextItem) bool) []*queuedItem {
	var out []*queuedItem
	for _, q := range s.queue {
		if q.reserved == nil && (keep == nil || keep(q.item)) {
			out = append(out, q)
		}
	}
	return out
}

// reserve marks items as carried by the Start with commandID for turn n.
// The caller holds mu.
func (s *Session) reserve(items []*queuedItem, commandID string, n int) *reservation {
	r := &reservation{commandID: commandID, n: n, open: true}
	for _, q := range items {
		q.reserved = r
	}
	return r
}

func itemMessages(items []*queuedItem) []llm.Message {
	out := make([]llm.Message, len(items))
	for i, q := range items {
		out[i] = q.item.Message.Clone()
	}
	return out
}

// decideReserved decides the fate of reserved items from the result of the Start
// that carried them. Success consumes them, and so does a turn_started
// event the run already reported (started): the engine emits a step only
// once it committed. Proof that the Start's turn_started step was not
// recorded restores them in order. Anything else is inconclusive, and the
// session loads the turn: a turn_started step with the Start's command ID
// consumes them, a missing one restores them, and a failed load keeps them
// reserved and adds its error. The caller holds mu.
func (s *Session) decideReserved(
	ctx context.Context,
	r *reservation,
	started bool,
	err error,
) error {
	r.open = false
	switch {
	case err == nil || started:
		s.release(r, true)
		return err
	case notStarted(err):
		s.release(r, false)
		return err
	}
	if lerr := s.finishByLoad(ctx, r); lerr != nil {
		return errors.Join(err, lerr)
	}
	return err
}

// errReserved marks a reservation the session cannot finish: it cannot
// tell whether a turn took the queued items. New commands still run, but
// the queue waits, so its items never run out of order or twice.
var errReserved = errors.New("session: queued items stay reserved")

// finishReserved finishes the reservations of Starts that ended with an
// unknown result: it consumes or restores their items. It returns an error
// matching errReserved when one cannot be finished; only the queue waits on
// it. The caller holds mu.
func (s *Session) finishReserved(ctx context.Context) error {
	var pending []*reservation
	for _, q := range s.queue {
		if r := q.reserved; r != nil && !r.open && !slices.Contains(pending, r) {
			pending = append(pending, r)
		}
	}
	for _, r := range pending {
		if err := s.finishByLoad(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) finishByLoad(ctx context.Context, r *reservation) error {
	id := TurnID(s.ID, r.n)
	loader, ok := s.recorder().(dashi.Loader)
	if !ok {
		return fmt.Errorf(
			"%w: turn %s cannot be loaded to learn whether it started",
			errReserved,
			id,
		)
	}
	turn, err := loader.Load(ctx, id)
	switch {
	case err == nil && len(turn.Steps) > 0 &&
		turn.Steps[0].Kind == dashi.StepTurnStarted &&
		turn.Steps[0].CommandID == r.commandID:
		s.release(r, true)
	case err == nil || errors.Is(err, dashi.ErrTurnNotFound):
		s.release(r, false)
	default:
		return fmt.Errorf(
			"%w until turn %s can be read: %w",
			errReserved,
			id,
			err,
		)
	}
	return nil
}

// queueBlocked returns a WarningQueueBlocked while a reservation cannot be
// finished. The caller holds mu.
func (s *Session) queueBlocked() []dashi.Warning {
	n := 0
	var id string
	for _, q := range s.queue {
		if r := q.reserved; r != nil && !r.open {
			n++
			id = TurnID(s.ID, r.n)
		}
	}
	if n == 0 {
		return nil
	}
	return []dashi.Warning{{
		Code: WarningQueueBlocked,
		Message: fmt.Sprintf(
			"%d queued items wait, and the queue with them, until the session "+
				"can read turn %s to learn whether it took them",
			n,
			id,
		),
	}}
}

// release consumes a reservation's items or puts them back in place.
func (s *Session) release(r *reservation, consume bool) {
	kept := s.queue[:0]
	for _, q := range s.queue {
		if q.reserved != r {
			kept = append(kept, q)
			continue
		}
		if !consume {
			q.reserved = nil
			kept = append(kept, q)
		}
	}
	clear(s.queue[len(kept):])
	s.queue = kept
}

// notStarted reports whether err proves that a Start's turn_started step
// was not recorded: Run recorded nothing, or that step's commit was
// rejected or conflicted.
func notStarted(err error) bool {
	if errors.Is(err, errStreamStopped) {
		return false
	}
	if !dashi.Recorded(err) {
		return true
	}
	var ce *dashi.CommitError
	return errors.As(err, &ce) && ce.Step != nil &&
		ce.Step.Kind == dashi.StepTurnStarted &&
		(ce.NothingWritten ||
			errors.Is(ce.Err, dashi.ErrStaleTurn) ||
			errors.Is(ce.Err, dashi.ErrCommandIDReused))
}

// planQueue chooses a queue turn for every free item, when the session is
// idle. It fails with errReserved while a reservation cannot be finished,
// since items that turn may have taken would otherwise run out of order.
// When a turn in the idempotency window already started with the same
// queue: key, the items were already added to it: it consumes them and starts
// nothing. An unreadable last turn is an error, even under Exclusive,
// since it may hold that key. The caller holds mu.
func (s *Session) planQueue(
	ctx context.Context,
	compacted bool,
) (startPlan, bool, error) {
	s.ready()
	if err := s.finishReserved(ctx); err != nil {
		return startPlan{}, false, err
	}
	items := s.free(nil)
	if len(items) == 0 {
		return startPlan{}, false, nil
	}
	if s.ID == "" {
		s.ID = dashi.NewID()
		s.created = true
	}
	last, err := s.findLast(ctx)
	if err != nil {
		return startPlan{}, false, err
	}
	if err := s.checkExists(last); err != nil {
		return startPlan{}, false, err
	}
	if s.compacting != "" {
		// The queue waits for the compaction, as for a running turn.
		return startPlan{}, false, nil
	}
	var p startPlan
	if last > 0 {
		// An unreadable last turn fails even under Exclusive: it may have
		// started with this queue key, and starting another queue turn
		// could add the items twice. A caller's Start moves past it.
		t, err := s.load(ctx, last)
		if err != nil {
			if s.Exclusive && ctx.Err() == nil {
				err = fmt.Errorf(
					"session: queued input waits: turn %s may hold its queue "+
						"key and cannot be read; a new Start moves past it: %w",
					TurnID(s.ID, last),
					err,
				)
			}
			return startPlan{}, false, err
		}
		if s.refine(ctx, t).next != dashi.NextNone {
			return startPlan{}, false, nil
		}
		if items = s.dropDelivered(ctx, last); len(items) == 0 {
			return startPlan{}, false, nil
		}
	}
	s.created = true
	ids := make([]string, len(items))
	origin := dashi.StartScheduled
	for i, q := range items {
		ids[i] = q.item.ID
		if q.item.Origin == dashi.ContextUser {
			origin = dashi.StartUser
		}
	}
	slices.Sort(ids)
	key, _ := json.Marshal(ids)
	c := dashi.Start{
		CommandID: "queue:" + string(key),
		Origin:    origin,
		Input:     itemMessages(items),
	}
	used, err := s.usedKey(ctx, c.CommandID, last)
	if err != nil {
		return startPlan{}, false, err
	}
	if used {
		s.release(s.reserve(items, c.CommandID, 0), true)
		return startPlan{}, false, nil
	}
	history, unreadable := s.history(ctx, last)
	if !compacted {
		if cp := s.planCompaction(ctx, last, history, nil); cp != nil {
			return *cp, true, nil
		}
	}
	p.warnings = append(p.warnings, unreadable...)
	c.History = history
	c.Agent = s.agentFor(ctx, dashi.Agent{}, last)
	p.reserved = s.reserve(items, c.CommandID, last+1)
	p.n, p.id, p.command, p.runner = last+1, TurnID(s.ID, last+1), c, s.runner()
	s.running[p.id]++
	s.driving = p.id
	return p, true, nil
}

// dropDelivered removes the free queued items that Session.Delivered names
// for turns 1 through last, and returns the free items left. A turn that
// cannot be read is left out. The caller holds mu.
func (s *Session) dropDelivered(ctx context.Context, last int) []*queuedItem {
	if s.Delivered == nil {
		return s.free(nil)
	}
	turns := make([]dashi.Turn, 0, last)
	for n := 1; n <= last; n++ {
		if t, err := s.load(ctx, n); err == nil {
			turns = append(turns, t.turn)
		}
	}
	if done := s.Delivered(turns); len(done) > 0 {
		s.queue = slices.DeleteFunc(s.queue, func(q *queuedItem) bool {
			return q.reserved == nil && done[q.item.ID]
		})
	}
	return s.free(nil)
}

// usedKey reports whether a turn in the idempotency window, from turn n
// back through the newest turn a caller's Start began, started with key.
// A turn that fails to load fails the check, since a missed key would
// add the items twice. The caller holds mu.
func (s *Session) usedKey(ctx context.Context, key string, n int) (bool, error) {
	for ; n >= 1; n-- {
		t, err := s.load(ctx, n)
		if err != nil {
			return false, err
		}
		id := t.turn.Steps[0].CommandID
		if id == key {
			return true, nil
		}
		if !internalKey(id) {
			return false, nil
		}
	}
	return false, nil
}
