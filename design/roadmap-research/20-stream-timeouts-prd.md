# 20. Model HTTP response and stream timeouts — PRD

**Status:** Reviewed for the first implementation slice, 2026-09-28. **Size/difficulty:** M / Medium. **Research:** [comparative findings](20-stream-timeouts-research.md). **Reference:** [roadmap item](../remaining-work.md#20-connection-and-idle-stream-timeouts), [Turn interruption](../turn-interruption.md). No prior dedicated timeout proposal exists.

## Problem and intended user outcome

An application using Dashi cannot bound how long a model takes to begin responding while still allowing a long, productive stream. The default HTTP client has one five-minute whole-call timeout; a supplied client may have a different one or none. If response bytes stop midstream, there is no separate idle bound. Durable hosts need to present the right cause, retain any partial output and usage, and decide whether an attempted call can safely run again. A timeout must never appear as a user cancellation or trigger an unseen retry.

**Success:** A host can configure response-start, stream-read idle, and total-call durations independently on the supported HTTP model clients. A saved Turn identifies which Dashi timeout fired, retains observed partial work and a conservative charge, and preserves dispatch uncertainty. Existing callers retain their timeout behavior unless they opt in to a new bound.

## First-slice scope

The option applies to the three adapters that call `providerhttp.Post`: OpenAI Responses, OpenAI Chat Completions, and Anthropic Messages. Their wrappers—Grok, Meta, DeepInfra, Mistral, OpenRouter, and Ollama—forward the same option. Gemini's model calls use the Google SDK and a distinct guarded `RoundTrip`; Gemini, media image/video clients, caller-defined `llm.Client` implementations, and browser live-update connections are outside this slice. Their constructors will not claim to accept these settings. The provider guide must show this support boundary plainly.

This slice does not add retries, TCP/DNS/TLS-specific knobs, a Turn reaper, or a provider-wide default change. `Limits.Deadline` remains the durable Turn deadline. An application's `http.Client` remains the escape hatch for custom transport, proxy, and TLS behavior.

## Proposed developer API and behavior

```go
// Public shared value in llm.
type HTTPTimeouts struct {
    ResponseStart time.Duration  // dispatch through response headers
    StreamIdle    time.Duration  // time blocked in each stream body read
    Total         *time.Duration // nil: existing default; zero: no Dashi total cap
}

// Example, after total := 20 * time.Minute:
// responses.Config{Timeouts: llm.HTTPTimeouts{
//     ResponseStart: 30*time.Second,
//     StreamIdle: 90*time.Second,
//     Total: &total,
// }}
```

Zero `ResponseStart` and `StreamIdle` leave those bounds off; negative durations are rejected by `New`. `Total == nil` retains today's behavior: a five-minute Dashi default when `Config.Client == nil`, or exactly the supplied `http.Client.Timeout` when a client is supplied. A positive `Total` adds a Dashi-owned whole-call bound. An explicit zero removes Dashi's five-minute default; it cannot disable a supplied client's own timeout. A shorter caller context, Turn deadline, or client timeout still wins. The configured client is copied for per-call use, never mutated in place.

`ResponseStart` starts just before HTTP dispatch and ends at response headers. It includes DNS, TCP, TLS, upload, and provider header wait as one budget; the result cannot identify which subphase stalled. `StreamIdle` covers each blocking raw response-body `Read` after headers for streaming calls. It re-arms on every nonempty byte read, including SSE comments and heartbeats. Processing a chunk in the application does not consume idle time. `Total` includes response startup and body consumption, including an otherwise active stream. Each expired Dashi timer cancels that one call and closes its response body. A timer must not leak or relabel an earlier parent-context cancellation. A caller-supplied client's own shorter timeout remains a transport error unless Dashi can establish its source without guessing.

On an owned expiry, direct `llm.Client.Send` returns `*llm.Error` with `ErrorTransport`, the original cause preserved for `errors.Is` / `errors.As`, and a distinct reason: `ReasonResponseStartTimeout`, `ReasonStreamIdleTimeout`, or `ReasonCallTimeout`. After headers, each OpenAI Responses, Chat Completions, and Anthropic stream decoder normalizes only a Dashi-owned timeout read cause while retaining its partial `Result`; a protocol error stays a protocol error. Parent cancellation takes priority when it happens first. The Runner stores that reason in a new optional `ModelFact.ErrorReason` on the existing `StepModelSettled` record; older records simply lack the field. It records `StopUncertain` when dispatch or effects remain unknown, as it does for other transport failures. A response-start timeout does not prove that no request reached the provider; a stream-idle timeout proves receipt of headers, not that provider-hosted actions finished. No timeout alone supplies `FailureNotDispatched` or `FailureRejected` evidence.

When a stream fails, the settlement keeps the provider's partial `Result` where available; if it lacks content or usage, the Runner fills those gaps from observed deltas without overriding more complete provider data. The saved result is incomplete and never becomes an actionable final answer or tool call. Accounting marks usage estimated when the call ended before final usage, even if an earlier usage frame exists, and includes visible output where provider usage omitted it. The intent and attempt ID remain stable and visible. A host can then display “model stream stopped receiving data,” keep the provisional answer, and reconcile any uncertain effect before choosing another attempt.

## Acceptance criteria

1. Each of the three base adapters and six wrappers exposes and forwards the same option; unsupported clients do not accept a silently ignored option. Documentation has one configuration example and a support table.
2. With a controllable HTTP test server, startup expiry occurs before headers, read-idle expiry occurs only during a blocked stream body read, raw heartbeat bytes reset idle, and slow consumer processing does not cause an idle expiry. Total expiry can end an active stream.
3. Nil, positive, zero, and invalid duration values behave as specified for both the default and a supplied `http.Client`. Redirect and no-hidden-retry behavior remain unchanged; the supplied client is not mutated.
4. Direct calls expose the right typed reason for a Dashi-owned expiry. The Runner persists that reason on `ModelFact` without changing old-record replay, and records parent cancellation or `Limits.Deadline` as their existing stop kinds when those contexts win.
5. A midstream failure with mixed text and structured content deltas plus partial usage settles observed content in order without duplication, marks accounting estimated, closes resources, and never replays incomplete content as an actionable final result. Unknown dispatch and hosted effects remain uncertain, with no automatic resend.
6. Focused tests cover response startup, idle body, heartbeat, slow consumer, active-stream total expiry, parent cancellation, Turn deadline, late terminal data, partial usage, and the shorter timeout of a supplied client. Each of the three base adapters has a direct `Send` idle-timeout test asserting its typed reason and partial result; shared transport tests cover the remaining mechanics. A timer firing after successful terminal data cannot overwrite success.

## Risks and review questions

- **Public type placement:** `llm.HTTPTimeouts` minimizes duplicated config types; review whether an HTTP-specific type belongs beside `llm.Error` or in another existing public package. Avoid creating a package solely for this value without a clear user benefit.
- **Custom transports:** A custom `RoundTripper` may return bodies that do not unblock on request cancellation. Tests can prove standard `net/http` behavior; documentation must state this escape-hatch responsibility.
- **Partial accounting:** The current settlement estimator treats interruption specially. The implementation must also recognize incomplete transport failures without changing charges for proven pre-dispatch rejection or fully settled results.
