package dashi

// Next is what a host does next with a turn, read from its recorded stop.
// TurnView.Next gives it, so a host need not decode Stop's fields.
type Next string

const (
	// NextNone means there is nothing to do: the turn completed, or it
	// ended for good (Stop.Ended), or it has not started.
	NextNone Next = "none"
	// NextRunning means no stop is recorded and the turn is known to be
	// live: the view is one a hook got during a run. A caller that tracks
	// workers can also refine NextRecover to it.
	NextRunning Next = "running"
	// NextRecover means no stop follows the latest Start or Continue, and
	// the turn is not known to be live: its worker may have crashed. Once
	// the worker is known to be gone, Continue resumes it if
	// ReadyToContinue is true. Otherwise Continue gives ErrNotReady: first
	// resolve each Pending item (for example, supply the result of a model
	// call whose result is unknown), or Cancel the turn.
	NextRecover Next = "recover"
	// NextResolvePending means the turn waits for approvals or tool
	// results; see TurnView.Pending. Resolve each one, then Continue.
	NextResolvePending Next = "resolve_pending"
	// NextContinue means Continue can run the turn now.
	NextContinue Next = "continue"
	// NextRaiseBudget means a cost or token cap stopped the turn. Continue
	// with a raised Budget; when Stop shows no model calls left, raise
	// Continue.ModelCalls as well.
	NextRaiseBudget Next = "raise_budget"
	// NextRaiseModelCalls means the model-call limit stopped the turn.
	// Continue with a higher Continue.ModelCalls.
	NextRaiseModelCalls Next = "raise_model_calls"
	// NextFix means a cause outside the turn must be fixed first: a
	// failure, a refusal, a host limit, a call with an unknown result to
	// resolve, or an interruption the turn cannot resume from. Fix it and
	// Continue, or Cancel the turn.
	NextFix Next = "fix"
)

// Next maps the turn's recorded stop to the host's next action. A stop
// that View or RunResult inferred (Stop.Inferred) was never recorded, so
// it gives NextRecover. A hook's view during a run has no stop and gives
// NextRunning. A refusal never gives NextContinue, so a host is never told
// to ask the model again after it refused. Next reads only the view's
// fields, so a view decoded from JSON gives the same answer.
func (v TurnView) Next() Next {
	stop := v.Stop
	switch {
	case v.LastSeq == 0:
		return NextNone
	case stop.Inferred:
		return NextRecover
	case stop.Kind == "":
		return NextRunning
	}
	switch {
	case stop.Ended, stop.Kind == StopCompleted, stop.Kind == StopCanceled:
		return NextNone
	case stop.Kind == StopRefused, stop.Kind == StopFailed:
		return NextFix
	case v.Output != nil && v.ReadyToContinue:
		// Continue finishes the recorded output without a model call.
		return NextContinue
	}
	switch stop.Kind {
	case StopLimited:
		return v.limitNext()
	case StopWaiting:
		if v.ReadyToContinue {
			return NextContinue
		}
		return NextResolvePending
	case StopInterrupted, StopResultUnknown:
		if v.ReadyToContinue {
			return NextContinue
		}
	}
	return NextFix
}

// limitNext maps a limited stop that has not ended the turn.
func (v TurnView) limitNext() Next {
	if v.Stop.Limit == nil {
		// One call hit its output or context window.
		return NextFix
	}
	switch v.Stop.Limit.Kind {
	case LimitModelCalls:
		return NextRaiseModelCalls
	case LimitCost, LimitOutputTokens, LimitTotalTokens:
		// A cap stop that Continue could run past without a raise was not
		// caused by the cap: the cost of the next call could not be
		// bounded, and a raise would stop it again.
		if v.ReadyToContinue {
			return NextFix
		}
		return NextRaiseBudget
	case LimitProviderPauses:
		if v.ReadyToContinue {
			return NextContinue
		}
	}
	return NextFix
}
