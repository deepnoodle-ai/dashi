# R03: Know what to do after a provider failure

[All examples](README.md) · [Review](../README.md)

The user wants a temporary provider problem to produce an appropriate next step.
The saved metadata can support that behavior. It does not implement the behavior.

## Sequence where it matters

1. The user asks the agent to continue its work.
2. At 11:00:00, the provider rejects the request with HTTP 429 and a 30-second
   retry hint. Dashi stops the run and records the failure.
3. At 11:00:05, the application exits before arranging the next action.
4. It restarts at 11:00:20 and loads the saved Turn.
5. A product with a retry policy needs to distinguish a known rejection from
   an invalid credential or a request whose remote outcome is unknown.
6. If that policy permits retry, it needs the original due time, 11:00:30.
   Starting a new 30-second delay on restart would move it to 11:00:50.

Today Dashi's transient llm.Error has the category and delay, but the saved
ModelFact does not retain both. Failure metadata keeps them available after
reload. A host can then show an appropriate repair action or schedule a retry
under its existing recovery and spending rules. An unknown send still needs
recovery; the hint cannot authorize another request.

**Current Swarm boundary:** Swarm currently reports suspended failed/unknown
Turns as requiring attention. It has generic recovery backoff, not this
provider-delay policy. The sequence above is a target application behavior,
not a claim that adding Failure automatically improves Swarm's recovery.
[Current failure handling](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1374)
and [generic recovery backoff](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/manager/recovery/recovery.go#L488).

**Judgment:** Useful durable data if the application consumes it. Define the
retry/credential-repair feature before claiming a user benefit. An application
could instead intercept provider errors and save its own safe metadata, but
Dashi currently owns the point where those errors become saved Turn facts.

## Before: current code

From [dashi/llm/llm.go:749–774](https://github.com/deepnoodle-ai/dashi/blob/ea65bb432ae12893cfe79e2d9bdcbbe1ea8724ed/llm/llm.go#L749). Surrounding definitions are omitted.

<!-- source: dashi llm/llm.go 749 774 -->
```go
// Category allows decisions without parsing provider-specific error text.
Category ErrorCategory
// Reason refines Category when a provider reports a failure Dashi
// recognizes; such failures keep the category the provider's response
// implies, usually ErrorInvalidRequest. Classification is best effort:
// empty means unclassified, not that a reason is ruled out. Which
// provider codes, statuses, and messages are recognized is an
// implementation detail that may change as providers change.
Reason ErrorReason
// SendStatus records what is known about this model call's remote effect.
// An absent value is unknown and must not authorize a retry.
SendStatus SendStatus

// Status is the HTTP status when a response was received, or zero.
Status int

// Code retains the provider error code, which may refine retry advice.
Code string
// Retryable is a hint from an explicit HTTP rejection, not permission to
// retry a call whose effect is unknown. The caller owns recording and retry
// policy.
Retryable bool

// RetryAfter preserves the provider delay hint; zero means none was
// supplied.
RetryAfter time.Duration
```

From [dashi/turn_facts.go:177–184](https://github.com/deepnoodle-ai/dashi/blob/ea65bb432ae12893cfe79e2d9bdcbbe1ea8724ed/turn_facts.go#L177). Surrounding definitions are omitted.

<!-- source: dashi turn_facts.go 177 184 -->
```go
Hidden []string    `json:"hidden,omitempty"`
Result *llm.Result `json:"result,omitempty"`
Error  string      `json:"error,omitempty"`
// ErrorReason is a stable, optional model failure code. It is absent on
// older records and does not replace the human-readable Error.
ErrorReason llm.ErrorReason `json:"error_reason,omitempty"`
SendStatus  llm.SendStatus  `json:"send_status,omitempty"`
Evidence    string          `json:"evidence,omitempty"`
```

## After: implemented in [#252](https://github.com/deepnoodle-ai/dashi/pull/252), awaiting merge

The data API is implemented. Scheduling and UI functions below illustrate
application code that still has to be built.

```go
// Older records have no Failure metadata.
if view.Next() == dashi.NextFix && view.Failure != nil {
    failure := view.Failure
    safeRetry := failure.SendStatus == llm.SendStatusNotSent ||
        failure.SendStatus == llm.SendStatusProviderRejected
    switch {
    case safeRetry && failure.Retryable:
        // Persist an absolute due time; restarting must not reset the delay.
        scheduleRetryAt(turnID, failure.RecordedAt.Add(failure.RetryAfter))
    case failure.Category == llm.ErrorAuthentication:
        requestCredentialRepair(turnID)
    default:
        requestRecoveryReview(turnID)
    }
}
```

**Observable result:** Reload retains the 429 classification and original delay. The user's next action changes only when the host adds a policy that uses them.

**Developer benefit:** Handle a useful saved failure without depending on provider text or losing the delay after restart.

**Application responsibility:** Own scheduling and credential repair. Do not save raw error bodies or authorize a resend from an unknown result.

The before excerpt shows the information available on `llm.Error` and the smaller set retained on `ModelFact`; it does not claim Swarm currently parses every error string.
