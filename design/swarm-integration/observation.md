# Showing what an agent is doing without writing a watch loop

See the [main recommendations](README.md). A UI needs truthful progress updates,
and closing the UI must leave a background agent running.

## R19 — distinguish “running now” from “may have been interrupted”

[Before and after](examples/r19-running-status.md).

**Problem.** Dashi's saved records cannot distinguish an active model request
from a process that died after starting one. `Turn.View` conservatively describes
unfinished saved work as interrupted or unknown. [Swarm][event] supplies a
separate `live` flag and corrects that interpretation while its worker is alive.

**Change.** Provide a view helper that also accepts worker activity: running,
stopped, or unknown. Keep the recorded stop and current activity distinguishable.
Swarm supplies reliable worker status; Dashi supplies the interpretation of its
records. Use one trustworthy activity snapshot. Session.WorkerStatusRunning means an
unexpired claim and may-still-act, not verified process liveness. Preserve that
evidence distinction; a stored claim or stale heartbeat cannot prove running now.

**Benefit:** remove the application's status-repair convention. Live status must
not hide unknown tool/model results or override a recorded final stop.

## R20 — provide a subscription to useful current state

[Before and after](examples/r20-progress-subscription.md).

**Problem.** [Swarm WatchTurn][watch] loads a snapshot, watches new records,
tracks worker activity, catches up when the worker exits, merges bursts of
updates, reloads cost, handles errors, and cleans up the reader. The function and
its support declarations are 188 lines, plus a separate 140-line follow adapter.
Dashi's Follow delivers Steps, leaving this assembly to the application.

**Change.** Offer a current-state subscription over the application's loader,
record reader, and worker-status source. Scope updates to Dashi reply, pending work, limits, model-cost evidence and
activity. Host ledger overlays, worker errors and retired formats stay explicit.
Incrementally update the view where justified instead of replaying everything.

The subscription must register notifications before its first read, read final
committed records before reporting worker exit, and report activity changes without a new record. Refresh state on notifications
and distinguish worker generations; old notifications are not current truth. Cancellation must stop and join reader work while
leaving the worker alone. Swarm still checks access and conversation existence.

**Benefit:** potentially less watch setup and fewer races, not deletion of the
whole adapter. Compare a WatchViews helper with existing Follow plus a small view
accumulator and activity subscription before choosing the larger surface. A progress snapshot
is for display; dispatching a tool still requires a current authorized lookup.
Allow optional merging of rapid UI updates while retaining raw Follow for
consumers that need every record.

## R21 — use the existing feed for output as it is generated

[Before and after](examples/r21-live-output.md).

**Problem.** [Swarm's Runner][runner] sets no Observer, so this watch path sees
committed records rather than model deltas and tool progress.

**Action now.** Connect `feed.Hub` and `feed.Follow` where Swarm wants live output.
They already combine temporary previews with saved results and signal when a
reader has missed previews. Clear that message's preview on a gap and keep
following to its committed finish; reload after an early run/commit failure.

Hub previews come from workers in the same process; other processes need a
host transport preserving sequence and gaps. Keep previews separate from saved
history, billed usage and published posts.
Show summary generation separately from the agent's ordinary reply. Document
one background worker with several detachable readers. A reader-owned
`Runner.Stream` cancels execution when its range ends, so it is the wrong
lifetime for that particular workflow.

**Benefit:** more responsive presentation. This may add UI code; it is not a
line-reduction claim.

## R22 — observe records after the agent stops

[Before and after](examples/r22-updates-after-finish.md).

**Possible use.** `Follow` ends when the Turn ends, although late results can be
saved afterward. A separate post-stop activity reader could use an all-records
iterator. No current Swarm consumer of that kind was identified.

**Candidate.** A feed helper can follow all later records until canceled.
Keep normal agent completion distinct from the lifetime of that separate reader.
Replacing WatchTurn with an endless iterator would not supply the missing feature.

Swarm already posts model charges in its record-commit transaction. Tail is not
required for that accounting. Host-only money updates also need their own
notifications; watching Steps cannot reveal them.

**Judgment: defer.** Establish a real consumer and its lifetime before adding the
API. For one-off inspection, use StepsAfter or reload. Reusing cursor/paging rules
is a potential benefit, not demonstrated Swarm code removal.

## Cases the progress API must handle

Use Swarm's existing tests for a worker exiting without a new record, a final
save racing with exit, a reader disconnecting, out-of-order notifications,
several tool results arriving together, and old records being retired. Add slow
preview readers and late billing. A configurable polling interval alone would
leave these problems with the adopter.

[event]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1173
[watch]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1280
[runner]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L676
