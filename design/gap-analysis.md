# Gap analysis against the Dive v2 goals

On 2026-09-27, five reviews compared Dashi `main` at 9d28264 with our
production apps' workarounds, Dive, adjacent frameworks and their trackers,
agent protocol specs, our streaming protocol, and our cost and budget needs.
The recorded Turn (acknowledged steps, intent before effect, explicit
uncertainty, bound approvals, idempotent commands) is the strongest part and
already ahead of Dive and the surveyed projects. The gaps sit around it. Gaps
are directions, not decisions.

**Names:** written before [decision 30](decisions.md); read `Definition` as
`Agent`, `Engine`/`Bindings` as `Runner`, `Accept` as `Settle`, and a command's
`ID` as `CommandID`.

## Summary table

| # | Theme | Severity | Goal(s) served | Status in one line |
|---|---|---|---|---|
| 1 | Turn lifecycle: cancel, pause, reconnect | High | Production workarounds, live updates | Any context cancel is a terminal stop; no pause, cursor, or final flag |
| 2 | Reasons and errors are strings | High | Production workarounds, DX, vocabulary | Typed `llm.Error` is flattened to text; `StopRefused` means three things |
| 3 | Tool execution semantics | High | DX, adjacent-library pitfalls | A plain tool error halts the turn; panics and reused IDs are unhandled |
| 4 | No simple entry point | High | DX, vocabulary | A tool turn needs 4 IDs, 5 structs, and a private digest to approve |
| 5 | Budgets and cost | High | Cost and budgeting | Tokens are summed; cost, budgets, and caching are absent |
| 6 | Live update fidelity | Medium | Live updates, protocols | Observer drops committed steps; previews lack identity |
| 7 | Provider parity with Dive | Medium | Cross-provider support | Ahead on errors and hosted tools; behind on caching, catalog, retry |
| 8 | Storage growth and testability | Medium | Small foundation, adjacent-library pitfalls | History copied per turn; unknown kinds unload a turn; no test kit |

## Themes

### 1. Turn lifecycle: cancel, pause, and reconnect [A, D]

A host cannot drain a worker without ending the turn; a UI cannot reconnect.

- Context cancellation records `StopCanceled` (`engine.go:422-423`,
  `tools_loop.go:47-48`), which `canContinue` rejects (`state.go:604`).
- `Engine.Stream` is single-reader and single-use (`engine.go:35-40`); breaking
  it cancels the turn (`engine.go:30-34`, `engine_stream_test.go:98`).
- No follow-by-cursor (`Recorder` has only `Commit`, `recorder.go:19-21`).
  `Stop` has no final flag or phase (`turn.go:256-259`); finality hides in
  `canContinue` (`state.go:603-608`), and a canceled turn still accepts late
  results (`state.go:129-135`).

Direction: resumable context cancellation, terminal only on `Cancel`; `Seq` as a
documented cursor with a read-after-`Seq` method; `Stop.Final` or a phase.

### 2. Reasons and errors are strings [A, B, D, E]

An app cannot branch on rate limit, deadline, or budget without parsing text.

- `Stop.Reason` mixes codes (`engine.go:457`), prose (`tools_loop.go:191`), and
  `err.Error()` (`engine.go:617-619`).
- `ModelFact` keeps only error text and an evidence string (`turn.go:215`,
  `engine.go:591,597`) though `llm.Error` is typed (`llm/llm.go:576-604`).
- `Outcome` has no error field (`turn.go:269-277`); command rejections are
  unexported strings (`engine.go:171,247,250`); policy or budget denial
  (`engine.go:559`) and model refusal (`engine.go:681`) share `StopRefused`.

Direction: typed stop codes with open-enum rules and text as the message; a
bounded structured failure on `ModelFact` and `Outcome`; typed command errors.

### 3. Tool execution semantics [A, B, C]

A routine tool error freezes the turn; probes confirmed bullets 1 and 2 [C].

- Any executor error marks the call uncertain and stops the turn
  (`tool/tool.go:310-315`, `tool/function.go:88-91`, `state.go:406-410`,
  `tools_loop.go:248-249`); Dive fed it to the model
  (`dive/agent.go:3482-3498`). This is Eino's pitfall in a new form.
- No `recover` in the tool goroutine (`tools_loop.go:229-237`). Dashi adopts
  the provider call ID as identity, and replay rejects a repeat
  (`state.go:580-583`), so provider ID reuse across attempts wedges the turn.
- `Execute` gets no turn or lease identity for idempotency keys
  (`tools_loop.go:230`); every uncertain call needs a manual `Accept`, with no
  retry-if-safe policy (annotations are advisory, `tool/tool.go:31-43`).
- No result size limit (`tool/tool.go:198-205`); extra argument keys are
  rejected (`tool/function.go:30,81`), each costing a model attempt.

Direction: known failure by default with an explicit uncertainty opt-in; recover
panics at dispatch; turn-scoped call IDs; an execution identity.

### 4. No simple entry point [A, B]

An app cannot run, approve, or recover a turn without plumbing and private code.

- A tool turn needs four minted IDs, five structs, a hand-written `Toolset`, and
  a policy (`examples/agent/main.go:27-29`, `engine.go:70-82`).
- The args digest is unexported (`turn.go:324-328`) but checked on approval
  (`engine.go:249`); only a test computes it (`engine_test.go:288`).
- A nil `Policy` silently denies tools (`engine.go:83-85,767`) while the turn
  usually reports completed. Strict capability checks reject unknown text
  support (`llm/model.go:120-123,206-208`) with no catalog
  (`docs/guides/model-calls.md:48`), so the first call fails.
- `Load` is outside `Recorder` with no context (`engine.go:101,122`);
  `Outcome.Pending` is an untyped `[]string` (`turn.go:273`); structured output
  takes and returns raw JSON (`turn.go:31,84`) while `tool.Func` is generic.
- Vocabulary drifts: two `StopKind`s, overloaded `Outcome`, a five-payload
  `Accept`, and a glossary at odds with code
  (`design/ubiquitous-language.md:38-40`).

Direction: a thin runner over `Engine` that binds dependencies once and mints
IDs; an approval helper; typed pending items; a `Loader`; a vocabulary pass.

### 5. Budgets and cost [A, E]

An app cannot declare a budget; it supplies estimate, ledger, and stop itself.

- `Limits` has no token or cost budget (`turn.go:35-43`); nothing calls
  `Pricing.Estimate` (`llm/pricing.go:198`); with no catalog, `Cost` is nil.
- `TurnView.Usage` drops nil usage uncounted (`state.go:115-121`). A budget stop
  looks like refusal (`engine.go:559`); an attempt-limited turn cannot continue
  (`state.go:607-608`), so no limit can be raised.
- No prompt caching controls or 5m/1h cache-write split; no retry policy that
  counts attempts and passes `BeforeSend` (`llm/llm.go:522-525`).

Direction: declared token and cost limits, a max-cost-at-risk estimator, turn
cost with a completeness count, typed limit reasons, and a recorded raise.

### 6. Live update fidelity [D]

A UI host must rebuild gap handling, phase folds, and preview bookkeeping.

- The observer drops any event, committed steps included, when its 64-slot
  buffer fills (`observer.go:19-26`), with no gap signal, contradicting
  `design/agent-loop.md:99-100`.
- Deltas lack offsets and emission time (`llm/llm.go:493-514`); only Anthropic
  emits the tool-call ID (`providers/anthropic/messages/stream.go:151`).
- `Event` has no worker identity (`policy.go:73-82`). Approval and wait steps
  carry only a reason or prompt (`policy.go:35-40`, `tool/tool.go:245-251`): no
  title, options, or deadline.
- The host must redact reasoning; executors lack a progress channel.

Direction: lossless committed steps, lossy previews with a gap signal,
normalized block starts with offsets, and a small `WaitInfo`.

### 7. Provider parity with Dive [E, C]

Dashi leads on hosted tools, tool choice, typed errors, frozen bodies, and
replay; an app still loses these Dive basics:

- No prompt cache controls in `providers/`, no native structured output (only
  `dashi_submit_output`, `output_contract.go:25`), no catalog or prices.
- No automatic retry, service tier, stop sequences, top_p, or seed
  (`llm/llm.go:327-345`); no typed escape hatch for provider controls.
- Reasoning controls are not normalized; MCP has no credentials
  (`providers/anthropic/messages/hosted_tools.go:161-168`); no computer use.
- The completions reader keys tool calls by index only
  (`providers/openai/completions/stream.go:118-133`); empty arguments fail
  `Call.Validate` (`tool/tool.go:178`); one 5-minute total HTTP timeout and no
  idle timeout (`internal/providerhttp/http.go:123`).

Direction: portable common controls plus typed per-route options recorded by
`Prepare`; a dated catalog; ID-then-index assembly; separate timeouts.

### 8. Storage growth and testability [A, B, C]

An app cannot keep long sessions cheap or test its agent with public helpers.

- Each `Start` copies prior history, attachment bytes included
  (`engine.go:158`), so storage grows quadratically; `content.Source` has no
  app-owned reference (`content/content.go:37-62`).
- Unknown step kinds or versions make a turn unloadable (`state.go:88,537`),
  contrary to design-pitfalls #11. Input is fully validated only at replay
  (`engine.go:143,162`; `state.go:708-720`), so a non-replaying recorder can
  persist an unreplayable step [B].
- Projection rebuilds from step 1 every call (`project.go:13-14`). No public
  test kit: the scripted client is test-only (`engine_test.go:15`); no recorder
  conformance cases or stream fixtures.
- `go.mod` pulls `genai` and gRPC for every user (`go.mod:8,26`).

Direction: history by reference, content references, validation before commit,
inspect-only replay, and a `dashitest` package.

## Decisions needed

1. **What a tool's Go error means.** Uncertainty fits infrastructure faults, not
   routine ones. Keep it, make it model-facing, or split via a sentinel.
2. **Tool panics.** Recovery protects the host but can mask bugs. Recover and
   record uncertain or failed, or require hosts to recover.
3. **Backpressured `Stream` or lossy `Observer`.** One stalls; one loses facts.
   Make committed delivery lossless, or have UIs read the record by cursor.
4. **Strict capabilities without a catalog.** Strictness fails the first
   program. Ship a catalog, allow recorded unverified use, or name the fix.
5. **History copy versus references.** Copying prevents substitution at
   quadratic cost. Keep copying, or pin by reference plus digest.
6. **Cancellation.** Terminal stop or resumable pause: a separate interrupted
   stop from `context.Cause`, or terminal only for an explicit `Cancel`.
7. **Where budgets live.** On `Definition`, on `Start`, or both, with the turn
   only narrowing the definition and raises recorded as commands.

## Already strong

- **Fenced, acknowledged journal** with intent before effect: uncertainty is
  recorded, never a blind resend, and no transcript repair is needed [A, C, D].
- **Bound approvals** on arguments and policy revision [A, B, C, D].
- **Idempotent commands** with deterministic attempt and message IDs [A, D, E].
- **One stream path** whose result equals `Send`'s [B, C, D].
- **Typed provider errors, honest cost facts** (nil means unknown), richer
  outcomes than the protocols, and more hosted tools than Dive [B, C, D, E].

## Sources

Full reports: [A](gaps/A-production-workarounds.md) production workarounds,
[B](gaps/B-developer-experience.md) developer experience,
[C](gaps/C-adjacent-libraries.md) adjacent libraries,
[D](gaps/D-protocols-streaming.md) protocols and streaming,
[E](gaps/E-providers-cost-budgets.md) providers, cost, and budgets.
