# A. Production application workarounds

Source review of Dashi main at 9d28264 on 2026-09-27; findings are directions,
not decisions.

**Names:** written before [decision 30](../decisions.md); read `Definition` as
`Agent`, `Engine`/`Bindings` as `Runner`, `Accept` as `Settle`, and a command's
`ID` as `CommandID`.

Dashi (main, 2026-09-27) already removes the central reconstruction burden that
both apps carry. The Turn is now an authoritative sequence of acknowledged
steps. The app's `Recorder` commits each fact before the next effect. Model
intents, tool starts, uncertainty, suspension, and late results are typed facts,
not callbacks and sentinel errors. Of 20 workarounds, **9 are Covered, 9
Partial, 2 Missing**. The biggest holes are on the edges of the turn, not in its
core. Context cancellation is terminal, so there is no resumable "yield" for
worker drain, lease handoff, or time budgets. Typed provider failure evidence is
flattened to strings at the Outcome boundary. Budget and deadline control happen
only between model calls. Tools get no execution identity. Dashi supplies no
read-side projections: generated transcript, public view, preview fold, or
attachment references. Everything below comes from code and tests. The design
docs were used only to name intent.

Apps: mobius-cloud @ `5645a5b5a` (M), nvoken-cloud @ `deea133a` (N). Dashi
paths are relative to the repo root.

| # | Workaround (what the app had to build) | Evidence (app path:line) | Dashi status | Dashi evidence (path:line) | Gap / direction |
|---|---|---|---|---|---|
| 1 | Stop the turn when tool persistence/infra fails, instead of feeding the error to the model (ORCH-03) | N `divegen/mcp_tool.go` Call path; M `executors/agent_tools.go:492` (`NewToolResultError(err)`) | Covered | `tool/tool.go:312` (error = uncertain); `tools_loop.go:24-34`; `state.go:406-410` → `StopUncertain` `tools_loop.go:248`; `engine_test.go:620` | Executors must now return `StateFailed` for model-facing errors. Document this migration clearly. |
| 2 | A checkpoint transaction that advances only after the app writes (message, intents, usage) | N `services/toolcalls.go:54`, `adapters/postgres/toolcalls.go:305`; M `executors/agent.go:4321-4336` (`HookAbortError` wrap), `agent.go:2697` | Covered | `recorder.go:19-33` (`Recorder`, `CommitError`); `engine.go:303-344`; `engine_test.go:715` | There is one `Commit` per step (about 6 per iteration with one tool) and no grouping. `Load(string)` is optional and takes no ctx (`engine.go:101`). |
| 3 | Lease/attempt fencing on every write; M's writes are uneven (status predicates) | N `domain/execution.go:26`, `engine/claim.go:36`; M `adapters/postgres/agent_turn_repo.go:583`, `internal/durable/lease.go` | Partial | `CommitRequest.Owner`/`ExpectedSeq` `recorder.go:11-15`; `recorder_test.go:12` | The fence is an optional opaque string, checked only by the recorder. Tools and policy never see it. Lease loss surfaces only as a rejected commit. |
| 4 | Admission and credit reservation before each paid call; per-call credential resolution | N `divegen/generator.go:2098-2193` (`resolvingModel`), `services/credit_authority.go:42` | Covered (admission) / Partial (credentials) | `Policy.BeforeSend` `policy.go:20-26,51`; admission is written into the intent at `engine.go:554-567`; `host_recovery_test.go:14-84` | `Bindings.Client` is fixed per Apply (`engine.go:18-27`). Revoking a credential mid-turn needs a `Client` wrapper or a BeforeSend deny. |
| 5 | A model-call fact for every attempt, including empty, failed, and uncertain ones | N `generator.go:2455` (`modelEvidence`), `credit_authority.go:274` (`resolveOpenModelCallFacts`) | Covered | `ModelFact` `state.go:197-219`; intent before Send `engine.go:567`; settlement `engine.go:588-611`; `Accept{ModelResult/ModelFailure}` `engine.go:205-232`; `model_failure_test.go:11-48` | No first-output timestamp. Attempt ordinal is the only retry index. |
| 6 | Usage reconstruction from `GenerationError`; invented billing keys `final`/`error:N`/`suspend:<id>` | M `executors/agent_model.go:196-200,336` | Covered | Stable `AttemptID` `engine.go:564`; `Usage` nil = unknown `llm/llm.go:466-468`; partial result kept on error `engine.go:606`; `TurnView.Usage` `state.go:115-122` | The attempt ID is `turnID:model:N`, a natural idempotency key for billing. |
| 7 | Mid-stream cost meter and time budget that can stop a running call | M `executors/agent_budget_meter.go:35-79`, `agent_resume.go:405`; N `generator.go:2364` (`budgetStreamIterator`), `:1600` | Partial | `Limits.Deadline` is checked only before a call (`engine.go:459`); `DeltaUsage` goes to the observer only (`engine.go:581`) | Cutting off a running call means cancelling ctx, and that gives a terminal `StopCanceled` (see #20). |
| 8 | Typed external stops (budget hold, interrupt) instead of sentinel errors | N `generator.go:1454-1456` (`errCheckpointBudget/Interrupt`) | Partial | `ModelDecision.Stop` `policy.go:15-19` → `engine.go:516`; a BeforeSend deny gives `StopRefused` (`engine.go:559`) | The reason is free text with no details. A policy deny and a model refusal share `StopRefused` (`engine.go:681`). There is no seam between tool calls. |
| 9 | Provider error classification (context overflow, media rejection) and retry control | N `divegen/context_window.go:97`, `media_rejection.go:57`, `generator.go:296,405,466`; M `agent_resume.go:334` | Partial | `llm.Error` Category/Disposition/Code/RetryAfter `llm/llm.go:548-600`; the client never retries (`:522`); a Continue after a known rejection starts a new attempt (`model_failure_test.go:11`) | There is no context-overflow or media category. The typed error is reduced to `ModelFact.Error` text plus an `Evidence` string (`engine.go:590-599`). The Outcome carries only `Stop.Reason` (`state.go:269-277`). |
| 10 | Transcript repair: dangling/duplicate/reordered tool calls, synthetic results, typed JSON rehydration | M `services/session_transcript.go:191,455,676`, `agent_resume.go:1040`; N `generator.go:1367` | Covered (Dashi-recorded turns) | `Project` omits unsettled calls and refuses unresolved ones (`project.go:17-28,78-100`); Cancel settles unstarted calls (`state.go:521-533`); `Prepare` rejects bad pairing (`providers/content_replay_test.go:211`); typed round trip `turn.go:338` | Dashi prevents bad records rather than repairing them. Legacy or app-supplied `Start.History` is rejected, not repaired. |
| 11 | Separating input from generated output; resume-tail backward scan | M `agent_resume.go:1085` (`turnGeneratedMessages`) | Partial | `StartFact` separates History/Input (`state.go:172-178`); output lives in `model_settled` steps; `Turn.AnswerText` `turn.go:99` | There is no exported "generated messages" or display projection, so every app will still walk `Steps` itself. |
| 12 | Suspension snapshot, durable wait, resume on another owner | M `agent.go:4321`, `agent_resume.go:545`, `daemon/session_runtime.go:563`; N `services/generation_recovery.go:32` | Covered | The Turn is the snapshot. `StepToolWaiting` (`tools_loop.go:31,180-185`); `Accept` (`engine.go:260-276`); committed final answer settles without a resend (`engine_test.go:326`); takeover (`host_recovery_test.go:84`) | The app can create its wait row in the same `Commit` as `tool_waiting`. `tool.Wait` is generic. |
| 13 | Provider-private replay artifacts; stripping reasoning across model lineage | N `services/provider_artifacts.go:102,181`; M `session_transcript.go:615` | Partial | `Content.Replay`/`Opaque` with Origin (`llm/llm.go:130-135`); `ContentPolicy` (`llm/content_policy.go`); `ContentChanges` in `ModelFact` (`state.go:209`); `content_replay_test.go:96` | There is no public/private split helper for API or display redaction. The only codec version is `Step.Version=1`. |
| 14 | Stripping attachment bytes from snapshots and restoring them by reference with an access recheck | M `agent_resume.go:960,994` | Missing | `content.Source` covers Data/URL/provider ID/URI only (`content/content.go:37-62`) | There is no app-owned reference or resolver at `Prepare`. Bytes sit in `turn_started`/`tool_settled` steps. Pinned `Start.History` copies them into every later turn. |
| 15 | Preview identity, per-call tool-argument fragments, live sequence numbers | N `generator.go:1458,2672` (`normalizedDelta`); M `executors/agent.go:500,535` | Partial | `Event.AttemptID/MessageID`, reused by `model_settled` (`policy.go:73-82`, `engine.go:581`); `Step.Seq`; backpressured `Engine.Stream` (`engine.go:35`) | Deltas carry no ordinal or offset. Argument fragments carry only `Index` (`providers/openai/responses/stream.go:144`). `Observer` drops events (`observer.go:23-26`). No reference fold. |
| 16 | Forcing non-streaming for provider-tool turns to keep citations and hosted blocks | N `generator.go:1448` (`nonStreamingModel`) | Covered (tested providers) | The `Result` is identical whether streamed or not (`llm/llm.go:452`); `providers/agent_parity_test.go:20`; citations delta at `providers/anthropic/messages/stream.go:174` | Hosted-tool streaming is not live-qualified (`docs/agent-loop-review.md`). |
| 17 | Idempotency keys for external jobs derived from run and tool-call identity | M `executors/job_action.go:259` | Partial | `tool_started` is committed before `Execute` (`tools_loop.go:216`) | `Execute(ctx, Call)` gets no turn ID, step seq, or owner (`tools_loop.go:230`), and `Call.ID` is provider-assigned. |
| 18 | Uncertain-effect policy: retry if safe, else tell the model | N `divegen/mcp_tool.go:240` (`safeMCPRetry`), `services/toolcalls.go:545` | Partial | `StopUncertain` plus host `Accept`; tri-state annotations (`tool/tool.go:32-43`) | There is no reconcile or retry-if-safe hook. The host must write an outcome for every uncertain call. |
| 19 | Mid-turn nudges, Stop-hook continuation, snapshot vs cumulative context | N hooks (DIVE-08/26); M M-01 findings | Covered | `ContextItem.Snapshot/NextCallOnly` (`state.go:69-75`, `project.go:37-72`); `ModelDecision.Deliver`; `StopDecision` (`engine.go:688-703`); `reminder_test.go:49`, `engine_test.go:669` | Delivery during a running `Apply` must be pulled by `BeforeModel`. |
| 20 | Worker drain, lease loss, and heartbeat cancellation that leave the turn resumable | M `internal/durable/heartbeat.go`, lease recovery; N claim heartbeat cancel (recs1 §ORCH-03) | Missing | ctx cancel commits `StopCanceled` even after cancel (`engine.go:315-320,422-424`; `tools_loop.go:47-48`); `canContinue` rejects it (`state.go:604`); `engine_stream_test.go:98-117` | Graceful shutdown or a deadline makes the turn permanently canceled. There is no soft stop or yield. |

## Top gaps

**1. Cancellation is terminal (#20, #7).** Dashi treats `ctx` cancellation as a
user Cancel and commits `StopCanceled`, which blocks `Continue`. A durable host
cancels ctx for many non-terminal reasons: graceful shutdown, heartbeat failure,
per-turn time budgets, and admin interrupts. With a live lease, all of these
would permanently end the turn. Under a lost lease the fence rejects the write,
but by luck rather than by contract. *Direction:* reserve the terminal outcome
for the explicit `Cancel` command. Make ctx cancellation produce a resumable
stop, for example `StopInterrupted` with a caller cause from `context.Cause`.
Started effects stay pending for reconciliation.

**2. Typed failure evidence is lost at the Outcome (#9).** `llm.Error` has the
right shape. But the engine stores `sendErr.Error()` and a formatted evidence
string, and `Outcome` exposes only `Stop.Reason`. An integrator still cannot
branch on Category, Retryable, RetryAfter, or Code without parsing strings. That
is the nvoken workaround Dashi meant to delete. Context-overflow and
media-rejection categories, which drove about 430 lines in nvoken, are also
absent. *Direction:* record a structured, bounded failure (category, code,
status, retry-after, disposition) in `ModelFact` and surface it on `Outcome`.
Add normalized categories for context overflow and rejected media, with counts
when the provider supplies them.

**3. Budget and time control only at call boundaries (#7, #8).** `BeforeSend`
admission plus the atomic `Commit` is a clean replacement for reservation
wrappers. Enforcement *during* a streaming call and *between tool executions*
still needs ctx tricks, and those hit gap 1. Policy stops are free-text, and a
spending deny is indistinguishable in kind from a model refusal. *Direction:*
consider a typed stop detail, for example `Stop.Cause` with a code and JSON, and
a distinct policy-refusal kind. Consider a bounded in-call check, such as a
policy callback on usage deltas that can end the call with a recorded, resumable
limit.

**4. Tools lack execution identity and an uncertainty policy (#17, #18, #3).**
Executors receive only `(ctx, Call)`. Mobius-style idempotency keys need turn ID
plus call ID, and a fenced tool write needs the owner/attempt. Today the app
must close over these per `Apply`. Every uncertain call also stops the turn for
manual `Accept`, so nvoken's "retry if read-only or idempotent" has nowhere to
live. *Direction:* put a small execution identity (turn ID, call ID, step seq,
owner) into the tool context. Offer an optional reconcile decision on `Policy`
(stop, retry-if-safe, report-to-model). The report should be projected rather
than stored as a message.

**5. No read-side projections or content references (#11, #13, #14, #15).** Both
apps built preview accumulators, generated-tail extraction, public/private
splits, and attachment dehydration. Dashi records the facts to do these
correctly, but offers no helpers, so each app will rewrite a fold over `Steps`.
Pinning full `Start.History` per turn also copies prior attachments and history
into every turn record. *Direction:* consider exported projections (generated
messages, public view with replay stripped) and a reference fold over
`Step`+`Delta` with per-attempt ordinals. Consider an app-owned content
reference resolved at `Prepare` under a resolver the host supplies. It could
keep authorization and storage in the app.

## Already strong

- **Acknowledged, fenced step journal.** `Recorder.Commit` with `ExpectedSeq`,
  `Owner`, and a three-way `Persistence` result directly replaces nvoken's
  checkpoint reconstruction and Mobius's `HookAbortError` wrapping. The test
  fixture shows budget reservation, lost acks, and takeover in one atomic host
  write (`host_recovery_test.go`).
- **Intent before effect for models and tools.** `model_requested` and
  `tool_started` precede dispatch. Missing results become explicit uncertainty,
  never a resend. This is nvoken's `model_call_facts` and pending/running
  discipline, moved into the library.
- **Tool infra errors cannot leak to the model.** The `Executor` contract makes
  a Go error mean "uncertain" and stop, which fixes ORCH-03 by construction.
- **Record vs projection.** Synthetic results and dropped partial calls are
  projections of facts (`Project`), so the transcript-repair class of bugs
  cannot arise for Dashi-recorded turns.
- **Suspension without snapshots.** Waiting, approval, and external tools are
  steps, and resume is `Accept` on any worker. Mobius's separate
  suspension/partial-resume checkpoint paths are unnecessary.

## Open questions

- How fast is per-step commit (about 6 transactions per tool iteration) against
  Postgres/Neon? Would apps want an opt-in commit group, such as `model_settled`
  plus tool decisions?
- Should `Load` become part of `Recorder` with a `context.Context`? Today it is
  discovered by type assertion with the signature `Load(string)`.
- How do multi-turn sessions avoid O(n²) history storage when every `Start` pins
  a full copy of the prior history? The code has no session-level API.
- Child turns (Mobius `executors/agent_subagents.go:284`) and compaction are out
  of Dashi's scope. How will they share usage and fencing with a parent turn?
- Is the engine-dispatch choice (vs host-dispatched effects) viable under
  nvoken's real claim executor? `docs/agent-loop-review.md` says this is
  unproven. No durable-host test exists in the repo.
- Env-var credential fallbacks remain in some providers (`gemini.go:87-91`,
  `openrouter.go:42`). The default HTTP client has a 5-minute total timeout
  (`internal/providerhttp/http.go:123`). Is either acceptable for multi-tenant,
  long-stream hosts?
