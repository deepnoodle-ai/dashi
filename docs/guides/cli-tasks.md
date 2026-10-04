# Delegated work in the CLI

Ask the agent to use Explore for a focused read-only investigation, or General
for work that can require effects. The parent receives a stable task ID and
continues. A child's text does not stream into the parent's reply.

Use `/tasks` during foreground work or while idle. A row shows the task,
subagent, state, turns (once it has more than one), **model calls**, tool
calls, and child spend. Model calls count calls that were started; they do
not prove a provider request was sent.
`/task ID` shows the recorded child Turn, parent call, accepted
evidence/resource identities, destination, limits, stop, ordinary messages,
and tool results. It counts model calls confirmed sent apart from started
calls with no recorded result. Inspection sends no model request, loads no
attachment content, and has no external effect. Long messages/results disclose
their 64 KiB display limit; the full content remains in the Turn record.

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
  record confirms it. It does not roll back effects or prove that a call with
  an unknown result never ran. Foreground work and other children continue.
- Ctrl-C interrupts the foreground parent and reports continuing children.
  Use `/tasks` and `/task-stop ID` to stop them. During a child approval,
  Ctrl-C retains that child prompt. At idle, running children also stay alive;
  explicit quit closes all children.
- `/task ID continue` resumes a ready stopped child with its recorded destination
  and restrictions. A waiting approval uses its original bound response. A
  model-call limit requires confirmation of the doubled model-call cap; other
  limits require their recorded remedy.
- `/task ID done CALL_ID` requires verification that this exact already-started
  effect completed. It records that verification with output unavailable. It
  executes no tool. Then use `continue` if all pending work is resolved.
- `/task ID discard` cancels that stopped child with an explicit warning about
  calls whose results are unknown. Wrong calls, stale turn records, ended
  tasks, and competing actions reject; none implicitly selects a different
  target. Parent `/done`, `/discard`, and empty Enter retain their parent
  target.

The agent and its tasks can message each other. A message shows as one line,
cut at the terminal edge: `→ t_k3f2  Skip vendor/` for one the agent sent
with `task_send` (`(queued)` after the ID when it waits for the task's turn
to end), and `← t_k3f2  Parse is in config/load.go` where the agent got one a
task sent with `message_parent`. A message can start a task's next turn,
whose status line reads `↳ task t_k3f2 (explore) turn 2 …`. `/tasks ID`
prints each turn of that task with the messages it got and sent, in order,
and its reply. A task's message reaches the agent the way its result does:
in the next queue Turn once the agent is idle, or sooner when a
`task_status` wait returns it.

A completed child's result reaches the parent through its recorded notice ID.
Inspection does not consume it. An idle parent receives it in a scheduled queue
Turn; a pending start and a stopped parent hold the notice until they are
resolved. Reload/callback repeats cannot add a recorded notice again.

Quit, `/new`, and session switching interrupt children and wait for their
records before closing pinned services and releasing session ownership. A
shutdown that cannot finish this keeps ownership and reports the failure. No
daemon remains after process exit. Reopen recovers children from parent call
records before unrelated new work. Canceled/ended children stay ended. Waiting
calls and calls with unknown results stay visible; finished calls do not run
again. Missing bindings identify affected children; restore the recorded
destination/tools and reopen before controlling them. Missing or corrupt
required journals leave the recorded task visibly unavailable and accounting
incomplete. Restore those journals and reopen before unrelated work,
continuation, sending results, or successful print completion.

`/tasks` reports parent, child, and inclusive spend from records, counting each
child Turn once. Model money and tool money stay separate; unknown charges,
unpriced work, estimated usage, and incomplete usage remain explicit. Running
values are lower bounds. Explore has 24 model calls and General has 32;
neither has a monetary cap. Any configured parent cap excludes children. This
slice adds no budget flag, queue cap, or family cap.

Print mode waits for children and permitted notice Turns before success. A stop
that needs action exits unsuccessfully and preserves records for reopening.
Existing `--yes` applies to each original call. Child updates/diagnostics go to
stderr; stdout contains only the final parent reply. Piped input similarly
waits for permitted task work at EOF and fails if no input can resolve a stop.
EOF never supplies an implicit approval or repeats an unknown effect.
