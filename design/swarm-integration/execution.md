# Starting, stopping, and resuming background work

See the [main recommendations](README.md). The application should be able to
save work, run it, and understand why it stopped using ordinary library calls.
These ideas concern the part of Swarm that translates those needs into Dashi
commands. API names below are sketches.

## R01 — save an agent Turn without starting a model request

[Before and after](examples/r01-save-queued-work.md).

**Problem.** A user can cancel queued work before it starts. [Swarm][cancel]
creates a closed pause channel, calls `Runner.Run` to save the initial Turn,
then cancels it. If credentials have disappeared, it also reconstructs the
provider identity. Dashi supports this, but the caller must discover the trick.

**Change.** Add an explicit `Runner.Admit` operation: validate and save the
initial Turn, with no model request or tool execution. Saved provider/API/model identity and resolved compaction-window evidence
should suffice without a client. The Turn can then continue or cancel.
This removes the pause-channel workaround. The application still saves its own
queued request and delivery acknowledgment in its transaction.

A repeated admission must match the original resolved settings. Changed defaults
must not silently change work that was already accepted.

## R02 — say “reached a limit” when work reached a limit

[Before and after](examples/r02-finish-at-limit.md).

**Problem.** Swarm [ends some limited work with Cancel][next], then
[recognizes the reason text][view] to display it as incomplete. That includes
model-call exhaustion, context overflow, and application routine caps.

**Change.** Allow a caller to finish a Turn while preserving the actual limit
or failure. Expose whether it can resume and whether it has ended. A shared
budget shortage may wait for a top-up; an application cap may finish the work.
Make “end when the model-call allowance is exhausted” an explicit policy.

This removes cancellation translation and reason-string checks. Keep the
existing resumable default. Reference the current stop sequence instead of accepting a replacement Stop.
Reject stale references, recognize exact retries, preserve late-result handling,
and prohibit fabricated completion.

## R03 — give applications enough information to handle a failure

[Before and after](examples/r03-handle-failure.md).

**Problem.** After a restart, the application needs to know whether to ask for
credentials, wait, or investigate an uncertain request. `llm.Error` has category
and retry information that `ModelFact` does not fully retain. `TurnView.Next`
groups several different situations into `NextFix`.

**Change.** Save safe, structured failure details and expose them in the view,
including preparation failures. Keep Next as the execution API; do not add a
competing Decision vocabulary. Store failure time plus retry delay, or an
absolute due time, so restart does not reset the wait.

Swarm still chooses its retry policy. “Retryable” must never mean “resend a
request whose execution is unknown.” Provider secrets and raw bodies stay out
of saved records. Current Swarm does not implement the illustrated provider-delay
policy. The metadata supports a future retry/repair feature; it does not create it.
No application-code reduction has been demonstrated.

## R04 — test a helper that advances saved work

[Before and after](examples/r04-advance-saved-work.md).

**Problem.** [Swarm][drive] loads the Turn, finds a saved command that still
needs applying, chooses the next command, saves it, runs it, and decides whether
to continue. It also guards against repeatedly running without making progress.
Much of this is knowledge of Dashi, rather than Swarm's product.

**Deferred experiment.** After improving the outcomes above, prototype a small optional
helper with one operation such as:

```go
// Proposed API: advance one command and explain what happened.
progress, err := driver.Advance(ctx, turnID)
// progress: changed, waiting for an input/tool/budget, finished, or needs attention.
```

It must apply already-saved commands first, preserve command IDs across retries,
and return when nothing can advance. Swarm still chooses the worker, protects
against two workers running the same Turn, wakes it, and joins it on shutdown.
Uncertain writes require reloading saved state before deciding what to do.

This could simplify `advance`, `unapplied`, `plan`, `decodeCommand`, and
`runAgain`. **Its value is unproven until the adapter actually gets smaller.**
The full after example must show a command journal and application planner,
including original admission/history, account availability, caps and ownership.
Stable IDs do not guarantee exactly-once remote execution. Avoid requiring a
large Recorder interface.

## R06 — accept initial reminders directly

[Before and after](examples/r06-initial-reminders.md).

**Problem.** [Swarm][runner] installs a BeforeModel hook just to add the initial
reminders once. It checks the saved sequence to avoid adding them again.

**Change.** Let Start accept initial context items, including their IDs, origin,
authority, and lifetime. Save them before the first request. This removes a
hook whose only job is initialization. Existing AddMessages still serves later
input.

## R07 — compare a small application destination table first

[Before and after](examples/r07-configured-models.md).

[Swarm][route] parses provider/model, finds a client, and reconstructs identity
for recovery. First share one application table with identity separate from
credentials. Preserve admitted destination information when keys disappear.
Agent's provider/API/model fields do not pin an endpoint; the application still
needs endpoint binding and model authorization.

**Defer a library registry.** Its extra types and configuration rules have not
shown a better result than this small table. A second consumer could establish
which lookup behavior belongs in Dashi.

## R08 — handle unsupported model settings precisely

[Before and after](examples/r08-unsupported-setting.md).

[Generate][generate] responds to any request-preparation error by retrying with
reasoning removed. That can hide an unrelated failure.

Give callers a typed “this setting is unsupported” preparation error. Reuse
llm.Setting, existing capability policies and Adjustments; defer a general
settings-negotiation framework. Remove only the rejected setting and report the effective settings.
Start with reasoning support. Invalid messages, schemas, and endpoints should
keep their own errors. This is a focused cleanup rather than a new runtime.

[cancel]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1030
[next]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L794
[view]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1173
[drive]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L686
[runner]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L621
[route]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L124
[generate]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/generate.go#L66
