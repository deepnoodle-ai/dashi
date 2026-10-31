# Watch delegated tasks beside the conversation

Status: In review\
PRD PR: [#357](https://github.com/deepnoodle-ai/dashi/pull/357) · Implementation PR: Not opened · Updated: 2026-10-31\
Tech spec: [CLI panes](../design/cli-panes.md)

## Problem

A developer runs Dashi as a coding agent and delegates work to tasks. Today
the fullscreen CLI shows a task as one status line under its `task` call:
state, model calls, spend. To see what the task does, the developer types
`/task ID`, and a modal viewer takes over the footer until they close it. It
shows a snapshot, not live work. With two or three tasks running, the
developer cannot watch any of them and keep composing at the same time.

The same pressure applies to long output. `/tasks`, `/settings`, and a long
tool result or diff push the conversation out of view or open a modal
dialog. Each look costs the developer their place.

If we do nothing, delegation stays a black box until a task ends. Developers
must trust the result line or interrupt to inspect.

## Context

- The [task track](cli-tasks.md) gave the CLI stable task IDs, status lines,
  `/tasks`, `/task ID`, approvals labeled with the task, and recovery after a
  restart. `delegate.Config.OnChild` already delivers every child event:
  committed steps, model text as it streams, and tool progress. The CLI uses
  it only to update the status line.
- The [terminal UI track](codex-terminal-ui.md) set the visual hierarchy,
  selection and copy, and the 80×24 and 40×16 acceptance sizes.
- Wonton v0.12.0, already on main, adds resizable split layouts
  (`tui.Split`, `SplitState`) and right-click menus. Its reference app,
  `examples/corral`, shows the interaction pattern: click to focus, drag the
  edge between panes, right-click for zoom and close.
- Terminal workspaces such as tmux and herdr put live sessions side by side.
  A developer can do that by hand today, but a second terminal loses Dashi's
  link between a task, its parent call, and its approvals.

## Users and job

Primary user: a developer who runs Dashi fullscreen in a repository and
delegates work to tasks. The job: see what each task is doing, inspect
details, and keep composing to the main agent, without a modal dialog
taking over the screen.

## Concepts

- **Pane**: one area of the fullscreen screen, side by side with others,
  that shows one conversation or one set of details and scrolls on its own.
  The screen always has the **main pane**, the conversation with the agent.
- **Task pane**: a pane that shows one task's conversation live, with a
  title row that names the task and its state. One per task.
- **Details pane**: one reused pane for `/tasks`, `/status`, `/settings`, and
  long tool output or diffs. New details replace the old ones.
- **Focus**: the pane that scroll keys act on. Typing always goes to the
  composer and the main agent, whatever pane has focus. The one-time key
  hint says so.
- **Zoom**: the focused pane fills the whole pane area until you zoom out.

A pane is not a new session or a new agent. Closing a task pane does not
stop the task; `/task-stop ID` does that, as today.

## Use cases

### UC-1: Watch a task while you keep working

Ask the agent to delegate an investigation. The task line under the `task`
call shows a quiet `›`. Click the line. A task pane opens to the right;
main keeps about 60% of the width. The pane streams the child's text, tool
calls, and tool progress. Keep typing to the main agent.

- [ ] Clicking a task line, choosing "Open task" from its right-click menu,
  or typing a bare `/task ID` opens that task's pane and focuses it. If the
  pane is open, the same action focuses it. Only a bare `/task ID` does
  this: `/task ID continue|done|discard` keep today's behavior and do not
  open a pane or change focus. No pane ever opens by itself.
- [ ] The title row shows task ID, subagent, state, model calls, and spend,
  and updates as the task runs. Below it, the pane shows the facts `/task ID`
  shows today (parent call, child Turn, limits, stop, approvals), then the
  child's conversation in the same style as main.
- [ ] Child text, tool rows, spinners, and tool progress stream live. A
  child turn started by `task_send` continues in the same pane.
- [ ] Each step shows once. Opening a pane mid-task shows the recorded steps
  so far, then live events, with no gap and no duplicates.
- [ ] Typing, Enter, steering, and queueing go to the main agent while any
  pane has focus. The draft is never lost by opening, closing, or focusing.
- [ ] When the task ends, its pane stays. The title row shows the final
  state until the developer closes the pane.

### UC-2: Watch several tasks and answer an approval

Two tasks run. Open both. The second stacks under the first in the right
column. One task asks to edit a file.

- [ ] Further side panes stack as rows in the right column. The details pane
  is the bottom row when open.
- [ ] A task that waits for the developer turns its title row (or tab) to
  the warning color and says "needs approval". The dialog stays in the
  full-width footer, labeled with the task, exactly as today.
- [ ] If the right column has no room for another pane (each row needs
  about eight lines), the oldest ended task pane closes to make room, and a
  hint names it. A focused pane or a pane that needs approval never closes
  this way. If no pane can close, the request says to close one first.

### UC-3: Move between panes and arrange them

- [ ] Mouse: click a pane to focus it; the wheel scrolls the pane under the
  pointer; drag the line between panes to resize; right-click a title row
  for Zoom, Equalize, and Close.
- [ ] Keys: Alt+arrows move focus to the neighboring pane; Alt+Z zooms or
  unzooms the focused pane; Alt+W closes the focused side pane. PgUp, PgDn,
  and Ctrl+End act on the focused pane. Esc keeps every current meaning.
- [ ] `/pane close`, `/pane zoom`, and `/pane equalize` do the same as the
  menu. `/pane focus main|ID|details` and `/pane next` move focus. `/help`
  lists the commands and keys. The first side pane in a session shows a
  one-time hint with the keys.
- [ ] The main pane cannot be closed. Closing the focused pane moves focus to
  main. Equalize restores main to about 60% and evens the right column.
- [ ] Where a terminal does not send Alt+arrows or Alt+letters (for example,
  macOS Option without "Option as Meta"), mouse and `/pane` give every
  action, including with mouse capture off. The guide says so.

### UC-4: Look at details without losing your place

Type `/tasks`, or right-click a long tool row and choose "Open in side
pane".

- [ ] `/tasks`, `/tasks ID`, `/status`, `/settings`, and "Open in side
  pane" (on tool rows and diffs) show their content in the details pane and
  focus it. The main transcript keeps its scroll position.
- [ ] Each new request replaces the details pane's content. There is never
  more than one details pane.

### UC-5: Select and copy from any pane

- [ ] Drag, double-click, and triple-click select text in the pane under the
  pointer. Starting a selection in one pane clears a selection in another.
- [ ] Ctrl+C with the composer active copies the selection, from whatever
  pane holds it, with the same fidelity rules as main today. Without a
  selection, Ctrl+C keeps its interrupt and exit meanings.

### UC-6: Narrow terminals, resize, and modes

- [ ] Below 120 columns, the split folds to the focused pane. A one-line tab
  strip (`main · t_k3f2 · details`) shows above it. Click a tab or press
  Alt+←/→ or Alt+↑/↓ to switch. Tabs that do not fit collapse to `+N`,
  which opens a menu of panes.
- [ ] At 80×24 and 40×16, with one or more panes, the composer, a pending
  dialog, and the status line stay reachable without overlap. Empty,
  streaming, approval, ended, and error states stay usable.
- [ ] Resizing, including across the 120-column line, keeps the draft, each
  pane's scroll position, follow mode, focus, and the open panes.
- [ ] With NO_COLOR, the focused pane is still clear (a `▸` mark on its title
  or tab), and "needs approval" is spelled out.
- [ ] With only the main pane, the screen is identical to today at every
  size, except the faint `›` after task lines: no title row, no gutter, no
  tab strip.
- [ ] Inline and plain modes do not change. `/task ID` there prints today's
  output. `/pane` there says panes need fullscreen mode.

### UC-7: Reopen a session with a running task

Quit while a task runs, reopen, and open its pane.

- [ ] The pane shows the recorded child Turns first, then continues live.
  No step shows twice. Each launch starts with only the main pane.
- [ ] `/new` or switching sessions closes every side pane.

## Requirements

- R-1: Panes exist only in fullscreen mode.
- R-2: The composer, status line, hints, and dialogs stay full width below
  the panes. They belong to the session, not to a pane.
- R-3: Pane colors come only from the roles in `theme.go`: the one blue
  accent for focus, warning only for "needs you", muted otherwise.
- R-4: Opening, watching, or closing a pane sends no model request, writes
  no record, and does not change a task's state or notice delivery.
- R-5: No Dashi library change. If one becomes necessary, the behavior goes
  in the library for every client, not in CLI plumbing. The build depends
  on Wonton v0.13.0, which decodes legacy `ESC ESC [D` as Alt+←
  ([wonton#227](https://github.com/deepnoodle-ai/wonton/pull/227)).

## Not in scope

- Task sidebar listing every task. The task lines and `/tasks` cover
  discovery for now; a sidebar is a separate layout question.
- Live workspace diff pane. It needs a diff source that updates during a
  turn; this track only shows recorded output.
- Remembered layout. Each launch starts with main only; persistence waits
  for evidence that people keep the same layout.
- Typing to a task from its pane. Steering a task is `task_send`'s job.
- Panes in inline or plain mode. Native scrollback cannot split.

## Decisions

The use cases state each choice; these are the rejected options.

- Shared composer. Rejected: a composer per pane, which makes focus decide
  who receives a prompt.
- Open on request only. Rejected: auto-open on task start, which moves the
  conversation without consent.
- Task panes reuse the main Renderer. Rejected: a compact task-only view, a
  second visual language.
- One reused details pane. Rejected: a pane per inspection.
- Main left at about 60%, side panes as rows. Rejected: free splitting in
  any direction, which needs layout commands this job does not need.
- Title rows and a faint gutter, only with two or more panes. Rejected: box
  borders, which cost width and leak into selections.
- Alt chords and `/pane`. Rejected: a prefix key (tmux owns Ctrl+B) and any
  new meaning for Esc.
- Fold to tabs below 120 columns. Rejected: two columns with a side pane
  under about 45 cells, which wrap too much to read.
- Make room by closing an ended task pane. Rejected: closing any unfocused
  pane, which could hide a task that needs approval.
- No layout persistence this round.

## Success

A developer watches two tasks stream beside the main conversation, answers
a child approval, and keeps composing, without a modal viewer. Must not get
worse: the single-pane screen, inline and plain modes, the 80×24 and 40×16
acceptance, copy fidelity, Esc and Ctrl+C meanings, and responsiveness while
children stream.

## Risks and open questions

- Alt chords vary by terminal. The tech spec lists the known cases and one
  Wonton decoder fix. Mouse and `/pane` remain the fallback.
- Several live panes add render work. The tech spec bounds it; a PTY test
  with three streaming tasks checks it.
- No open question blocks the build.
