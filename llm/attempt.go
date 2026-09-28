package llm

// Call is one dispatch of a prepared request, the argument to Client.Send.
//
// Attempt is an explicit field rather than a context value because a client
// may depend on it for correctness, as an idempotency key for durable work.
// In the signature it is visible to every implementation and cannot be
// silently dropped by a wrapper that replaces the context.
type Call struct {
	// Request is the prepared request to send.
	Request Request
	// Attempt is the caller's identity for this dispatch. It is zero when the
	// caller records no attempts, as for a direct Send or Generate.
	Attempt Attempt
	// Sink observes provisional deltas. It may be nil.
	Sink Sink
}

// Attempt is the caller's identity for one model dispatch. A caller that
// records model calls, such as Kernel's turn engine, sets it on Call so a
// client can tie the dispatch to its own durable work: a queued job, an
// idempotency key, or a credit hold. A client must still work when it is
// zero.
//
// Kernel's engine sets AttemptID to the ModelFact.AttemptID it records in the
// turn's StepModelIntent step and repeats in StepModelSettled or
// StepModelReconciled, and to Event.AttemptID on live updates, so clients,
// recorders, and observers can correlate by it. Under Kernel:
//
//   - Send receives an ID only after its intent is durably recorded, and
//     Kernel calls Send at most once per ID. A client may use it as an
//     idempotency key: if a job with this ID already exists (for example
//     after the client's own lost acknowledgement), join or reuse it instead
//     of dispatching again.
//   - Recovery never reuses an ID. An interrupted call is settled under its
//     own ID (Interrupted is set), and the resend is a new intent with a new
//     ID. Work still running under the old ID is stale: Kernel will not act on
//     its output, though its usage is the application's to account. After a
//     worker crash leaves the intent unsettled, a host that finds the old job
//     finished can record it with Accept{CallID: id, ModelResult: ...} before
//     Continue, and Kernel will not send again. The same Accept settles an
//     attempt left uncertain, for example by a provider-hosted tool.
type Attempt struct {
	// TurnID identifies the caller's unit of work, such as a Kernel turn.
	TurnID string
	// AttemptID identifies this dispatch. It is unique within the turn.
	AttemptID string
}

// IsZero reports whether the caller supplied no attempt identity.
func (a Attempt) IsZero() bool {
	return a == Attempt{}
}
