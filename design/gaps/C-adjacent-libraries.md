# C. Adjacent libraries: ideas to borrow, pitfalls to avoid

Source review of Dashi main at 9d28264 on 2026-09-27; findings are directions,
not decisions.

**Names:** written before [decision 30](../decisions.md); read `Definition` as
`Agent`, `Engine`/`Bindings` as `Runner`, `Accept` as `Resolve`, and a command's
`ID` as `CommandID`.

Dashi already adopts the survey's hardest-won structural ideas. Its turn record
acknowledges each step before execution continues. Approvals are made per call
and bound to the recorded arguments. Limits stop the turn gracefully and do not
reset on continuation. Streaming and collected runs share one execution path,
and provider provenance and replay are recorded. The recurring pitfalls it still
has sit at the edges an application developer touches first: tool ergonomics,
provider stream normalization, identity, timeouts, and record growth. Three
probe tests run against a scratch copy of `main` (not committed) confirmed the
worst three:

- A tool that panics crashes the process.
- A provider that reuses a tool-call ID across model attempts leaves the turn
  stuck: it is uncertain and cannot continue.
- A Go error from any tool executor stops the whole turn as `uncertain`. That is
  Eino's "a tool error aborts the run" in a new form.

Counts: 15 ideas (6 Covered, 6 Partial, 3 Missing) and 15 pitfalls (5 Avoided, 7
Exposed, 3 at risk). Compaction, MCP, child turns, and sessions are not built
yet; they are noted as N/A where relevant.

## Ideas

| # | Idea | Seen in | Why it worked | Dashi status | Evidence | Direction |
|---|---|---|---|---|---|---|
| 1 | `iter.Seq2` from provider to agent; `break` cancels | ADK-Go, MAF-Go | One type for streaming and collected results, backpressure, no leaks | Covered | `engine.go:35-65`, `llm/stream.go:27` | Keep. Document that the `Stream` consumer is the caller, not a display fan-out (see note 5 on design-pitfalls.md) |
| 2 | Commit before proceeding; intent before effect; fencing | ADK-Go, Nvoken, pi harness | Tells "never ran" apart from "may have run" | Covered | `recorder.go:11-21`, `engine.go:303-344,567,216`, `turn.go` recording rule | Keep. Add recorder conformance cases (idea 14) |
| 3 | Per-call approval bound to the recorded call, arguments, and policy revision | MAF-Go (binding), OpenHands/MAF (anti-pattern: per batch) | Clients can't forge or swap an approval; safe siblings still run | Covered | `tools_loop.go:118-166`, `engine.go:244-259` (stale approval rejected) | Make the denial text final ("denied; do not retry") so the model does not re-request (MAF #8503) |
| 4 | Graceful cap: a final tool-less call and a typed `Limited` stop | MAF-Go; OpenHands/Eino as anti-patterns | Users get an answer, not an `ERROR` | Covered | `engine.go:478-511,456-461`, `turn.go:45-47` | Consider telling the model its remaining budget before the last call, not only on it |
| 5 | Tool failures reach the model as `is_error` results; details opaque by default; consecutive-error budget | MAF-Go, Eino tracker (#258, #928) | The model self-corrects; secrets don't leak; the run survives | Partial | Known failures project as `IsError` (`project.go:131`), but any executor Go error becomes uncertain (`tool/tool.go:310-315`, `tools_loop.go:25-27,245-250`) | See pitfall 5 and recommendation 2 |
| 6 | Repair model mistakes: unknown-tool result, argument feedback, aliases, lenient extra keys | Eino (`ToolAliases`), pi (#6278) | Cheap, and measurably reduces failed turns | Partial | Unknown tool gets a `NotExecuted` result (`tools_loop.go:75-80`); invalid arguments get a result (`:89-98`); no aliases; strict decoding (`tool/function.go:30,81`) | Ignore unknown keys by default, allow strict per tool, and add optional name/key aliases on `Definition` |
| 7 | API / Provider / Model split, with dialect quirks as data on the model | pi | One OpenAI-compatible adapter serves dozens of backends without forks | Partial | `Client.Provider/API/ModelInfo` (`llm/llm.go:526-546`); capabilities (`llm/model.go:90-108`). Quirks are per-client config (`completions/completions.go:29-46`); no `reasoning_content` echo | Move quirks such as `SystemRole` and reasoning echo into `ModelInfo` compat flags, and document each flag next to the 400 error it prevents |
| 8 | Provenance on messages, opaque reasoning replay, deterministic tool-ID remap | pi, MAF-Go (`ProtectedData`) | Fixes cross-provider hand-off 400s | Covered | `llm/llm.go:259-275`, `llm/opaque.go:30-54`, `design/provider-content-replay.md` | Build the all-pairs hand-off matrix as recorded fixtures |
| 9 | Typed provider errors plus a retry decision that callers can find | Eino (`ShouldRetry`), ADK tracker (#710, #542), pi (durable waits) | Handles 429/503 and "compress and retry" without guesswork | Partial | `llm.Error{Category, Disposition, RetryAfter}` (`llm/llm.go:548-615`); no context-overflow category; every failure stops the turn (`engine.go:615-619`). The no-retry `Client` is deliberate (`llm/llm.go:522-525`) | The rejection holds at the `llm` layer. Add an engine-level decision (`Policy` or `Limits`) that returns `Waiting{Retry, NotBefore}` for known-rejected attempts |
| 10 | Stream identity: preview message ID equals the recorded ID; a tool-start event | Nvoken, OpenHands v2, Eino | UIs reconcile previews with the saved record; clients see progress | Covered | `engine.go:564-566,581`; `StepToolStarted` yielded via `commit` | Keep. A reference reducer and fixtures would help hosts |
| 11 | Cached incremental projection | OpenHands | Record as the truth without O(history) rebuild per step | Missing | `Project` and `replay` rebuild from step 1 on every model call, on every tool in a batch, and on every `MemoryRecorder.Commit` (`project.go:13-14`, `tools_loop.go:50`, `engine.go:415,479`, `recorder.go:107`) | Implement design principle 5 before sessions land |
| 12 | Write-time invariants plus recorded cold-load repair ("view properties") | OpenHands, pi (63 bricked-session issues) | Stops one bad fact from breaking every later request | Partial | Replay validates each step (`state.go:84-92,580-582`); projection drops unsettled calls (`project.go:94-105`); no repair path, so a violating fact wedges the turn (pitfall 4) | Add typed repairs: record a projection adjustment and never rewrite a fact |
| 13 | Resource-keyed tool concurrency | OpenHands | Strictly better than a boolean "concurrency-safe" flag | Missing (safe default) | `Limits.ToolConcurrency` defaults to 1 (`turn.go:46`) | Add an optional `Resources(call)` interface before raising the default |
| 14 | Public scripted model and storage and dialect conformance suites | pi (`faux`, `ConformanceCase[]`), ADK (`sessiontestsuite`), Eino (#1029) | Users can test their agents; hosts prove their recorder matches | Missing | `scriptedClient` is test-only (`engine_test.go:15`); there is one fixture dir (`providers/openai/responses/testdata`) | Ship a `dashitest` package: scripted client, recorder cases, dialect corpus |
| 15 | Stock policies: stuck detection with one nudge; token/cost budget | OpenHands, MAF (#7472) | Loops and runaway spend are top complaints everywhere | Partial | The seams exist (`policy.go:49-54`; `BeforeSend` admission, `engine.go:554-563`), but no stock policy and no token or cost field in `Limits` | Ship opt-in policies and a way to compose them, so hosts don't write a four-method policy for each concern |

Also covered, and not repeated as rows:
- Steering through `AddMessages` and `BeforeModel.AddMessages` at step boundaries, with
  `ContextItem.Origin` (`turn.go:69-75`, `engine.go:519-528`).
- Refusing tool calls from truncated or unknown stops (`engine.go:658-663`).
- Deterministic attempt IDs (`engine.go:564`). The clock is not injectable
  (`engine.go:307,459`); that is a minor partial.

## Pitfalls

| # | Pitfall | Seen in (project, theme) | Dashi status | Evidence | Direction |
|---|---|---|---|---|---|
| 1 | Deciding tool use from early stream chunks; streaming path diverges from collected | Eino #806/#892; MAF #3848 | Avoided | The full result is classified (`engine.go:635-665`); both modes use `llm.Stream` (`engine.go:579`) | Keep a parity test per provider |
| 2 | OpenAI-compatible tool-call assembly: missing `index`, reused index with a new ID, empty arguments | Eino #631, #927, #493; pi #4228 | Exposed | Calls keyed by `index` only; a new ID overwrites the old one (`completions/stream.go:118-133`); empty arguments fail `Call.Validate` (`tool/tool.go:178`), which becomes a protocol error and then an uncertain stop | Key by ID, then index; start a new call on an ID change; normalize `""` to `{}`; add recorded fixtures |
| 3 | Tool errors abort the run | Eino #258; ADK #260 | Exposed (reframed) | Any executor `error` means uncertain, so the turn stops with `StopResultUnknown`. Probe: a `404` error from a tool gave `uncertain: tool outcome needs reconciliation` | Recommendation 2 |
| 4 | Provider-generated call IDs used as durable identity | Eino #1290; Nvoken discrepancy 1; pi #198 | Exposed | Replay rejects a repeated ID (`state.go:580-582`). Probe: two attempts that both return `call_0` leave the turn at model intent pending, `uncertain`, with `Continue` refused. A host recorder that doesn't replay on commit would store the step and then fail every later `Apply` | Recommendation 1 |
| 5 | Tool panic crashes the process | ADK-Go parallel dispatch | Exposed | No `recover` around `ex.Execute` (`tools_loop.go:229-237`). Probe: the test binary panicked | Recover at the executor boundary; record the call as uncertain with a panic reason |
| 6 | Hangs: stalled streams; a total timeout kills slow local streams | pi #8331, #3715, #4945 (240 issues) | Exposed | Default `http.Client{Timeout: 5*time.Minute}` (`internal/providerhttp/http.go:123`) covers the body read; no idle watchdog; `Limits.Deadline` is only checked before a call (`engine.go:459`) and does not bound `Send` or tools | Use a separate idle-stream timeout; derive the call context from `Deadline` |
| 7 | Unbounded tool results re-sent on every later call | ADK #1618 | Exposed | No size bound on `tool.Output` (`tool/tool.go:198-205`, `project.go:125-132`); only structured output is capped | Add a per-turn result cap with an actionable truncation notice, recorded as a projection adjustment |
| 8 | Strict arguments reject harmless extra keys | pi #6278, #5501 | Exposed | `DisallowAdditionalProperties` plus `DisallowUnknownFields` (`tool/function.go:30,81`); each rejection costs one of 8 default attempts | Lenient by default; strict per tool |
| 9 | No loop bound, or bound reset by re-entry | ADK #1287; MAF #7472 | Avoided | `ModelAttempts ≥ 1` is required (`turn.go:53`), counted across `Continue` (`state.go:canContinue`) | Add token and cost bounds when child turns arrive |
| 10 | Batch approval, implicit consent, conflated pending states | MAF #3054, #8079; OpenHands sdk#5092 | Avoided | Separate statuses: `approval_pending`, `waiting`, `uncertain` (`state.go:16-21`); `AddMessages` never approves | Keep one contract test per path (external, streamed, hosted) |
| 11 | Partial turns lost on cancel or crash | MAF #2889, #8157 | Avoided | Commit per step; a bounded commit context after cancel (`engine.go:315-320`) | Qualify with a durable host (the open gate) |
| 12 | Re-scanning the whole history per step; record growth | ADK; OpenHands sdk#1824; pi #8746 | At risk | Every turn's start fact copies the full prior session history (`engine.go:158`), so storage grows quadratically over a session. Replay and project run from scratch (idea 11) | Pin history by reference plus digest; cache the projection |
| 13 | One unknown event kind makes the conversation unloadable | OpenHands sdk#4080 | At risk | Replay errors on an unknown kind or version (`state.go:88,537`), so `Replay`, `Project` and `StructuredOutput` all fail. The design promises inspection (`turn.md`) | Add an inspect-only replay that tolerates unknown kinds and blocks only execution |
| 14 | Heavy or vendor dependencies in the core module | ADK #743; Eino kin-openapi; OpenHands LiteLLM | Avoided | `providers/google/gemini` is its own module, so `google.golang.org/genai` and gRPC are absent from the root `go.mod` | Keep vendor-SDK providers in their own modules |
| 15 | Observers or ambient callbacks controlling execution; shared mutable config | Eino #517, #1177; ADK #498 | Avoided | Observer is lossy, recovers panics, and returns nothing (`observer.go`); no `ctx` values; values are cloned; `Engine` is stateless | Keep |

Also at risk:
- The reminder note and the last-call note are appended to the system prompt
  partway through a turn (`engine.go:787-801`), which breaks the prefix cache
  (pi #6621, OpenHands sdk#3690).
- A 429 simply fails the turn (idea 9).

## Top recommendations

**1. Dashi should own tool-call identity.** When a model result settles, give
each client call a turn-scoped effect ID (for example `<attempt>:call:<n>`).
Keep the provider ID as a wire attribute that preparation maps back. That
resolves the repeated or empty ID wedge (pitfall 4), gives tools a globally
unique idempotency key, and matches Nvoken's lesson and pi's "reserved result ID
is the invocation identity". Pair it with the completions assembly fixes
(pitfall 2), because both come from trusting provider IDs.

**2. Tool failures should be model-visible by default and panics contained.**
From the developer's side, returning `err` from a `tool.Func` callback is the
natural thing to do. Today that halts the turn for human reconciliation. The
`uncertain` concept is right for effects that may have happened, but it should
be something a tool opts into. Two directions:
- A plain `error` becomes `StateFailed` with an opaque model message, and the
  full error goes to the observer and the record.
- A typed `tool.ErrUncertain` sentinel, or `KindExternal`, means "may have
  happened".

Recover panics once at the dispatch goroutine. Add MAF's consecutive-failure
budget as a limit.

**3. Separate the three timeouts, and let the engine return retry waits.**
- Replace the 5-minute total HTTP timeout with an idle-stream watchdog.
- Apply `Limits.Deadline` to the model-call and tool contexts.
- Add a typed retry decision for known-rejected attempts (429/503 with
  `RetryAfter`). It should return a durable `Waiting{Retry, NotBefore}` outcome,
  not sleep, which suits Cloud Run hosts.

This covers the pi hang cluster and the ADK "retry is undiscoverable" cluster
without breaking the rule that `Client` never retries.

**4. Fix record growth before building sessions.** Pin prior history by
reference plus digest, not by copying it into each start fact. Cache the
projection incrementally, as principle 5 already asks. Cap tool-result size
through a recorded projection adjustment. Otherwise the session layer will
inherit quadratic storage and per-step rebuilds, which are ADK's and OpenHands'
scaling bugs.

**5. Ship a `dashitest` package and a dialect corpus.**
- Export the scripted client and a table of recorder conformance cases.
- Add recorded completions fixtures: missing index, reused IDs, empty arguments,
  `reasoning_content` echo, metadata-only chunks.
- Make unknown-key tolerance the default for `tool.Func`.

Every tracker ranks provider dialects first, and pi's roughly 160 regression
files show that the corpus is the product.

## Notes on design-pitfalls.md

1. **The tracker's top theme is missing.** Provider-dialect normalization,
   provider IDs, and replay fidelity lead all five trackers. The doc only
   touches metadata-only chunks under #8. Add a pitfall along the lines of
   "trusting provider wire data (IDs, indices, empty fields) as record
   identity". The code shows the gap (pitfalls 2 and 4).
2. **#5 guards one direction only.** It warns against labeling uncertainty as
   known failure. It does not warn against the opposite: escalating routine,
   known failures into human reconciliation. The executor-error rule does
   exactly that, and it turns Eino's #1 tool complaint into a stuck turn.
3. **#11 disagrees with the code.** It says unknown data "should remain
   inspectable". `replay` rejects unknown kinds and versions outright, so no
   inspection API works on such a turn.
4. **#10 omits process-level safety.** It covers resource ownership but not a
   panicking tool killing the host. It also omits timeouts: principle 9 names
   idle-stream timeouts, but the pitfalls list has no hang item, and pi's
   largest runtime cluster (240 issues) is hangs.
5. **#10 needs a caveat.** It says "slow display consumers must not hold durable
   execution hostage", while `Engine.Stream` is synchronously backpressured by
   design (`design/iterator-streaming.md`). Both are defensible. The pitfall
   should say the consumer of `Stream` must be the owning caller, and that hosts
   fan out to UIs from the record, not from the iterator.
6. **Ergonomic and process pitfalls are missing.** Unbounded tool results,
   strict argument rejection, dependency weight in the core module,
   system-prompt volatility breaking the cache, and docs or examples drifting
   from releases (Eino #614, ADK #443) all recur in the trackers and have no
   entry.
7. **#12 is well placed but not yet applied to Dashi itself.** Dashi's own
   guarantees are proven by local fixtures and three live routes. Add a line
   that each guarantee should name the test that pins it (MAF go#492: "tests as
   the design spec").
