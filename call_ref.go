package dashi

import (
	"context"
	"errors"
	"unicode/utf8"
)

// CallRef names one tool call in one turn. A child turn records the call that
// caused it as its parent, so a host can walk from a child record back to
// the work that asked for it.
type CallRef struct {
	TurnID string `json:"turn_id"`
	CallID string `json:"call_id"`
}

// Validate requires both IDs to be nonempty valid UTF-8. It does not check
// that the call exists: the record of the parent turn is a separate record,
// and the host owns any cross-record check.
func (c CallRef) Validate() error {
	for _, id := range []string{c.TurnID, c.CallID} {
		if id == "" || !utf8.ValidString(id) {
			return errors.New(
				"dashi: parent requires a nonempty turn ID and call ID",
			)
		}
	}
	return nil
}

type currentCallKey struct{}

// callContext is what the runner puts in an executor's context.
type callContext struct {
	ref    CallRef
	runner Runner
}

// CurrentCall returns the turn and tool call that the executor running under
// ctx is serving. It reports false outside an executor that the runner
// started. A tool that starts other work for the call, such as a child turn,
// uses it to derive stable identities, so a retry finds the same work.
func CurrentCall(ctx context.Context) (CallRef, bool) {
	c, ok := ctx.Value(currentCallKey{}).(callContext)
	return c.ref, ok
}

// CurrentRunner returns a copy of the runner that runs the call CurrentCall
// names, at call time. Its Agent is the turn's resolved agent, so Model shows
// the model in use now. Its Hooks are the runner's followed by the hooks of
// the current command. A tool that starts a child turn uses it to inherit the
// client, recorder, hooks, and cancel behavior of its parent, so the child
// follows the host when the host changes any of them. It reports false
// outside an executor that the runner started.
func CurrentRunner(ctx context.Context) (Runner, bool) {
	c, ok := ctx.Value(currentCallKey{}).(callContext)
	return c.runner, ok
}

func withCurrentCall(ctx context.Context, ref CallRef, r Runner) context.Context {
	return context.WithValue(ctx, currentCallKey{}, callContext{ref, r})
}

// callRunner returns the runner as a child turn should inherit it.
func (r *runtime) callRunner() Runner {
	out := r.runner
	out.Agent = r.agent
	out.Hooks = r.hooks
	return out
}
