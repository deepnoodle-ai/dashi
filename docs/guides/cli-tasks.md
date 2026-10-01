# Delegated work in the CLI

Ask the agent to use Explore for a focused read-only investigation, or General
for work that can require effects. The parent receives a stable task ID and
continues. A child's text does not stream into the parent's answer.

Use `/tasks` during foreground work or while idle. A row shows the task,
subagent, state, **model attempts**, tool calls, and child spend. Attempts count
admissions; they do not prove a provider call. `/task ID` shows the recorded
child Turn, parent call, admitted evidence/resource identities, destination,
limits, stop, ordinary messages, and tool results. Its confirmed dispatch count
and unresolved intents are separate. Inspection performs no model dispatch,
content resolution, or external effect. Long messages/results disclose their
64 KiB display limit; the full content remains in the Turn record.

Fullscreen and inline modes use the native detail viewport. Page Up/Down
scrolls; Enter/Escape closes it. The composer draft and attachment selection
return afterward. Plain mode prints the same details.

Each approval identifies the original child Turn/call, tool, arguments, and
existing preview/scope. Parent and child questions share one dialog owner.
While a dialog remains pending, type `/tasks`, `/task ID`, or `/task-stop ID`
to inspect or stop exactly one child. Escape returns to the original dialog
with its draft, selection, and call unchanged. Resolution commands wait until
the dialog closes.
Approval of task creation grants no later child effects. Allow-once stays on
that original call; remembered grants keep their existing run scope.

- `/task-stop ID` stops that live child. Cancellation is reported only after its
  record confirms it. It does not roll back effects or prove an uncertain effect
  never ran. Foreground work and other children continue.
- Ctrl-C interrupts the foreground parent and reports continuing children.
  Use `/tasks` and `/task-stop ID` to stop them. During a child approval,
  Ctrl-C retains that child prompt. At idle, running children also stay alive;
  explicit quit closes all children.
- `/task ID continue` resumes a ready stopped child with its recorded destination
  and restrictions. A waiting approval uses its original bound response. An
  attempt limit requires confirmation of the doubled attempt cap; other limits
  require their recorded remedy.
- `/task ID done CALL_ID` requires verification that this exact already-started
  effect completed. It records that verification with output unavailable. It
  executes no tool. Then use `continue` if all pending work is settled.
- `/task ID discard` cancels that stopped child with an explicit warning about
  uncertain effects. Wrong calls, stale revisions, terminal tasks, and competing
  actions reject; none implicitly selects a different target. Parent `/done`,
  `/discard`, and empty Enter retain their parent target.

A completed child's result reaches the parent through its recorded notice ID.
Inspection does not consume it. An idle parent receives it in a scheduled queue
Turn; pending admission and a stopped parent hold delivery until their existing
resolution. Reload/callback repeats cannot redeliver a recorded notice.

Quit, `/new`, and session switching interrupt and settle children before closing
pinned services and releasing session ownership. A shutdown that cannot settle
keeps ownership and reports the failure. No daemon remains after process exit.
Reopen recovers children from parent call records before unrelated new work.
Canceled/ended children stay ended. Waiting or uncertain effects stay visible;
settled effects do not run again. Missing bindings identify affected children;
restore the recorded destination/tools and reopen before controlling them.
Missing or corrupt required journals leave the recorded task visibly unavailable
and accounting incomplete. Restore those journals and reopen before unrelated
work, continuation, result dispatch, or successful print completion.

`/tasks` reports parent, child, and inclusive spend from records, counting each
child Turn once. Model money and tool money stay separate; unknown charges,
unpriced work, estimated usage, and incomplete usage remain explicit. Running
values are lower bounds. Explore has 24 model attempts and General has 32;
neither has a monetary cap. Any configured parent cap excludes children. This
slice adds no budget flag, queue cap, or family cap.

Print mode waits for children and permitted notice Turns before success. A stop
that needs action exits unsuccessfully and preserves records for reopening.
Existing `--yes` applies to each original call. Child updates/diagnostics go to
stderr; stdout contains only the final parent answer. Piped input similarly
waits for permitted task work at EOF and fails if no input can resolve a stop.
EOF never supplies an implicit approval or repeats an unknown effect.
