package dashi

// Next is what a host does next with a turn, read from its recorded stop.
// TurnView.Next gives it, so a host need not decode Stop's fields.
type Next string

const (
	// NextNone means there is nothing to do: the turn completed, or it
	// ended for good (Stop.Terminal), or it has not started.
	NextNone Next = "none"
	// NextRunning means no stop is recorded and the turn is known to be
	// live. TurnView cannot know liveness and never returns it; a caller
	// that tracks workers can refine NextResume to it.
	NextRunning Next = "running"
	// NextResume means no stop follows the latest Start or Continue, and
	// the turn is not known to be live: its worker may have crashed.
	// Continue resumes it once the worker is known to be gone.
	NextResume Next = "resume"
	// NextAnswer means the turn waits for approvals or tool results; see
	// TurnView.Pending. Settle each one, then Continue.
	NextAnswer Next = "answer"
	// NextContinue means Continue can run the turn now.
	NextContinue Next = "continue"
	// NextRaiseBudget means a cost or token cap stopped the turn. Continue
	// with a raised Budget; when Stop shows no model attempts left, raise
	// Continue.ModelAttempts as well.
	NextRaiseBudget Next = "raise_budget"
	// NextRaiseAttempts means the model-call limit stopped the turn.
	// Continue with a higher Continue.ModelAttempts.
	NextRaiseAttempts Next = "raise_attempts"
	// NextResolve means a cause outside the turn must be fixed first: a
	// failure, a refusal, a host limit, an uncertain effect to settle, or
	// an interruption the turn cannot resume from. Fix it and Continue, or
	// Cancel the turn.
	NextResolve Next = "resolve"
)

// Next maps the turn's recorded stop to the host's next action. It reads
// the stop the record holds, not the projected Stop: a turn with no stop
// after its latest Start or Continue gives NextResume. A refusal never
// gives NextContinue, so a host is never told to ask the model again after
// it refused.
func (v TurnView) Next() Next {
	if v.Revision == 0 {
		return NextNone
	}
	if !v.stopRecorded {
		return NextResume
	}
	stop := v.Stop
	switch {
	case stop.Terminal, stop.Kind == StopCompleted, stop.Kind == StopCanceled:
		return NextNone
	case stop.Kind == StopRefused, stop.Kind == StopFailed:
		return NextResolve
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
		return NextAnswer
	case StopInterrupted, StopUncertain:
		if v.ReadyToContinue {
			return NextContinue
		}
	}
	return NextResolve
}

// limitNext maps a limited stop that has not ended the turn.
func (v TurnView) limitNext() Next {
	if v.Stop.Limit == nil {
		// One call hit its output or context window.
		return NextResolve
	}
	switch v.Stop.Limit.Kind {
	case LimitModelAttempts:
		return NextRaiseAttempts
	case LimitCost, LimitOutputTokens, LimitTotalTokens:
		// A cap stop that Continue could run past without a raise was not
		// caused by the cap: the cost of the next call could not be
		// bounded, and a raise would stop it again.
		if v.ReadyToContinue {
			return NextResolve
		}
		return NextRaiseBudget
	case LimitProviderPauses:
		if v.ReadyToContinue {
			return NextContinue
		}
	}
	return NextResolve
}
