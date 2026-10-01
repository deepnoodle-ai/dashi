package llm

// Call is one dispatch of a prepared request, the argument to Client.Send.
//
// Identity is an explicit field rather than a context value because a client
// may depend on it for correctness, as an idempotency key for durable work.
// In the signature it is visible to every implementation and cannot be
// silently dropped by a wrapper that replaces the context.
type Call struct {
	// Request is the prepared request to send.
	Request Request
	// Identity is the caller's identity for this dispatch. It is zero when the
	// caller records no model calls, as for a direct Send or Generate.
	Identity CallIdentity
	// Sink observes provisional deltas. It may be nil.
	Sink Sink
}

// CallIdentity is the caller's identity for one model dispatch. A caller that
// records model calls, such as Dashi's runner, sets it on Call so a client can
// tie the dispatch to its own durable work: a queued job, an idempotency key,
// or a credit hold. A client must still work when it is zero.
//
// Dashi's runner sets ModelCallID to the ModelFact.ModelCallID it records in
// the turn's StepModelCallStarted step and repeats in StepModelCallFinished or
// StepModelCallFinishedByHost, and to Event.ModelCallID on live updates, so
// clients, recorders, and observers can match records by it. Under Dashi:
//
//   - Send receives an ID only after its model_call_started step is durably
//     recorded, and Dashi calls Send at most once per ID. A client may use it
//     as an idempotency key: if a job with this ID already exists (for
//     example after the client's own lost acknowledgement), join or reuse it
//     instead of dispatching again.
//   - Recovery never reuses an ID. An interrupted call is finished under its
//     own ID (dashi.ModelFact.Interrupted names the cause), and the resend is
//     a new model call with a new ID. Work still running under the old ID is
//     stale: Dashi will not act on its output, though its usage is the
//     application's to account. After a worker crash leaves the started call
//     unfinished, a host that finds the old job finished can record it with
//     Resolve{CallID: id, ModelResult: ...} before Continue, and Dashi will
//     not send again. The same Resolve finishes a model call whose result is
//     unknown, for example because of a provider tool.
type CallIdentity struct {
	// TurnID identifies the caller's unit of work, such as a Dashi turn.
	TurnID string
	// ModelCallID identifies this dispatch. It is unique within the turn.
	ModelCallID string
}

// IsZero reports whether the caller supplied no call identity.
func (a CallIdentity) IsZero() bool {
	return a == CallIdentity{}
}
