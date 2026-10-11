# Showing what an agent is doing without writing a watch loop

See the [main recommendations](README.md). A UI needs truthful progress updates,
and closing the UI must leave a background agent running.

## R19 — distinguish “running now” from “may have been interrupted”

**Problem.** Dashi's saved records cannot distinguish an active model request
from a process that died after starting one. `Turn.View` conservatively describes
unfinished saved work as interrupted or unknown. [Swarm][event] supplies a
separate `live` flag and corrects that interpretation while its worker is alive.

**Change.** Provide a view helper that also accepts worker activity: running,
stopped, or unknown. Keep the recorded stop and current activity distinguishable.
Swarm supplies reliable worker status; Dashi supplies the interpretation of its
records. A stored owner ID or stale heartbeat does not prove a worker is alive.

**Benefit:** remove the application's status-repair convention. Live status must
not hide unknown tool/model results or override a recorded final stop.

## R20 — provide a subscription to useful current state

**Problem.** [Swarm WatchTurn][watch] loads a snapshot, watches new records,
tracks worker activity, catches up when the worker exits, merges bursts of
updates, reloads cost, handles errors, and cleans up the reader. The function and
its support declarations are 188 lines, plus a separate 140-line follow adapter.
Dashi's Follow delivers Steps, leaving this assembly to the application.

**Change.** Offer a current-state subscription over the application's loader,
record reader, and worker-status source. Each update should explain the current
reply, pending tools, limits, cost, and whether work is active or finished.
Incrementally update the view where justified instead of replaying everything.

The subscription must register notifications before its first read, read final
committed records before reporting worker exit, and report activity changes even
when no new record was saved. Cancellation must stop and join reader work while
leaving the worker alone. Swarm still checks access and conversation existence.

**Benefit:** a simpler application watch path and fewer races. A progress snapshot
is for display; dispatching a tool still requires a current authorized lookup.
Allow optional merging of rapid UI updates while retaining raw Follow for
consumers that need every record.

## R21 — use the existing feed for output as it is generated

**Problem.** [Swarm's Runner][runner] sets no Observer, so this watch path sees
committed records rather than model deltas and tool progress.

**Action now.** Connect `feed.Hub` and `feed.Follow` where Swarm wants live output.
They already combine temporary previews with saved results and signal when a
slow reader has missed previews. On a gap, reload saved state.

Keep preview text separate from saved history, billed usage, and published posts.
Show summary generation separately from the agent's ordinary reply. Document
one background worker with several detachable readers. A reader-owned
`Runner.Stream` cancels execution when its range ends, so it is the wrong
lifetime for that particular workflow.

**Benefit:** more responsive presentation. This may add UI code; it is not a
line-reduction claim.

## R22 — allow cost updates after the agent finishes

**Problem.** `Follow` ends when the Turn ends, although late results can be saved
afterward. Its documentation tells account/mirror readers to continue through
StepsAfter. Swarm can also correct billing in its own tables without a new Step.

**Change.** Offer an explicit mode that follows all later records until canceled.
This would be useful with R11's billing-correction records. Keep normal agent
completion distinct from waiting for final billing.

If money changes remain outside Dashi's records, the application must also supply
notifications for those changes. Watching Steps alone cannot reveal them.

**Benefit:** fewer custom tailing paths for accounting consumers. Preserve
cursor checks, paging, and duplicate handling.

## Cases the progress API must handle

Use Swarm's existing tests for a worker exiting without a new record, a final
save racing with exit, a reader disconnecting, out-of-order notifications,
several tool results arriving together, and old records being retired. Add slow
preview readers and late billing. A configurable polling interval alone would
leave these problems with the adopter.

[event]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1173
[watch]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1280
[runner]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L676
