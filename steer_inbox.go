package dashi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

// MemoryInbox is a ready-made in-memory Inbox. It is safe for concurrent
// use, so one goroutine can send while a run takes commands, and it can be
// reused across turns, with one active run per turn at a time.
//
// It holds two kinds of entry. Put addresses a command to one turn, as a
// host inbox table would. Send queues a steer for whichever turn runs next
// with this inbox, as an interactive app needs.
//
// A sent steer the inbox hands to a run is claimed: Withdraw no longer
// removes it. When the run ends, claims it did not record are released. A
// steer in a write whose result is unknown is in doubt: it goes only to
// runs of the same turn until Leftovers checks it against the reloaded
// turn.
//
// MemoryInbox is an InboxNotifier: Put and an urgent Send signal Ready, so
// a run can cut short the model call in flight and Drive wakes. The zero
// value is ready to use.
type MemoryInbox struct {
	mu      sync.Mutex
	entries []*inboxEntry
	// ready holds one channel per watched turn, created on first use, so
	// MemoryInbox{} works. Its buffer of one coalesces signals: a run that
	// has not yet looked still finds one. It is dropped when its last
	// watcher unwatches.
	ready map[string]*readyChannel
}

// readyChannel is a turn's Ready channel and its number of watchers.
type readyChannel struct {
	ch       chan struct{}
	watchers int
}

type inboxEntry struct {
	command Command
	// to is the turn a Put addressed the command to; empty for a steer
	// from Send, which goes to whichever turn runs.
	to string
	// turn is the turn that claimed the entry; empty means unclaimed.
	turn    string
	inDoubt bool
}

func (e *inboxEntry) id() string {
	return CommandIDOf(e.command)
}

// NewMemoryInbox returns an empty inbox.
func NewMemoryInbox() *MemoryInbox {
	return &MemoryInbox{}
}

// Put queues command for turnID and signals Ready(turnID). The command
// needs a command ID, which the inbox must not already hold; a steer needs
// a valid item, and an empty Origin means ContextUser.
func (in *MemoryInbox) Put(turnID string, command Command) error {
	if turnID == "" {
		return errors.New("dashi: turn ID is required")
	}
	if CommandKindOf(command) == "" {
		return fmt.Errorf("dashi: cannot queue command of type %T", command)
	}
	if steer, ok := command.(Steer); ok {
		var err error
		if command, err = newSteer(steer, false); err != nil {
			return err
		}
	}
	id := CommandIDOf(command)
	if id == "" {
		return errors.New("dashi: command ID is required")
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.find(id) >= 0 {
		return fmt.Errorf("dashi: command ID %q is already queued", id)
	}
	in.entries = append(in.entries, &inboxEntry{command: command, to: turnID})
	in.signalLocked(turnID)
	return nil
}

// Send queues a steer for whichever turn runs next with this inbox and
// returns its ID, generating one when Item.ID is empty. An empty Origin
// means ContextUser. It returns an error for an invalid item or an ID the
// inbox already holds. An urgent steer signals Ready for every turn once it
// is queued.
func (in *MemoryInbox) Send(steer Steer) (string, error) {
	steer, err := newSteer(steer, true)
	if err != nil {
		return "", err
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.find(steer.Item.ID) >= 0 {
		return "", fmt.Errorf("dashi: steer ID %q is already queued", steer.Item.ID)
	}
	in.entries = append(in.entries, &inboxEntry{command: steer})
	if steer.Urgent {
		for turnID := range in.ready {
			in.signalLocked(turnID)
		}
	}
	return steer.Item.ID, nil
}

// newSteer copies and checks a steer, filling an empty Origin, and an
// empty ID when generate is set.
func newSteer(steer Steer, generate bool) (Steer, error) {
	item := cloneContextItem(steer.Item)
	if item.ID == "" && generate {
		item.ID = NewID()
	}
	if item.Origin == "" {
		item.Origin = ContextUser
	}
	if err := item.Validate(); err != nil {
		return Steer{}, fmt.Errorf("dashi: invalid steer: %w", err)
	}
	return Steer{Item: item, Urgent: steer.Urgent}, nil
}

// SendUrgent queues text from the user as an urgent steer, which may cut
// short the model call in flight, and returns the steer's ID.
func (in *MemoryInbox) SendUrgent(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("dashi: steer text is empty")
	}
	return in.Send(Steer{
		Item: ContextItem{
			Origin:  ContextUser,
			Message: llm.UserMessage(llm.Text(text)),
		},
		Urgent: true,
	})
}

// SendText queues text from the user and returns the steer's ID.
func (in *MemoryInbox) SendText(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("dashi: steer text is empty")
	}
	return in.Send(Steer{Item: ContextItem{
		Origin:  ContextUser,
		Message: llm.UserMessage(llm.Text(text)),
	}})
}

// Ready signals, coalesced, that a command for turnID may be waiting. Its
// watchers of one turn share a channel, which unwatch drops after the
// last of them; unwatch may be called more than once.
func (in *MemoryInbox) Ready(turnID string) (<-chan struct{}, func()) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.ready == nil {
		in.ready = map[string]*readyChannel{}
	}
	rc := in.ready[turnID]
	if rc == nil {
		rc = &readyChannel{ch: make(chan struct{}, 1)}
		in.ready[turnID] = rc
	}
	rc.watchers++
	var once sync.Once
	return rc.ch, func() {
		once.Do(func() {
			in.mu.Lock()
			defer in.mu.Unlock()
			rc.watchers--
			if rc.watchers == 0 && in.ready[turnID] == rc {
				delete(in.ready, turnID)
			}
		})
	}
}

// signalLocked wakes turnID's watchers, if any.
func (in *MemoryInbox) signalLocked(turnID string) {
	rc := in.ready[turnID]
	if rc == nil {
		return
	}
	select {
	case rc.ch <- struct{}{}:
	default:
	}
}

// Withdraw removes a queued command that no run has taken. It returns
// false once the command was handed to a run, is in doubt, or is gone.
func (in *MemoryInbox) Withdraw(id string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	i := in.find(id)
	if i < 0 || in.entries[i].turn != "" {
		return false
	}
	in.entries = slices.Delete(in.entries, i, i+1)
	return true
}

// Waiting returns, oldest first, the commands put for turnID, the sent
// steers no other turn has claimed, and those already claimed by turnID,
// and claims them for turnID.
func (in *MemoryInbox) Waiting(_ context.Context, turnID string) ([]Command, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []Command
	for _, e := range in.entries {
		if e.to != "" && e.to != turnID ||
			e.turn != "" && e.turn != turnID {
			continue
		}
		e.turn = turnID
		out = append(out, cloneInboxCommand(e.command))
	}
	return out, nil
}

// cloneInboxCommand copies a steer's item so a caller cannot change the
// queued one; other commands are values the inbox does not share further.
func cloneInboxCommand(c Command) Command {
	if steer, ok := c.(Steer); ok {
		return Steer{Item: cloneContextItem(steer.Item), Urgent: steer.Urgent}
	}
	return c
}

// Delivered removes commands the turn recorded or refused. A recorded steer
// belongs to the turn's history from then on, which reaches the next turn
// through TurnMessagesFull.
func (in *MemoryInbox) Delivered(_ context.Context, _ string, ids []string) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, id := range ids {
		if i := in.find(id); i >= 0 {
			in.entries = slices.Delete(in.entries, i, i+1)
		}
	}
	return nil
}

// Release returns the run's unrecorded claims to the queue and marks the
// in-doubt ones, which stay claimed by turnID. A command put for turnID
// stays its own.
func (in *MemoryInbox) Release(_ context.Context, turnID string, unrecorded, inDoubt []string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, id := range unrecorded {
		if i := in.find(id); i >= 0 && in.entries[i].turn == turnID &&
			!in.entries[i].inDoubt {
			in.entries[i].turn = ""
		}
	}
	for _, id := range inDoubt {
		if i := in.find(id); i >= 0 && in.entries[i].turn == turnID {
			in.entries[i].inDoubt = true
		}
	}
}

// Leftovers returns, oldest first, the steers not recorded in turn, and
// removes them from the inbox, so the app can start its next turn with
// them: those still queued, those put for the turn, those the turn claimed
// but did not record, and in-doubt steers the reloaded turn shows were
// never written. Recorded steers are never returned: they reach the next
// turn through TurnMessagesFull, with the rest of the turn's history,
// whether or not a model call saw them. Steers claimed by or put for
// another turn stay queued, and so do commands other than steers.
//
// A queued steer counts as recorded only when the turn records the same
// steer, with the same content, as a steer; an unrelated item that happens
// to share its ID does not hide it. Pass a freshly loaded turn: in a stale
// copy, recorded steers look unrecorded and would be sent twice. An invalid
// turn returns an error and changes nothing.
func (in *MemoryInbox) Leftovers(turn Turn) ([]Steer, error) {
	if _, err := turn.View(); err != nil {
		return nil, err
	}
	recorded := map[string]ContextItem{}
	for _, step := range turn.Steps {
		if step.Context == nil || !step.Context.Steering {
			continue
		}
		for _, item := range step.Context.Items {
			recorded[item.ID] = item
		}
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []Steer
	kept := in.entries[:0]
	for _, e := range in.entries {
		steer, ok := e.command.(Steer)
		if !ok || e.to != "" && e.to != turn.ID ||
			e.turn != "" && e.turn != turn.ID {
			kept = append(kept, e)
			continue
		}
		item := steer.Item
		if old, ok := recorded[item.ID]; ok && canonjson.SameValue(old, item) {
			continue
		}
		out = append(out, Steer{
			Item:   cloneContextItem(item),
			Urgent: steer.Urgent,
		})
	}
	clear(in.entries[len(kept):])
	in.entries = kept
	return out, nil
}

// Has reports whether the inbox holds a command with id: queued, claimed,
// or in doubt. Send and Put refuse such an ID, so a host that keeps a queue
// of its own beside the inbox checks it before queueing an item there.
func (in *MemoryInbox) Has(id string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.find(id) >= 0
}

func (in *MemoryInbox) find(id string) int {
	return slices.IndexFunc(in.entries, func(e *inboxEntry) bool {
		return e.id() == id
	})
}

var (
	_ Inbox         = (*MemoryInbox)(nil)
	_ InboxReleaser = (*MemoryInbox)(nil)
	_ InboxNotifier = (*MemoryInbox)(nil)
)
