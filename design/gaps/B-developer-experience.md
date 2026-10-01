# B. Developer experience, types, and modern Go

Source review of Dashi main at 9d28264 on 2026-09-27; findings are directions,
not decisions.

**Names:** written before [decision 30](../decisions.md); read `Definition` as
`Agent`, `Engine`/`Bindings` as `Runner`, `Accept` as `Resolve`, and a command's
`ID` as `CommandID`.

Dashi's model-call layer (`llm`, `tool`, `content`) is already a clear DX
improvement over Dive (see "Already strong"). The agent layer (the root `dashi`
package) is where the stated goals are not met yet. It exposes the engine's
recovery protocol directly as the application API. Approval and crash recovery
require reimplementing unexported logic and classifying opaque pending IDs,
failure causes become strings, and several core nouns mean different things in
different packages. The conceptual proposals said "the facade must make the
one-line case one line"
(`tmp/research/design/2026-09-26-dive-v2-conceptual-model.md:955-957`,
`…-conceptual-design.md:124-140`). Dashi has no facade yet, and none of its
docs or examples shows a tool, an approval, or a recovery.

## Happy paths

| Path | Dashi (concepts / ~lines / friction) | Dive (concepts / ~lines) |
| --- | --- | --- |
| (a) One model call | Client+Config, `UnknownPolicy`, `Input`, `Message`/`Content`; ~5 lines via `llm.Generate`. `Model` is required, and strict capability checking rejects every model unless you set `UnknownAllow` or supply dated evidence. | `anthropic.New()`, `Generate(ctx, llm.WithMessages(...))`, `AnswerText()`; ~3 lines, default model. |
| (b) Streamed call | Adds `Prepare`, `Input.Stream: true`, and `llm.Stream`; ~12 lines. There is no Input-level stream helper, and forgetting `Stream: true` yields a result with no deltas. | Type-assert `StreamingLLM`, then Next/Event/Err/Close plus manual accumulation; ~15 lines. **Dashi is better.** |
| (c) Agent turn with a tool | `tool.Func`, a hand-written `Toolset` type, `Definition` (ID, Revision, Provider, API, Model, Limits), `Turn{ID}`, `Start{ID, Input}`, `Bindings`, `MemoryRecorder`, `AllowLocalPolicy`, then checks on `Persistence` and `Stop`; ~25 lines, **4 IDs, 5 structs, 1 interface**. | `NewAgent(AgentOptions{Model, Tools})` and `CreateResponse(WithInput)`; ~8 lines, 0 IDs, 1 struct. |
| (d) Resume after a crash | Host-specific `Load` (not part of `Recorder`), `Turn.View`, classify each `Pending` string (model attempt, tool call, or hosted request), gather evidence, then `Accept{…ModelFailure/Outcome}` or `Continue`. You must re-supply a byte-identical `Definition`. ~30-40 lines. | Session store, `CreateResponse` with resume or tool results, and `TurnOutcome.Next` says what to do; ~6-10 lines. |
| (e) Approve a tool call | Implement `Policy` (4 methods; embed `AllowLocalPolicy`) and return `DecisionNeedsApproval` with a `Revision`. On `StopWaiting`: `Turn.View`, look up the call, **recompute the unexported args digest**, then `Accept{ID, CallID, Approval: &ApprovalDecision{CallID, Approve, ArgsHash, PolicyRevision}}`. ~25 lines. | `PreToolUse` hook or permission rules plus a `Dialog`, or `NewSuspendResult` then `WithToolResults`; ~10 lines. **Dashi's model is more correct** (binding to args and revision). |

## Findings

**1. The agent API has no facade: every turn is a raw engine protocol call.**
Evidence: `examples/agent/main.go:28-30` (one 300-character line, IDs
`"example"`, `"1"`, `"example-turn"`, `"example-start"`); `engine.go:18-28`
(`Engine struct{}`, so the call reads `(dashi.Engine{}).Apply(...)`);
`engine.go:70-82` (a command ID, a turn ID, a client, and a recorder are all
mandatory); `turn.go:49-51` (a Definition needs ID, Revision, Provider, API, and
Model). `Provider: client.Provider(), API: client.API()` restates data the
client already has (`engine.go:80`). Why it matters: the Turn is supposed to be
the primary noun, but the application has to hold a Turn snapshot, a Definition,
a Recorder, and command IDs, and thread all of them through every call.
Direction: add a thin runner or agent over `Engine`. It would bind definition,
client, tools, recorder, and policy once, mint IDs by default (a command ID
derived from turn ID and sequence), load the turn itself, and offer
`Start/Approve/Resolve/Continue/Cancel` methods. Keep `Engine.Apply` as the
host-level primitive. **High.**

**2. Tool approval cannot be completed with public API.** Evidence:
`engine.go:249` and `state.go:454` compare `digest(v.Call.Args)` against
`ApprovalDecision.ArgsHash`. `digest` is unexported (`turn.go:324-328`) and is
sha256 over `json.Marshal(json.RawMessage)`, which compacts whitespace. So
`sha256(args)` computed by a caller can differ. Only tests compute it
(`engine_test.go:288`). `Accept.CallID` must also repeat `Approval.CallID`
(`engine.go:246`). Direction: expose `TurnView.Tools[id].Approval()`, or a
helper such as `dashi.Approve(view, callID, bool)` that builds the fact.
Consider a separate `Approve` command rather than a fourth `Accept` payload.
**High.**

**3. An error from a tool function halts the turn as uncertain.** Evidence:
`tools_loop.go:24-26` turns an executor error into a nil `Outcome`.
`state.go:406-410` marks the call `ToolResultUnknown`, and `tools_loop.go:247-249`
stops with `StopResultUnknown`. `tool.Func` passes the callback's error straight
through (`tool/function.go:88-91`). Dive turned the same `return nil, err` into
an `IsError` result the model sees (`dive/agent.go:3482-3498`). A Dashi live
test also returns `errors.New` for bad arguments (`agent_live_test.go:221-223`).
Why it matters: this is the most common Go habit and the most likely migration
bug. A routine "not found" freezes the turn until someone reconciles it by hand.
Direction: keep the uncertainty semantics for `Executor`. Give `tool.Func` a
known-failure path, such as `tool.Failed(err)`, `tool.Succeeded(text)`, or an
error type the adapter maps to `StateFailed`. Document the difference loudly in
the `Func` godoc. **High.**

**4. Failure causes become strings, and `Stop.Reason` mixes tokens with prose.**
Evidence: `Stop{Kind, Reason string}` (`turn.go:256-259`). Machine tokens
(`"model_calls"`, `"deadline"`, `"output"`, `"context"`, `"provider_pauses"`,
`"empty_response"`; `engine.go:408,457-460,655-667`) share the field with prose
(`"tool binding unavailable"`, `tools_loop.go:191`) and raw `err.Error()`
(`engine.go:617-619`). A provider `*llm.Error` (Category, RetryAfter,
Disposition; `llm/llm.go:576-604`) reaches the caller only as text
(`ModelFact.Error string`, `turn.go:215`; `engine.go:591`). `Outcome` has no
`Err`. Command rejections in `Apply` are unexported `errors.New` strings, for
example `"stale tool approval"` and `"turn cannot continue"`
(`engine.go:171,247,250`). Only five sentinels exist (`turn.go:330-336`). Design
principle 9 asks for "a limit as a typed outcome"
(`design/design-principles.md`), and the architecture review asks to "preserve a
Go error cause" (`…-architecture-review.md:302-303`). Direction: add a typed
`StopReason` enum, an `Outcome.Err` that wraps the live cause (not recorded),
and sentinels or a `*CommandError` with a code for command rejections.
`errors.AsType` from Go 1.26 then makes inspection a one-liner. **High.**

**5. The default policy silently denies tools.** Evidence: `engine.go:83-85` and
`engine.go:764-768`. With a nil `Policy`, every call becomes `not_executed` with
the text "no tool policy configured", fed back to the model. The turn usually
ends `StopCompleted` with an apologetic answer. Why it matters: the developer
sees success and a wrong answer, not an error. Direction: reject at `Apply` when
tools are bound and `Policy` is nil, or stop the turn with `StopRefused` and a
typed reason. At minimum, show `AllowLocalPolicy` in the README. **High.**

**6. Strict capability validation fails every call out of the box.** Evidence:
`FeatureText` is always requested (`llm/model.go:206-208`), and unknown support
is rejected under the default `UnknownReject` (`llm/model.go:118-123`,
`providers/openai/responses/responses.go:49-51`). No catalog ships
(`docs/guides/model-calls.md`, "No built-in model catalog"). Both examples opt
out. Direction: ship a small dated catalog, or make the error message name the
fix (`UnknownAllow` or `Config.Models`). **Medium.**

**7. Pending work is an untyped `[]string`.** Evidence: `Outcome.Pending
[]string` (`turn.go:273`) mixes model attempt IDs, tool call IDs, and hosted
request IDs. Hosted IDs come from map iteration, so their order is
nondeterministic (`state.go:541-558`). The model attempt ID is not otherwise
exposed on `TurnView` (`state.go:32-44`). `Accept.CallID` carries all three
kinds (`engine.go:206,234,246`). `TurnView.Stop` can be empty while
`Outcome.Stop` for the same record says `uncertain` or `waiting`
(`engine.go:381-390`). Dive gave a `Next` hint (`dive/outcome.go:178-184`).
Direction: add `Pending []PendingItem{Kind, ID, Tool, Wait}` and a derived
`Next()` (reconcile, approve, supply result, continue, done). Derive
`TurnView.Stop` the same way `Outcome` does. **Medium.**

**8. Recovery depends on an undeclared `Load`, and the Definition is
re-supplied.** Evidence: `Recorder` has only `Commit` (`recorder.go:19-21`).
`Apply` duck-types `interface{ Load(string) (Turn, error) }` with no `context`
(`engine.go:101,122`). The docs tell callers to "reload the turn by ID" after
`break` or an unknown write (`docs/agent-turns.md:140-141`). Every command also
needs the Definition, which must match the recorded one exactly
(`engine.go:90-91`). The recorded copy is available through
`turn.View().Definition`. `Limits.Deadline` is an absolute time inside the
"reusable" Definition (`turn.go:42`), so a per-turn deadline changes the digest.
Direction: add a `Loader` interface (`Load(ctx, turnID)`), let the engine or
runner default to the recorded Definition, and move `Deadline` to
`Start`/`Bindings`. **Medium.**

**9. Invalid input is detected only after it is committed.** Evidence: the
`AddMessages` and `Start` branches check only emptiness (`engine.go:143,162`) before
`commit`. `ContextItem` and message validation run only inside `replay`
(`state.go:207,229,708-720`). `MemoryRecorder` happens to replay before
accepting (`recorder.go:107`), but the Recorder contract does not require it
(`docs/agent-turns.md:89-99`). Why it matters: a host recorder that simply
appends persists a step that makes the turn permanently unreplayable. For
example, `Origin: "Operator"`. Direction: validate step payloads in
`runtime.commit` before calling the recorder, and return a Go error from
`Apply`. **Medium.**

**10. Iterators cover the streams but not the record.** Evidence: `llm.Stream`
and `Engine.Stream` (`llm/stream.go:27`, `engine.go:35`) are good. Everything
else hands back slices and asks the caller to switch on `Kind`: `Turn.Steps`,
`Message.Content`, and tool calls (no `Result.ToolCalls()`; Dive had one at
`dive/llm/response.go:96`). `Turn.AnswerText` reimplements `Message.Text`
(`turn.go:97-114` vs `llm/llm.go:295-303`). Streaming has three mechanisms:
`llm.Sink`, `Observer`, and `Engine.Stream`. `Observer` runs on a hidden
goroutine that swallows panics and can fire after `Apply` returns
(`observer.go:7-18,28-31`). `llm.Stream` pairs the terminal `err` with a partial
`Result` in the same yield (`llm/stream.go:64`), which is unusual. Direction:
add `Turn.Models()`, `Turn.ToolCalls()`, `Message.ToolCalls()`, and
`Turn.Pending()` as `iter.Seq`. Consider retiring `Observer` in favor of
`Engine.Stream`, or document that it may fire after return. **Medium.**

**11. Structured output and typed tools use different generics stories.**
Evidence: `tool.Func[T]` generates a schema from `T` (`tool/function.go:22-47`).
`Definition.OutputSchema` and `Start.OutputSchema` are hand-written
`json.RawMessage` (`turn.go:31,287`), and the value comes back as
`json.RawMessage` through a triple return `(StructuredOutput, bool, error)`
(`turn.go:84`). Dive had `Message.DecodeInto` (`dive/llm/message.go:164`).
Direction: add `dashi.OutputSchemaFor[T]()` and `dashi.OutputAs[T](Outcome)`.
**Medium.**

**12. Ergonomic gaps for everyday values.** Evidence: `tool.Func` callbacks must
build `o := tool.TextOutput(..); return tool.Outcome{State: tool.StateSucceeded,
Output: &o}` (`agent_live_test.go:225-226`). There is no `tool.Succeeded`
helper. The callback must accept an unused `tool.Call`. No exported `Toolset`
adapter exists; tests define `fixedTools`/`liveToolset` (`engine_test.go:53`,
`agent_live_test.go:421-423`). There are no `SystemMessage` or `UserText`
helpers. The internal `ptr[T]` helper (`engine.go:373`) could be replaced by Go
1.26 `new(expr)`, which the docs could also recommend for `Temperature:
new(0.7)`. `omitempty` is used on struct and `time.Time` fields where it has no
effect (`llm/llm.go:368`, `tool/tool.go:71`, `turn.go:42`); use `omitzero`.
**Low.**

**13. Discoverability stops at the model call.** Evidence: the README shows only
a model call. `docs/guides/running-turns.md` has no code. There are no `Example`
functions anywhere. Roughly 40 exported root identifiers lack godoc, despite
`docs/dev/go-style.md`: `Turn`, `Outcome`, `Engine`, `Bindings`, `Start`,
`Accept`, `Stop`, `Limits`, `DefaultLimits`, `Policy` types, and
`MemoryRecorder`. `Event` and `Observer` live in `policy.go`. `DefaultLimits`
reserves a final answer that ends `StopLimited` "even when it contains useful
text" (`docs/agent-turns.md:52-60`), which is surprising for callers who check
`StopCompleted`. Direction: add runnable `Example`s for paths (c)-(e) and a
"check Stop" snippet, and document every root type. **Medium.**

**14. The media and provider surface drifts from `llm`.** Evidence:
`media.Error` and `media.ErrorCategory` duplicate the `llm` categories but drop
`Disposition` (`media/error.go:11-42`). Hosted-tool constructors vary in shape:
`NewCodeExecutionTool(provider string)` vs `(options)`, `WebSearchOptions` vs
`WebSearchToolOptions`, some return errors and some do not
(`providers/*/provider_tools.go`). Anthropic's `NewMCPToolset` returns a
`tool.Definition`, not a `tool.Toolset`
(`providers/anthropic/messages/provider_tools.go:171`). There are no video job
polling helpers (an iterator candidate). **Low.**

## Vocabulary drift

| Term | Where used | Conflict | Suggested direction |
| --- | --- | --- | --- |
| Stop / StopKind | `llm.Stop{Kind,Raw}` (finished, refusal, output_limit); `dashi.Stop{Kind,Reason}` (completed, refused, limited) | Two same-named types with parallel but different enums (`StopRefusal` vs `StopRefused`). `StopRefused` also covers a host `BeforeSend` denial (`engine.go:559`). | Rename the llm one to `Finish`/`FinishKind`; split policy refusal from model refusal. |
| Outcome | `dashi.RunResult` (invocation result); `tool.Outcome` (execution state); `Accept.Outcome`, `ToolFact.Outcome` | `Accept{Outcome: …}` means a *tool* outcome passed to a call that returns a *turn* Outcome. | Keep `tool.Outcome`; call the turn one `Result` or `Report`. |
| Result | `llm.Result`, `media.Result`, `Accept.ModelResult`, `llm.ToolResult` (a model content item) | `llm.ToolResult` vs `tool.Outcome`/`tool.Output` are two shapes for the same fact (`project.go:125-132` converts). | One tool-result term per layer. |
| Definition | `dashi.Definition` (agent), `tool.Definition`, `StartFact.Definition`, `ToolFact.Definition` | The glossary says "Agent definition… tools" (`ubiquitous-language.md:10-11`), but the type has no tools, and no `Agent` type exists. | `AgentDefinition`, or an `Agent` facade. |
| Origin | `content.Origin` (endpoint/credential scope struct); `StartOrigin` (user/scheduled/operator, typed); `ContextItem.Origin string` (user/library/tool/operator, untyped, `state.go:709-720`) | Three meanings, two overlapping closed vocabularies, one of them untyped. | `content.Scope` for replay; one typed `Authority` enum for starts and context. |
| Revision | `Definition.Revision` (version string), `TurnView.LastSeq` (int64 sequence), `ToolDecision.RulesVersion` (policy version), `ErrStaleTurn` (sequence) | Three meanings. | `Version`, `Seq`, and `PolicyVersion`. |
| Step / Fact / Event | `Step` holds `*Fact` payloads; `ApprovalDecision` is also a command input; `dashi.Event` vs `llm.StreamEvent` vs `Delta` | "Fact" is used both for records and inputs. The glossary says "Event and observer… callback" only. | Keep Step as the record; make inputs `Approval`, not `ApprovalDecision`. |
| StepKind names vs values | `StepTurnStarted="turn_started"`, `StepModelCallStarted="model_call_started"`, `StepApproval="approval_accepted"` (also used for denials) | Constant names, JSON values, and meaning disagree. | Align names with values; use `approval_recorded`. |
| Input | `llm.Input` (model input), `Start.Input []llm.Message` | The glossary says turn input is "context items" (`ubiquitous-language.md:48-49`). | Update the glossary or accept `[]ContextItem`. |
| Accept / Continue | Code: `Accept` carries 5 payloads (tool, approval, hosted approval, model result, model failure); `Continue{ID}` only | The glossary: Accept is "a tool call's… outcome", Continue is "optionally with more context" (`ubiquitous-language.md:38-40`). | Split `Approve` and `Reconcile`; fix the glossary. |
| Decision | `ToolDecision{Kind: DecisionAllow…}` vs `ModelAuthorization{Allow bool}` vs `StopDecision{Continue bool}` | Three decision shapes on one interface (`policy.go:15-45`). | One `Decision` shape. |
| Toolset | `tool.Toolset` interface; `messages.NewMCPToolset` returns a Definition | The glossary says "named source" (`ubiquitous-language.md:21`); the code has no name. | Rename to `NewMCPConnector`. |
| Clone / Copy | `Usage.Copy()` (`llm/usage.go:165`) vs `Clone()` everywhere else; `Usage.Absorb` vs `Add` | Inconsistent verbs. | `Clone`. |
| Kind | `content.Kind`, `tool.Kind`, `media.Kind`, `llm.ContentKind`, `StepKind`, `StopKind`, `DecisionKind`, `DeltaKind` | Fine by package, but `tool.Kind` means *placement* (local, external, hosted). | `tool.Placement`. |

## Already strong

- **Model call pipeline.** `Input → Prepare → Request → Send → Result`, with a
  recordable `RequestSnapshot`, is clearer and more honest than Dive's
  functional options, and it is documented in `design/terminology-proposal.md`.
- **Streaming.** `llm.Stream` is a single-use, backpressured `iter.Seq2` whose
  terminal `Result` equals `Send`'s (happy path b; `dive/llm/llm.go:17-33` for
  Dive's shape). `Engine.Stream` extends it to turns, with `ErrStreamConsumed`
  guarding against a paid double dispatch.
- **Typed errors with effect knowledge.** `llm.Error` has `Category`,
  `Disposition`, `RetryAfter`, and `Unwrap`, with `ErrorCategory` for decisions
  without parsing.
- **Tagged unions with `Validate` and `Clone`.** `llm.Content`, `tool.Outcome`,
  and `content.Block` all follow this pattern, with the aliasing rules
  documented. `tool.Outcome` separates known failure, nonexecution, waiting, and
  detached. Dive mixed suspend and background work into `ToolResult`.
- **`tool.Func[T]`.** It generates and compiles the schema at construction,
  reports construction errors eagerly (Dive deferred them to call time), and
  never runs on invalid arguments.
- **Durable semantics.** Durable turns, approval binding to arguments and policy
  revision, explicit uncertainty, duplicate-command handling, and `Persistence`
  are all first-class. None of that existed in Dive's `CreateResponse`. The task
  now is to put a small facade over it, not to change its semantics.
