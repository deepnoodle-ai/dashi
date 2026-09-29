package dashi

import (
	"context"
	"errors"
	"sync"
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
	turn   func() *replayState
}

// CurrentCall returns the turn and tool call that the executor running under
// ctx is serving. It reports false outside an executor that the runner
// started. A tool that starts other work for the call, such as a child turn,
// uses it to derive stable identities, so a retry finds the same work.
func CurrentCall(ctx context.Context) (CallRef, bool) {
	c, ok := ctx.Value(currentCallKey{}).(callContext)
	return c.ref, ok
}

// CurrentTurn returns the view of the turn whose tool call ctx serves, as of
// the call's start, including its recorded agent: Agent.Model is the model in
// use now. As in a hook's view, the turn is running, so Stop is empty. A
// Toolset sees the turn as of the model call it offers tools for.
// CurrentTurn reports false outside the runner.
func CurrentTurn(ctx context.Context) (TurnView, bool) {
	var turn func() *replayState
	if c, ok := ctx.Value(currentCallKey{}).(callContext); ok {
		turn = c.turn
	} else if c, ok := ctx.Value(currentRunnerKey{}).(runnerContext); ok {
		turn = c.turn
	}
	if turn == nil {
		return TurnView{}, false
	}
	s := turn()
	if s == nil {
		return TurnView{}, false
	}
	return snapshotView(s.TurnView), true
}

// CurrentRunner returns a copy of the runner that runs the call CurrentCall
// names, at call time. Its Hooks are the runner's followed by the hooks of the
// current command. A tool that starts a child turn uses it to inherit the
// client, recorder, hooks, and cancel behavior of its parent, so the child
// follows the host when the host changes any of them; CurrentTurn gives the
// parent's agent. Its Recorder is nil when the host set none. A Toolset sees
// the same runner while the runner asks it for tools. CurrentRunner reports
// false outside the runner.
func CurrentRunner(ctx context.Context) (Runner, bool) {
	if c, ok := ctx.Value(currentCallKey{}).(callContext); ok {
		return c.runner, true
	}
	c, ok := ctx.Value(currentRunnerKey{}).(runnerContext)
	return c.runner, ok
}

// currentRunnerKey marks a context that carries the runner but no call. The
// runner sets it when it asks a Toolset for tools, so a toolset can shape its
// definitions to the host's recorder.
type currentRunnerKey struct{}

// runnerContext is what the runner puts in a Toolset.Tools context.
type runnerContext struct {
	runner Runner
	turn   func() *replayState
}

// toolsContext returns ctx for a Toolset.Tools call.
func (r *runtime) toolsContext(ctx context.Context) context.Context {
	return context.WithValue(
		ctx,
		currentRunnerKey{},
		runnerContext{r.callRunner(), r.turnAt()},
	)
}

func (r *runtime) withCurrentCall(ctx context.Context, ref CallRef) context.Context {
	return context.WithValue(
		ctx,
		currentCallKey{},
		callContext{ref, r.callRunner(), r.turnAt()},
	)
}

// turnAt returns a function that replays the turn as recorded now, once and
// only when asked. Commits only append steps, so the capped slice stays
// fixed while the run goes on.
func (r *runtime) turnAt() func() *replayState {
	n := len(r.turn.Steps)
	turn := Turn{ID: r.turn.ID, Steps: r.turn.Steps[:n:n]}
	return sync.OnceValue(func() *replayState {
		s, err := replay(turn)
		if err != nil {
			return nil
		}
		return s
	})
}

// callRunner returns the runner as a child turn should inherit it.
func (r *runtime) callRunner() Runner {
	out := r.runner
	out.Hooks = r.hooks
	if r.implicitRecorder {
		// The private per-Run recorder ends with the Run. A child must not
		// inherit it, so the tool sees that the host set no recorder.
		out.Recorder = nil
	}
	return out
}
