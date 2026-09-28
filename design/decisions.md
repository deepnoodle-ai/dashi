# Decision log

Outcomes of the decisions raised by [gap-analysis.md](gap-analysis.md). A
decision here is a direction the code should follow until a later entry
supersedes it. Items marked "design first" are not decided; each gets its own
design document before any code changes.

## 2026-09-27

**1. A tool's Go error is a model-facing failure; a sentinel stops the turn.**
Decided. A plain error returned by a tool becomes a failed result the model
sees, as Dive did. A typed sentinel marks an error as fatal to the turn, which
stops it in place. Uncertainty remains Dashi's word for a call whose result
was never observed (crash, lost result, panic); it is not something a tool
returns. Rationale: almost any error in a tool call should go back to the model,
and returning `err` is the idiomatic way to say so. Supersedes the "Go execution
errors represent uncertainty" rule for tools.

**2. Tool panics are recovered and recorded as uncertain.** Decided. Recover
once at dispatch, commit the settled step with the panic value as the reason,
and stop the turn as uncertain. Nothing is re-run.

**3. Live updates use three delivery classes and a cursor.** Decided in
direction; proposal in [live-delivery.md](live-delivery.md). Committed steps are
never dropped; previews are lossy and a gap signal tells the consumer to resync;
UIs follow the record by `Seq` through a read-after-cursor path so several
readers can replay independently and a disconnect never cancels work. The
backend shape is open and should be the simplest thing that meets those
properties; a design note precedes the code.

**4. Capability checks warn and adjust by default; they do not fail.** Decided
in direction. When a request asks for something the model cannot take, prefer
the nearest supported value (an effort of `extra-high` becomes `high`) and
record a warning; when support is unknown, send it and record that. Failing the
request is reserved for cases with no safe substitute, and strict mode is
opt-in. Warnings must be visible to the caller on the prepared request and on
the result. Dive's model catalog, with pricing and features, is to be ported so
the defaults work for known models.

**5. Prior history: reference large content, more analysis for text.** Design
first; proposal in [history-storage.md](history-storage.md). Referencing
attachments and other large content through an application-owned reference is
agreed in principle. Whether text history is copied, referenced with a digest,
or handled by an immutable session history needs its own document with growth
numbers before choosing.

**6. Context cancellation must not end the turn.** Design first; proposal in
[turn-interruption.md](turn-interruption.md). Agreed: the Go context ending is
not a terminal event. Not agreed: the shape of pause and resume. The design
document must explain how the production gateway runs a turn today (whether a
goroutine stays live per turn, how a dead worker's turn is picked up) and
propose the Dashi model from that.

**7. Budgets.** Design first; proposal in [budgets.md](budgets.md). A
product-level document should define where limits are declared, how they are
checked and reserved, how a budget stop is represented and resumed, and what
stays in the application.

## 2026-09-28

These refine decision 6; the design is
[turn-interruption.md](turn-interruption.md).

**8. The context ending pauses the turn; only `Cancel` ends it.** Decided.
Dashi records a resumable stop, and `Continue` resumes the turn on any worker.
A crash is a pause whose stop was never written. Rationale: deploys, crashes,
and lease loss are common and recoverable, not user decisions.

**9. A cut-off model call is sent again on resume.** Decided. It is recorded as
interrupted (`ModelFact.Interrupted`, not `Abandoned`); partial output and usage
are kept for display and accounting, never acted on. Rationale: a model call
costs money but changes nothing outside the turn, and our production hosts
already resend it.

**10. Provider-hosted tools follow the local-tool rule.** Decided. If a cut-off
call may have run a hosted tool not known to be read-only or idempotent, resume
does not resend it and the application decides; otherwise it is resent. Dashi
marks built-in hosted tools it knows are read-only, such as web search;
definitions can say so for others.

**11. Safe local tools are rerun on resume.** Decided. A started call with no
recorded result is rerun when annotated `ReadOnly` or `Idempotent`; otherwise
it is uncertain and the application reports it. This does not wait for
execution-identity work.

**12. Interruptions have their own limit.** Decided. A new
`Limits.Interruptions` (zero means the default, 10; negative means none) counts
resumes; exceeding it stops the turn
as `limited`. Interruptions do not consume `ModelAttempts`: a model call
recorded as interrupted is not counted as an attempt, though its usage and cost
still count toward spend. Rationale: they are
usually the hosting's fault, a different budget from model retries.

**13. Tool results arriving after the context ends are dropped.** Decided. No
late-result window. Rationale: rerunning safe tools covers the common case
without timing-dependent behavior.

**14. Names: `interrupted` and `uncertain`.** Decided. The resumable stop is
`StopInterrupted`. A call that started with no saved result, so Dashi cannot
tell whether it ran, stays `uncertain`.

**15. Graceful shutdown uses a built-in pause signal.** Decided. A per-call
option (`WithPause`) makes the runner stop before the next model
call or tool call and record `interrupted` ("pause requested"); in-flight work
is not cut off. Rationale: `Policy` is a four-method interface every app would
have to wrap, and `BeforeModel` does not fire during a long tool loop.

**16. `Settle` on an interrupted turn only records.** Decided. `Continue`
resumes the turn. Generalized by decision 18.

**17. The caller can declare that an ended context means cancel.** Decided.
Interrupted is the default; a runner setting
(`Runner.OnContextDone`) set to cancel makes a context end, including leaving
`Runner.Stream`, record a final `StopCanceled`. Rationale: a web handler whose
client disconnected knows the turn will not resume, and `net/http` cancels
without a cause, so the cause alone cannot say so.

**18. `Settle` only records; only `Continue` runs a turn.** Decided. This
applies everywhere, including approvals and tool results delivered to a waiting
turn, which today carry on immediately. The report is committed before `Settle`
returns, and the outcome and saved turn say when the turn is ready to continue.
Rationale: one consistent rule, and the request that records an approval should
not run the model inline; the ready flag keeps a forgotten `Continue` visible.

**19. The application passes prior history on each call; the turn record does
not store it.** Decided; answers decision 5. `Start` and every `Continue`
carry the earlier conversation, and Dashi uses it for that call only. The
turn-started step keeps the turn's own input and a fingerprint of the history,
not the history itself. A `Continue` whose history does not match the
fingerprint records a warning and proceeds; a host may set
`Continue.OnHistoryMismatch` to reject that `Continue` before any step is
committed instead. Rationale: none of the compared
libraries copies history into each run, and a turn record that grows with the
conversation stores what the application already keeps. The cost: a turn's
record no longer shows the earlier conversation, and a wrong history on resume
is the application's bug, which the fingerprint makes visible. Whether Dashi
gains sessions or conversations is a separate, open question. Referencing
attachments in a turn's own input or tool output (the proposal's content
references) remains a later refinement.

**20. The recorder is optional.** Decided. With no `Runner.Recorder`, the runner
uses an in-memory one, so a simple program needs no storage; an application
that resumes turns across processes binds a durable recorder. Rationale:
Dashi works by default.

Decisions 21–26 answer decision 7; the design is [budgets.md](budgets.md).

**21. A cost cap is a hard model-call admission limit.** Decided. `MaxCost`
covers model-call charges. Dashi starts a call only when its prepared request
has prices, a finite output-token allowance, and a worst-case charge that fits
the remaining cap. Provider-hosted operations with no bounded request count
cannot run under that cap. Local tool charges are reported separately; an
application promising an all-in customer ceiling must admit and limit those
tools itself. Missing usage has the exception in decision 24. Rationale: the
worst case is what an application can reserve against tenant credit. The cost:
turns sometimes stop with budget unused.

**22. When the next call does not fit, the turn stops; Dashi does not shrink
the reply.** Decided. The turn stops as `limited`, and Dashi does not lower
the output-token allowance to fit a cost shortfall (it still lowers it to fit
an output-token cap). This departs from decision 4, so the stop explains
itself: its reason gives the cap, the call's worst case, and the output-token
allowance that would fit when one exists. If input alone consumes the
remaining cap, no positive allowance fits; raise the cap or reduce the input
or history. Rationale: a reply silently cut short is worse than a clear stop
the application can fix.

**23. An unbounded call stops before dispatch under a cost cap.** Decided.
An unpriced model stops `limited` with a reason naming it and asking for its
prices. A priced call without a finite effective output-token allowance also
stops `limited` and asks for one; since preparation defaults `MaxTokens` to
`llm.DefaultMaxTokens`, that is rare. A call offering provider-hosted tools whose
request count is unbounded stops likewise. Rationale: a cap must never be
silently ignored; applications can supply prices without a catalog update.

**24. A call without final usage counts a best guess.** Decided. When a worker
crashes or a call is cut off before final usage arrives, Dashi preserves any
streamed counts, fills absent input from the recorded estimate, and counts the
greater of streamed output and a visible-text estimate. The call is marked
estimated in the running total. The consequence, accepted: for such calls the
cap is no longer a strict charge guarantee, since the guess can undercount.
The admission check also relies on supplied prices and the provider respecting
its output allowance. Rationale: charging the worst case would stop turns
early after a bad day of deploys.

**25. The caller sets a turn's budget; the agent carries none.** Decided.
Cost and token caps are passed on `Start` and raised on `Continue`; there is no
agent ceiling that `Start` narrows. Plan and tenant ceilings stay in the
application. Existing agent limits (`ModelAttempts`, `Deadline`,
`Interruptions`) are unchanged; whether they should also move to the caller is
not decided. Rationale: a budget depends on who is calling and what they paid
for, which the application knows and the agent's configuration does not.
Supersedes the proposal's definition ceiling.

**26. `Continue` may raise a turn's budget, and every raise is recorded.**
Decided. The saved turn records each raise with its new amounts and when it
happened, so raises are visible in an audit. Who may raise, and by how much, is
the application's authorization. The review chose "allowed when marked as an
override" of the definition ceiling; with no ceiling (decision 25) there is
nothing to override, so the part kept is the audit trail.

**27. Recorded values are compared by JSON meaning, not bytes.** Decided. Every
digest or comparison of a recorded value against a live one (commands, tool
definitions and arguments, approval argument digests, the agent,
history fingerprints, idempotent step retries) hashes or compares a canonical
JSON form: RFC 8785 layout (sorted keys, no whitespace, minimal string
escapes, ECMAScript number layout), except that numbers keep their exact
decimal value rather than rounding to a double, and a duplicate key keeps its
last value. A recorder therefore only has to preserve JSON meaning; `jsonb`
storage works. New digests carry a `v2:sha256:` prefix and history
fingerprints move to `v2:`; bare-hex digests and `v1:` fingerprints in older
records are still checked their original way. Rationale: jsonb is the most
common store, and byte-exact digests made its reloaded turns fail recovery
with "tool binding changed" or a false history mismatch.

**28. Money is an exact integer count of nano-units.** Decided. Prices,
charges, caps, and spend use `money.Amount`, an int64 of 1e-9 currency units
as in `google.type.Money`; JSON writes a decimal string (`"0.5"`) and still
reads the bare numbers of older records. Known break, accepted: a retry of a
capped `Start` or budget-raising `Continue` recorded before exact money returns
`ErrCommandConflict`, since its digest covers the old number encoding. Cents
are too coarse for per-token prices; nanos hold every catalog price exactly and
cover about ±$9.2 billion.
One rounding rule: a cost at risk rounds up, so rounding never breaches a cap,
and a settled charge rounds half to even; each rounds the exact sum of its
token-rate products once per call. An overflow makes the amount unknown rather
than wrapping. `LimitDetail` splits counts (`Limit`, `Used`) from money
(`CostLimit`, `CostUsed`, `CostAtRisk`). Rationale: integrators keep exact
ledgers keyed by attempt ID, and float sums drift and can misjudge a cap at its
boundary. The type lives in a stdlib-only `money` package because `llm`
imports `tool`, so `tool` cannot import `llm`.

**29. The zero `Limits` is the default configuration.** Decided. A zero count
means its default (8 model attempts, 10 interruptions, 3 provider pauses, 1
concurrent tool); a negative `Interruptions` or `ProviderPauses` means none, as
with `net.Dialer.KeepAlive`. `NoReservedFinalAnswer` keeps the reserve on by
default. `DefaultLimits` was removed. Dashi resolves defaults when it accepts
an agent and records the concrete values, so a saved turn keeps its limits.
Known cost, accepted: after a default changes, continuing an older turn with
zero limits returns `ErrAgentChanged`; pin the recorded values instead.
Turns recorded before this change are not supported: Dashi had no users or
stored turns, so there is no migration, and such a turn must be restarted.
Rationale: `Limits{}` used to be rejected, and a forgotten bool silently turned
off the final-answer reserve.

**30. Names: `Agent`, `Runner`, `Settle`, and `CommandID`.** Decided 2026-09-28.
`Definition` becomes `Agent`: the same plain JSON configuration, recorded under
`"agent"`, with `ErrAgentChanged`. `Engine` and `Bindings` merge into `Runner`,
a value holding the agent and its long-lived services (client, tools, recorder,
policy, observer, `OnContextDone`). `Runner.Run(ctx, turn, cmd, opts...)`
collects a result and `Runner.Stream` yields one; per-call options are
`WithOwner` and `WithPause`, and `OnHistoryMismatch` moves to `Continue`. An
empty `Agent.Provider` or `Agent.API` is filled from the runner's client; a
value that differs from the client's is still an error. The command `Accept`
becomes `Settle`, and every command's `ID` becomes `CommandID`. The approval
steps become `approval_settled` and `hosted_approval_settled`. "Turn" keeps its
meaning: one input processed until the agent settles, possibly many model calls.
"Session" is reserved for a future conversation of many turns. Dashi keeps Go's
`StopCanceled`; an ACP adapter maps it to `cancelled`. Rationale: every
comparable library names these things Agent and Runner (ADK-Go and Eino each
have a runner holding the agent and services; OpenHands' agent is frozen,
serializable configuration like ours). Dashi's turn matches ACP's prompt turn,
the turn in the Codex App Server's thread, turn, and item, and Anthropic's
`end_turn`. Some SDKs (OpenAI Agents, Claude Agent, pi) count one model call
plus its tool calls as a turn; Dashi calls that a model attempt, as in
`Limits.ModelAttempts`. `Accept` read like "approve", which is confusing for a
denial; `Settle` matches the step names `model_settled` and `tool_settled`. A
command's `ID` looked like the turn ID; `CommandID` names its idempotency role.
Dashi had no users, so there is no migration.

**31. Hooks replace `Policy`, and bound tools run by default.** Decided
2026-09-28; the design is [hooks.md](hooks.md). `Runner.Hooks` replaces
`Runner.Policy`, and `denyToolPolicy` is removed. With no hook opinion, local
and external tools the app binds are allowed; a gate is one `BeforeTool` hook
that denies or defers. Rationale: Dashi works by default, and binding a tool is
the app's statement that the model may use it. Pi and most agent libraries
behave this way, while deny-by-default made the first run of every new app
fail. Dashi had no users, so there is no migration.

**32. Steering interruptions count as model attempts.** Decided 2026-09-28;
supersedes part of decision 12. The design is [steering.md](steering.md). A
model call cancelled because of an urgent steer (`ModelFact.Interrupted:
"steering"`, `InterruptSteering`) counts toward `ModelAttempts` and spend, and not toward
`Interruptions`. Every other interrupted call keeps decision 12's rule. A call
that completes before the cancel takes effect is not interrupted. Rationale: a
steering interruption is the user's choice, not the hosting's fault, and
counting it bounds a flood of urgent steers.

**33. A first turn needs only a client and input.** Decided 2026-09-28; the
design is [zero-config.md](zero-config.md). An empty turn ID (on `Start`) and
`CommandID` are generated UUIDv7s (`dashi.NewID`); an empty `Agent.Model` is
`catalog.Default` when the client knows it; an empty `Revision` is `"auto:"`
plus a digest of the agent's behavior fields, excluding `Limits.Deadline`;
`Agent.ID` may stay empty. On an existing turn, a zero `Agent` is the recorded
one and a partly set agent takes empty fields from the record;
`ErrAgentChanged` names the differing fields. A nil `Policy` allows bound
tools (decision 31 applied early), recording revision `default-v1`. A missing
API key fails the first `Send` before dispatch with `llm.ErrNoAPIKey`, not
`New`, and is skipped for a custom endpoint or HTTP client. Constructors keep
returning concrete types; runtime choice uses a documented `llm.Client`
switch, with no registry. Rationale: works by default. No warning is recorded
for generated IDs: step-sequence fencing already stops a stale retry, and a
recorded warning would sit in every turn of a durable host that chose to rely
on it. Dashi had no users, so there is no migration.
The nil-Policy default is now "no hook opinion" (decision 31).
