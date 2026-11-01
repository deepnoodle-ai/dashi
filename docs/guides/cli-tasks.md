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

Every task line starts with the task ID. In fullscreen mode, it ends in
"click to watch" instead of the inspect hint. Clicking it, choosing "Open task" from its right-click menu, or
typing a bare `/task ID` opens that task's pane beside the conversation, or
focuses it if it is open; the line then reads "watching". The first task line
of a session shows a one-time hint saying so. This works while an approval
waits too. The pane's title row reads task ID · state · spend · model calls
· subagent, shows "needs approval" in the warning color in place of the
state while the task waits for you, and ends with a `×` that closes it. Below the title,
one quiet line gives the task's description, then the child's conversation
streams live in the same style as the main pane: recorded steps first, then
live ones, each once. A turn that `task_send` starts continues in the same
pane. An ended task keeps its pane until you close it; closing a pane never
stops the task. Typing still goes to the main agent. For the full facts,
right-click the pane's title and choose "Task details", or type
`/tasks ID`; both show in the details pane, above the task's turns. Inline
and plain modes print `/task ID` details instead.

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
  and restrictions. A waiting approval uses its original bound response. With
  `--decide command_risk`, a waiting bash call is checked as a live one is,
  and the child's record keeps the answer with your decision; `/task ID`
  shows it, such as `Recorded approval: call c1 · bash · denied · decide
  command: severe 91% — flagged`. A
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
and its reply.

A task's result and its messages reach the parent the way `delegate` delivers
them with `Config.Session`. A result or a steer message (the default) that
arrives while the parent works reaches its running Turn before the next model
call. A queue message, or anything that arrives while the parent is idle,
waits on the session's queue and starts a queue Turn. When a prompt queued
with `/queue` waits there, nothing starts: the next prompt takes the queued
prompt and the task's items together. Inspection does not
consume a result. A pending start and a stopped parent hold queued results
until they are resolved, and the record keeps a result from reaching the
parent twice across Turns. When a `task_status` read returns a result while
the parent works, its notice can still reach that Turn, so the model may see
the result twice there.

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
unpriced work, calls without final usage, and incomplete usage remain explicit. Running
values are lower bounds. Explore has 24 model calls and General has 32;
neither has a monetary cap. Any configured parent cap excludes children. This
slice adds no budget flag, queue cap, or family cap.

Print mode waits for children and permitted notice Turns before success. A stop
that needs action exits unsuccessfully and preserves records for reopening.
Existing `--yes` applies to each original call. Child updates/diagnostics go to
stderr; stdout contains only the final parent reply. Piped input similarly
waits for permitted task work at EOF and fails if no input can resolve a stop.
EOF never supplies an implicit approval or repeats an unknown effect.
