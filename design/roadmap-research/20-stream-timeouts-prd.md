# 20. Connection and idle-stream timeouts — draft PRD

**Status:** Draft for review, 2026-09-28. **Size/difficulty:** M / Medium (roadmap estimate). **Research:** [comparative findings](20-stream-timeouts-research.md). **References:** `design/remaining-work.md:190-197`, `design/turn-interruption.md:90-108,145-164`. There is no prior dedicated timeout proposal.

## User problem

A Go application cannot tell Dashi to stop waiting for response headers quickly while allowing a productive model stream to run longer. The default five-minute `http.Client.Timeout` applies to the whole response and a silent stream has no narrower watchdog. When a timeout happens, a host needs to know whether it was transport startup, idle reading, the whole call, or its own Turn deadline, and whether a provider may already have acted or billed. A user should see an actionable stalled-call outcome and any partial answer/usage, not an unexplained cancel or an invisible retry.

## Goals and non-goals

**Goals:** Consistent transport timeout names across Dashi's HTTP model adapters; separate response-start, stream-read idle, and total-call bounds; keep `Limits.Deadline` distinct; preserve partial deltas, usage, exact attempt identity, and dispatch uncertainty; allow applications to set longer total duration for slow but active streams without disabling stall detection.

**Non-goals:** A job scheduler, Turn reaper, browser SSE/WebSocket reconnect policy, provider retry loop, TCP/DNS/TLS knobs for every custom transport, or a changed default before conformance tests establish provider behavior. Existing `http.Client` injection remains an escape hatch for host-specific transports.

## Proposed smallest API and behavior

```go
// Sketch; names subject to Go API review.
type TransportTimeouts struct {
    ResponseStart time.Duration  // request start through response headers
    StreamIdle    time.Duration  // blocked wait for next body bytes
    Total         *time.Duration // nil: existing 5m default; zero: no total cap
}
// Each Dashi HTTP model provider Config may set Timeouts TransportTimeouts.
```

Positive values enable a bound; negative values reject at construction. `ResponseStart == 0` and `StreamIdle == 0` mean off in the first slice, preserving today's behavior. `Total` nil preserves the five-minute default; explicit zero removes that *transport* ceiling, leaving caller context and `Limits.Deadline` in force. Example: `Timeouts{ResponseStart: 45*time.Second, StreamIdle: 90*time.Second, Total: ptr(20*time.Minute)}`. The exact defaults may be reconsidered after tests with the target providers; they are not silently changed by this feature.

`ResponseStart` begins immediately before HTTP dispatch and ends when headers arrive. It covers DNS, TCP, TLS, request write, and waiting for headers as one portable budget; it cannot diagnose which subphase stalled. `StreamIdle` starts with the first body read after successful headers and re-arms after every nonempty **raw byte** read, including SSE comments/heartbeats. It applies only while blocked in a read, so slow application processing is not mislabeled provider idleness. Its expiry closes the body and cancels only that model-call context. A user-provided HTTP client's shorter timeout still wins; the effective bounds must be documented.

The provider returns `llm.Error{Category: ErrorTransport, Reason: ReasonResponseStartTimeout | ReasonStreamIdleTimeout | ReasonCallTimeout, Cause: ...}` using Dashi's existing reason field. The Runner commits `StepModelSettled` with any partial text/content and streamed usage; missing output usage remains estimated by the existing settlement rule. This requires salvaging observed deltas in the ordinary `sendErr` branch, which today does so only when the parent context ends (`runner.go:1236-1300`). The timeout is a model-call failure, **not** `StopInterrupted` or `StopLimited` unless the parent host context or `Limits.Deadline` actually expired. Header timeout does not prove non-dispatch: the provider may have received the request before headers stalled. Stream-idle timeout definitely follows a response, but may precede provider-hosted actions. The existing `FailureDisposition` and `StopUncertain` path therefore remain conservative unless evidence proves rejection or no dispatch. No automatic resend is introduced.

### Developer flow

The host configures one timeout group when constructing its provider client, supplies its normal Turn deadline separately, then reads the Turn's settled model fact on failure. Logs/UI can say “model stream stopped receiving data” or “provider did not start responding” and retain the partial answer as provisional. A durable host may then reconcile an uncertain provider-hosted action before choosing a new attempt. The UI's live connection timeout must not be presented as a provider failure.

## Acceptance criteria

1. Shared HTTP adapters accept one documented timeout group; supported adapters apply it consistently or reject unsupported combinations at construction. A custom HTTP client retains its transport, proxy, TLS, and authentication configuration.
2. Response-start timeout fires while connect or headers stall, not after headers. Stream-idle timeout fires on a blocked body read, resets on bytes including heartbeat bytes, and does not fire merely because a caller spends time processing a delivered chunk. Total timeout still caps the full call when configured.
3. `Total=nil` preserves today's five-minute default; explicit longer and disabled totals behave as documented. The earlier of parent context, Turn deadline, and transport timers wins, and the recorded reason reflects that winner without relabeling user cancel as provider timeout.
4. Each timeout closes response resources and produces a typed `llm.Error.Reason` visible to callers. A model intent that may have dispatched remains recorded; partial output and usage are retained even when `Send` returns an empty result, and are not acted on as a final answer.
5. No timeout path retries implicitly. Unknown dispatch or hosted-tool effects remain uncertain, and continuation follows existing reconciliation rules. A proven pre-dispatch failure can be identified only from explicit transport evidence.
6. Focused `httptest` cases cover stalled connect/headers, headers with no body bytes, periodic heartbeat bytes, a slow consumer, total expiry during active bytes, host cancellation, Turn deadline, and late terminal data. At least one provider adapter test proves the group reaches real HTTP I/O; others use the shared helper contract.

## Risks and open decisions

- **Portable boundary:** `http.Client` customization can carry a shorter total or transport timeout. Decide whether to expose the effective duration in diagnostics; avoid modifying the caller's client in place.
- **Error shape:** Add only timeout values to existing `llm.Error.Reason`; preserve `Category`, `Disposition`, and original causes for `errors.Is`/`errors.As`. Confirm whether any provider uses a distinct timeout error that needs normalization.
- **Adapter coverage:** Some SDK-based or media adapters do not use `providerhttp.Client.Post`. Inventory before publication and document any adapter that cannot honor stream-idle rather than silently accepting an ignored setting.
- **Timer point:** Test that the idle guard covers raw reads, not parsed model events, and that timeout cleanup cannot race a final result into a false failure. This is a transport responsibility, not a Turn scheduling feature.
