package dashi

import "errors"

// persistence returns the persistence of a *CommitError returned by Run,
// empty for nil, and "other" for any other error.
func persistence(err error) Persistence {
	if err == nil {
		return ""
	}
	var ce *CommitError
	if errors.As(err, &ce) {
		return ce.Persistence
	}
	return "other"
}

// unacknowledged returns the step a *CommitError names, or nil.
func unacknowledged(err error) *Step {
	var ce *CommitError
	if errors.As(err, &ce) {
		return ce.Step
	}
	return nil
}

// pendingIDs lists the IDs of pending items in order.
func pendingIDs(items []Pending) []string {
	var ids []string
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}
