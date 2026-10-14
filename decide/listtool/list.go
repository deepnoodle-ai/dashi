package listtool

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// List is a host list that both tools can read, such as the open support
// tickets or the user's contacts. Define it once and pass it to JudgeItems
// and PickItem through Config.Lists.
type List[T any] struct {
	// Name is the list's name, such as "tickets". The model passes it as
	// the list argument. It is 1 to 64 letters, digits, underscores, or
	// hyphens.
	Name string
	// Description says what the items are and what a query means, such as
	// `Open support tickets. The query is a search such as "label:billing".
	// An empty query means all open tickets.` The model reads it in each
	// tool's description.
	Description string
	// Items returns the items for a query: judge_items passes the model's
	// query, and pick_item passes its looking_for text, so Items can
	// filter by it. ctx carries the host's values, and dashi.CurrentTurn
	// reads the turn from it. An error fails the call. pick_item shows the
	// model its text; judge_items shows a fixed message and keeps the text
	// in Result.Err.
	Items func(ctx context.Context, query string) ([]T, error)
	// ID returns the item's ID in the host, such as "T-1042". The model
	// sees it, and the tools return it. Every item needs one, and the items
	// of one call need different IDs; otherwise the call fails before it
	// sends a request. Use a key the host assigns, never outside text such
	// as an email subject: judge_items is closed-world, so a prompt
	// injection check skips its results.
	ID func(T) string
	// Text returns what the decision model reads about the item. The model
	// never sees it in a judge_items result; pick_item shows the first
	// part of it beside each candidate.
	Text func(T) string
}

// AnyList holds a List of any item type, so one Config can hold lists of
// different types. Only List implements it.
type AnyList interface {
	name() string
	check() error
	describe() string
	read(ctx context.Context, query string) ([]item, error)
}

// item is one entry of a list, as the tools use it.
type item struct {
	id, text string
}

var listName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (l List[T]) name() string { return l.Name }

func (l List[T]) describe() string { return l.Description }

// check says what makes the list unusable, so a constructor fails at
// startup instead of on the first call.
func (l List[T]) check() error {
	switch {
	case l.Name == "":
		return errors.New("a list has no Name")
	case !listName.MatchString(l.Name):
		return fmt.Errorf("list name %q must be 1 to 64 letters, digits, underscores, or hyphens", l.Name)
	case l.Items == nil:
		return fmt.Errorf("list %q has no Items function", l.Name)
	case l.ID == nil:
		return fmt.Errorf("list %q has no ID function", l.Name)
	case l.Text == nil:
		return fmt.Errorf("list %q has no Text function", l.Name)
	}
	return nil
}

// read returns the items for query with their IDs and text. An item with
// no ID, or with the ID of an earlier item, is an error, because the tools
// could not tell the model which item an ID names.
func (l List[T]) read(ctx context.Context, query string) ([]item, error) {
	values, err := l.Items(ctx, query)
	if err != nil {
		return nil, err
	}
	items := make([]item, len(values))
	seen := make(map[string]int, len(values))
	for i, v := range values {
		items[i] = item{id: l.ID(v), text: l.Text(v)}
		if items[i].id == "" {
			return nil, fmt.Errorf("item %d has no ID", i+1)
		}
		if first, ok := seen[items[i].id]; ok {
			return nil, fmt.Errorf("items %d and %d have the same ID %q", first, i+1, items[i].id)
		}
		seen[items[i].id] = i + 1
	}
	return items, nil
}
