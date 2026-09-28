package llm

import "context"

// Attempt is the caller's identity for one model call. A caller that records
// model calls, such as Kernel's turn engine, puts it on the context passed to
// Client.Prepare and Client.Send so a client can tie the call to its own
// durable work: a queued job, an idempotency key, or a credit hold. Calls made
// outside such a caller carry no Attempt, and a client must still work.
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
//   - Prepare receives the ID the intent will carry if Kernel records it.
//     Preparation has no effect; when the turn stops before recording, a
//     later Prepare may see the same ID again.
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
	// AttemptID identifies this model call. It is unique within the turn.
	AttemptID string
}

type attemptKey struct{}

// WithAttempt returns a context carrying attempt identity for a model call.
func WithAttempt(ctx context.Context, attempt Attempt) context.Context {
	return context.WithValue(ctx, attemptKey{}, attempt)
}

// AttemptFrom returns the attempt identity on ctx. False means the caller
// supplied none, as for a direct Prepare or Send outside Kernel.
func AttemptFrom(ctx context.Context) (Attempt, bool) {
	attempt, ok := ctx.Value(attemptKey{}).(Attempt)
	return attempt, ok
}
