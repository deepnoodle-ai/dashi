# 19. Durable retries after proven rejection — draft PRD

**Status:** Draft proposal, 2026-09-28. **Roadmap:** [item 19](../remaining-work.md). **Basis:** [source research](19-durable-retries-research.md). **Decision sought:** Add an opt-in, recorded model retry wait only where Dashi has evidence of rejection.

## User problem

A developer running a durable Turn should not have to write a separate retry loop for a provider's explicit rate-limit rejection. Today Dashi exposes `Retryable` and `RetryAfter`, but a rejected 429 stops failed. A worker that sleeps through backoff wastes capacity and loses its plan on a crash. Retrying a 503 or broken connection just because it looks transient can repeat an accepted request, including provider-hosted work. Operators need to see which attempts ran, why another was scheduled and when it may start.

## Goals and non-goals

**Goals:** safe opt-in retry after a proven, retryable model rejection; durable due time that survives reload; one recorded identity and accounting entry per attempt; cancellation, budget and policy reevaluation before each new dispatch; a simple `waiting` outcome a host can schedule.

**Non-goals:** retries after unknown acceptance, partial output/usage or timeout; tool retries; provider/model fallback; a built-in job queue or sleeping worker; generic retries of every 5xx; changing `llm.Client.Send`'s one-dispatch contract. `FailureNotDispatched` may be safely repeatable but remains outside this first automatic policy until retryability of local failures is designed.

## Smallest API and flow

Pin an optional policy with the Agent, alongside its existing `Limits`:

```go
agent.ModelRetry = dashi.ModelRetryPolicy{
    MaxRetries: 2,                  // zero (default) disables automatic retry
    MaxWait:    2 * time.Minute,    // cap for any one scheduled wait
}
```

`Limits.ModelAttempts` remains the hard total attempt cap; `MaxRetries` limits consecutive retries of one model call. A fixed, documented bounded exponential delay (starting at one second) keeps the public surface small. The selected wait is at least a valid provider `RetryAfter`. If the hint exceeds `MaxWait` or the due time exceeds the Turn deadline, do not schedule an earlier retry; return a typed failed/limited stop with the last error and hint.

When `Send` returns, the Runner schedules a retry only if its final result has no ID, output or usage; the typed error says `FailureRejected` **and** `Retryable`; context is live; and retry/attempt/deadline limits permit another call. The existing shared HTTP adapter makes 429 eligible, but generic 503 is `FailureUnknown` (`internal/providerhttp/http.go:286-305,330-336`; `errors.go:113-126`) and therefore **not eligible** despite `Retryable=true`. A provider adapter may classify a particular 5xx as rejected only with stronger evidence that its request was not accepted; status alone is insufficient. A request exposing side-effecting provider-hosted tools should require proof that those effects could not have run, or remain ineligible.

Record the failed attempt's structured retry decision, source hint and exact `NotBefore` **on its `StepModelSettled`**, so a crash between settlement and a later stop cannot lose the wait. `Replay` derives `Stop{Kind: StopWaiting, Retry: {AttemptID, NotBefore}}` from that fact; `Outcome.Stop.Retry` gives the host a typed scheduling target. The ordinary `StepStopped` may present the wait, but is not its sole authority. Do not persist provider error bodies, credentials or prepared request bytes in the retry fact. A settled rejection counts toward `ModelAttempts`; the scheduled wait does not count toward the interruption limit.

The host saves the returned Turn and arranges a wake at `NotBefore` using its own queue/timer. `Continue` before that time returns `ErrNotReady` without a commit or model call. On or after it, a new worker uses the caller's normal `Continue.History`, gets a fresh attempt ID, and reruns cancellation/deadline and cost/attempt checks, `BeforeModel`, preparation, `BeforeSend` and host admission. A hook may deliberately change the next request; its digest then identifies the new bytes. `Cancel` during a wait ends the Turn with no further dispatch. If the policy or budget refuses the next attempt, return the existing refused/limited stop with the rejected attempt preserved. For a worker crash or unknown persistence outcome, reload and replay the Turn rather than inventing a new retry decision.

## Acceptance criteria

1. With opt-in policy, a 429 carrying a valid `Retry-After` and no partial result records one rejected attempt plus a due time, returns typed `waiting`, and makes no second dispatch before that time. Reloading on a different Runner produces the same wait and eventual new attempt ID.
2. A 503, 408/409/425, transport timeout, stream error, partial result/usage, missing disposition or `Retryable=false` never auto-retries. Unknown work remains `StopUncertain` and needs existing reconciliation; a terminal rejection remains failed.
3. Repeated delivery of the same `Continue.CommandID` or concurrent wake attempts cannot duplicate dispatch. An early wake commits nothing. An unknown recorder write is resolved by loading authoritative steps before any call.
4. Every rejection increments model attempts and exposes failure/charge evidence; the wait preserves the provider hint and chosen due time. Retry does not consume `Limits.Interruptions`. Budget and `BeforeModel`/`BeforeSend` are rechecked and may stop the next call without dispatch.
5. Cancellation during the wait is terminal. Exhausted retry count, `ModelAttempts`, `MaxWait` and deadline produce a typed stop with the last provider failure visible. An unrecognized or malformed retry fact fails replay validation.
6. A test with a counting transport proves one wire request per Dashi attempt, including custom HTTP clients. Documentation warns that SDK or transport retry layers must be disabled or independently audited.

## Risks and open decisions

- **Meaning of rejection:** Dashi's shared HTTP classifier intentionally treats 5xx as unknown. Provider-specific proof and provider-hosted side effects require review before widening the eligible set. Do not infer safety from a transient status or absence of streamed output.
- **Wait representation:** `StopWaiting` already means external approval/tool work. A typed `Stop.Retry` must make scheduling distinct without a second stop kind. The settled fact must be sufficient to reconstruct the wait after a crash.
- **Time and policy:** Decide the exact default backoff, clock injection for deterministic tests, and whether a provider hint over `MaxWait` is a failed or limited stop. These choices must be stable on a pinned Agent and recorded as the chosen absolute due time.
- **Manual continuation:** Existing `Continue` can resume a known failed Turn. Clarify whether a host may manually override an exhausted retry policy; automatic retry must never use that escape hatch.
