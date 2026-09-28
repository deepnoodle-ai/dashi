package dashi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

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
type Inbox struct {
	mu      sync.Mutex
	entries []*inboxEntry
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
// invalid item or an ID the inbox already holds.
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
	in.entries = append(in.entries, &inboxEntry{steer: Steer{Item: item}})
	return item.ID, nil
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
		out = append(out, Steer{Item: cloneContextItem(e.steer.Item)})
	}
	return out, nil
}

// Delivered removes steers the turn recorded. The record keeps them, marked
// as steers, so Leftovers can still return any no model call saw.
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

// Leftovers returns the steers no model call of turn saw, oldest first, so
// the app can start its next turn with them. They are the steers the turn
// records with no later model_requested step, read from the record, followed
// by those still queued that the turn did not record, which the inbox
// forgets. Steers claimed by another turn stay queued.
//
// Call it once per turn, when the turn will not run again or to abandon it:
// the recorded steers come from the record on every call, so not sending
// them twice is the caller's job. Pass a freshly loaded turn: in a stale
// copy, recorded steers look unrecorded and would be delivered twice. An
// invalid turn returns an error and changes nothing.
func (in *Inbox) Leftovers(turn Turn) ([]Steer, error) {
	if _, err := Replay(turn); err != nil {
		return nil, err
	}
	// recorded holds every recorded item ID; unseen lists the steers no
	// later model call saw, in order.
	recorded := map[string]bool{}
	var unseen []ContextItem
	for _, step := range turn.Steps {
		if step.Kind == StepModelIntent {
			unseen = unseen[:0]
			continue
		}
		for _, item := range step.Items() {
			if item.ID == "" {
				continue
			}
			recorded[item.ID] = true
			if step.Context.Steering {
				unseen = append(unseen, item)
			}
		}
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []Steer
	for _, item := range unseen {
		out = append(out, Steer{Item: cloneContextItem(item)})
	}
	kept := in.entries[:0]
	for _, e := range in.entries {
		if e.turn != "" && e.turn != turn.ID {
			kept = append(kept, e)
			continue
		}
		if !recorded[e.steer.Item.ID] {
			out = append(out, Steer{Item: cloneContextItem(e.steer.Item)})
		}
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
)
