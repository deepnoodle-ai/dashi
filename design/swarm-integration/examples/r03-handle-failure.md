# R03: Know what to do after a provider failure

[All examples](README.md) · [Review](../README.md)

A provider rejects a request with HTTP 429 and asks the caller to wait 30 seconds.
The process restarts before scheduling that retry. A second case has unknown
execution: the request may have been accepted before the connection broke.

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

## After: Proposed API sketch

The proposed Dashi helper does not exist yet. Application storage methods
show the responsibilities that remain with the caller.

```go
// Proposed Failure fields extend the existing Next API.
if view.Next() == dashi.NextFix {
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

**Observable result:** The saved 429 would retain its retry delay. Unknown execution would still require a recovery decision, even if a provider error carries a retry hint.

**Developer benefit:** Handle a useful saved failure without depending on provider text or losing the delay after restart.

**Application responsibility:** Own scheduling and credential repair. Do not save raw error bodies or authorize a resend from an unknown result.

The before excerpt shows the information available on `llm.Error` and the smaller set retained on `ModelFact`; it does not claim Swarm currently parses every error string.
