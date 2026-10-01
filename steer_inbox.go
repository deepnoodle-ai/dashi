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

// Inbox is a ready-made in-memory Steering source. It is safe for
// concurrent use, so one goroutine can send while a run takes steers, and it
// can be reused across turns, with one active run at a time.
//
// A steer the inbox hands to a run is claimed: Cancel no longer removes it.
// When the run ends, claims it did not record are released. A steer in a
// write whose outcome is unknown is in doubt: it goes only to runs of the
// same turn until Leftovers reconciles it against the reloaded turn.
//
// Inbox is a SteeringNotifier: an urgent steer signals Ready, so the run
// can cut short the model call in flight. The zero value is ready to use.
type Inbox struct {
	mu      sync.Mutex
	entries []*inboxEntry
	// ready is created on first use, so Inbox{} works. Its buffer of one
	// coalesces signals: a run that has not yet looked still finds one.
	ready chan struct{}
}

type inboxEntry struct {
	steer Steer
	// turn is the turn that claimed the steer; empty means unclaimed.
	turn    string
	inDoubt bool
}

// NewInbox returns an empty inbox.
func NewInbox() *Inbox {
	return &Inbox{}
}

// Send queues a steer and returns its ID, generating one when Item.ID is
// empty. An empty Origin means ContextUser. It returns an error for an
// invalid item or an ID the inbox already holds. An urgent steer signals
// Ready once it is queued.
func (in *Inbox) Send(steer Steer) (string, error) {
	item := cloneContextItem(steer.Item)
	if item.ID == "" {
		item.ID = NewID()
	}
	if item.Origin == "" {
		item.Origin = ContextUser
	}
	if err := validateContext(item); err != nil {
		return "", fmt.Errorf("dashi: invalid steer: %w", err)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.find(item.ID) >= 0 {
		return "", fmt.Errorf("dashi: steer ID %q is already queued", item.ID)
	}
	in.entries = append(
		in.entries,
		&inboxEntry{steer: Steer{Item: item, Urgent: steer.Urgent}},
	)
	if steer.Urgent {
		select {
		case in.readyLocked() <- struct{}{}:
		default:
		}
	}
	return item.ID, nil
}

// SendUrgent queues text from the user as an urgent steer, which may cut
// short the model call in flight, and returns the steer's ID.
func (in *Inbox) SendUrgent(text string) (string, error) {
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

// Ready signals, coalesced, that an urgent steer may be waiting. It returns
// the same channel on every call. Only one run should watch it at a time,
// as for the inbox itself.
func (in *Inbox) Ready() <-chan struct{} {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.readyLocked()
}

func (in *Inbox) readyLocked() chan struct{} {
	if in.ready == nil {
		in.ready = make(chan struct{}, 1)
	}
	return in.ready
}

// SendText queues text from the user and returns the steer's ID.
func (in *Inbox) SendText(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("dashi: steer text is empty")
	}
	return in.Send(Steer{Item: ContextItem{
		Origin:  ContextUser,
		Message: llm.UserMessage(llm.Text(text)),
	}})
}

// Cancel removes a steer that no run has claimed. It returns false once the
// steer is claimed, in doubt, or gone.
func (in *Inbox) Cancel(id string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	i := in.find(id)
	if i < 0 || in.entries[i].turn != "" {
		return false
	}
	in.entries = slices.Delete(in.entries, i, i+1)
	return true
}

// Steers returns, oldest first, the unclaimed steers and those already
// claimed by turnID, and claims them for turnID. Steers claimed by another
// turn, including those in doubt there, are not returned.
func (in *Inbox) Steers(_ context.Context, turnID string) ([]Steer, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []Steer
	for _, e := range in.entries {
		if e.turn != "" && e.turn != turnID {
			continue
		}
		e.turn = turnID
		out = append(out, Steer{
			Item:   cloneContextItem(e.steer.Item),
			Urgent: e.steer.Urgent,
		})
	}
	return out, nil
}

// Delivered removes steers the turn recorded. From then on they belong to
// the turn's history, which reaches the next turn through Project.
func (in *Inbox) Delivered(_ context.Context, _ string, ids []string) error {
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
// in-doubt ones, which stay claimed by turnID.
func (in *Inbox) Release(_ context.Context, turnID string, unrecorded, inDoubt []string) {
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
// them: those still queued, those the turn claimed but did not record, and
// in-doubt steers the reloaded turn shows were never written. Recorded
// steers are never returned: they reach the next turn through Project, with
// the rest of the turn's history, whether or not a model call saw them.
// Steers claimed by another turn stay queued.
//
// A queued steer counts as recorded only when the turn records the same
// steer, with the same content, as a steer; an unrelated item that happens
// to share its ID does not hide it. Pass a freshly loaded turn: in a stale
// copy, recorded steers look unrecorded and would be sent twice. An invalid
// turn returns an error and changes nothing.
func (in *Inbox) Leftovers(turn Turn) ([]Steer, error) {
	if _, err := Replay(turn); err != nil {
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
		if e.turn != "" && e.turn != turn.ID {
			kept = append(kept, e)
			continue
		}
		item := e.steer.Item
		if old, ok := recorded[item.ID]; ok && canonjson.SameValue(old, item) {
			continue
		}
		out = append(out, Steer{
			Item:   cloneContextItem(item),
			Urgent: e.steer.Urgent,
		})
	}
	clear(in.entries[len(kept):])
	in.entries = kept
	return out, nil
}

func (in *Inbox) find(id string) int {
	return slices.IndexFunc(in.entries, func(e *inboxEntry) bool {
		return e.steer.Item.ID == id
	})
}

var (
	_ Steering         = (*Inbox)(nil)
	_ SteeringReleaser = (*Inbox)(nil)
	_ SteeringNotifier = (*Inbox)(nil)
)
