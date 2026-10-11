# Accept background work and recover useful failure details

Status: Draft\
PRD PR: pending  Implementation PR: pending  Updated: 2026-10-11

## Problem and users

Application developers need to accept agent work before a worker can run it.
They must also cancel accepted work after credentials disappear.
Swarm currently creates a closed pause channel to record that work.
It installs a first-call hook to deliver reminders known at acceptance.
After a restart, recorded model failures lose retry hints available on `llm.Error`.

The developer's job is to save accepted work, run it later, and choose a recovery
action from saved evidence. Scheduling, credentials, storage transactions,
ownership, account policy, and approved tool execution remain application responsibilities.

## Context and concepts

The reviewed [R01, R03, and R06](../../design/swarm-integration/execution.md)
establish these cases. Their [review](../../design/swarm-integration/reviews/execution-history.md)
requires original settings, stable retry timing, and conservative send evidence.
`Runner.Run` already fences commits with `WithOwner` and recognizes repeated commands.
`ContextItem` already defines authority, identity, and lifetime.
`TurnView.Next` already describes the execution action.

- **Admission:** `Runner.Admit` records a `Start` and leaves the Turn ready for `Continue` or `Cancel`.
- **Initial context:** `Start.Context` contains ordinary `ContextItem` values saved with the accepted input.
- **Failure details:** `TurnView.Failure` contains safe model failure facts. `Next` remains the execution API.

## Use cases and acceptance

### UC-1: Accept, retry, then cancel queued work

The developer saves the original `Start`, Turn ID, and command ID in application storage.
They call `runner.Admit(ctx, turn, start, dashi.WithOwner(owner))`.
They later reload and call `Runner.Run` with `Cancel` or `Continue`.

- [ ] Admission invokes no model preparation, send, tool discovery, tool execution, hooks, steering source, or content loader.
- [ ] An acknowledged admission records `StopReady`, gives `NextContinue`, and uses no model calls, spend, or resumes.
- [ ] A new admission needs no Client when provider, API, model, and any enabled compaction window are explicit.
- [ ] Enabled compaction without a Client or explicit window fails before any commit with a useful error.
- [ ] An exact retry returns current saved state without consulting changed credentials, client identity, catalogs, or defaults.
- [ ] Changed explicit behavior, input, initial context, or compaction policy conflicts. Repeated IDs never execute accepted work again.
- [ ] Every commit forwards the owner. Failed writes and lost acknowledgments retain `Run`'s existing error and reload contract.
- [ ] Cancel needs no Client. Continue requires the matching Client and runs the accepted work without charging an admission resume.

### UC-2: Supply project reminders once

The developer sets `start.Context` to a checkout reminder with an ID, origin,
name, and lifetime. They admit or run the Start, reload, and continue it.

- [ ] The first request contains each item once, after `Start.Input`, with its recorded authority and complete content.
- [ ] Reload, repeated Start, and Continue never add the initial items again.
- [ ] `Snapshot`, `NextCallOnly`, named message identity, and later `AddMessages` retain their existing behavior.
- [ ] IDs participate in Turn-wide duplicate checks. Repeated IDs in the initial batch fail before recording.
- [ ] All item fields and order belong to the Start hash. Changing authority or lifetime under the same command ID conflicts.
- [ ] Nil and empty initial context preserve old behavior. Invalid roles, reminder tiers, or content fail before any commit.
- [ ] Run and Admit mark Starts with nonempty initial context as record version 2. Older record readers reject them before execution.
- [ ] Updated readers reject context-bearing version-1 Starts and unknown versions. Nil/empty context retains version 1 and existing hashes.

### UC-3: Handle failure after a restart

A provider rejects a request with HTTP 429 and a 30-second delay.
The developer reloads `view.Failure`, checks `view.Next()`, and saves an absolute
retry time in application storage. Another request loses its connection after possible dispatch.

- [ ] Reload preserves category, classified reason, status, send evidence, retry hint, delay, and the original failure time.
- [ ] Preparation errors expose the same safe details and prove nondispatch without fabricating a model call or cost.
- [ ] Computing `RecordedAt.Add(RetryAfter)` after restart produces the original due time.
- [ ] Unknown dispatch or a partial result remains unresolved even when `Retryable` is true. Continue cannot resend it without existing recovery evidence.
- [ ] New model failures save no raw provider body, arbitrary provider code, cause text, credentials, or endpoint secrets.
- [ ] Records without failure details retain their current `Next`, pending work, send safety, and accounting behavior.

## Decisions and scope

- Reuse Start and its command identity for Admit. A separate admission command would duplicate accepted-work identity and retry rules.
- Record a ready stop. Reusing an interrupted pause would retain the workaround's misleading explanation.
- Use existing `ActiveCompactionPolicy.Window` and `StartFact.ActiveCompactionWindow`. A second caller window would compete with the existing bound and evidence.
- Save initial items on Start. Adding them through a later command leaves acceptance incomplete if its write fails.
- Gate initial context with the existing step version. Optional fields alone let older readers silently omit operator reminders.
- Add optional failure facts beside existing execution evidence. Persisting `llm.Error` would expose arbitrary provider data and error causes.
- Preserve Next and application scheduling. A second decision API or automatic retry would conceal policy and unknown effects.

This track excludes an execution driver, scheduling, model registry, end-at-limit
policy, accounting changes, and Swarm edits. It adds no storage interface or worker lease service.

## Success and risks

Committed offline examples demonstrate all three workflows. Compatibility and
failure-injection tests cover old hashes, reload, partial admission writes, and unknown sends.
`make check` passes. Independent reviewers run the examples and examine saved-record safety.
Upgrade record readers and saved-command consumers before enabling new writers.
The [technical spec](../design/swarm-turn-inputs.md) defines version rejection, command rollout, and retry matching.
No design question blocks the planning review.
