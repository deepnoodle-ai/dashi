# CLI panes

Status: In review · Date: 2026-10-31 · PRD PR: #357\
Requirements: [Watch delegated tasks beside the conversation](../prds/cli-panes.md)

## Context

The [PRD](../prds/cli-panes.md) adds side-by-side panes to the fullscreen
CLI: main, one pane per task, and one reused details pane. A separate
refactor moves the transcript into a `transcriptPane` and gives `screen` a
one-leaf `tui.SplitState` holding `main`; this spec builds on it. No Dashi
library change is needed: `delegate.Config.OnChild` carries every child
event. One Wonton decoder fix is needed (below).

## Goals

- Task panes use main's `Renderer`; each step shows once and none is lost.
- `OnChild` never renders, loads, or blocks on the loop.
- One pane draws today's frame, except the task-line hint, the lines-below
  mark, and the copy hint (see "Stable pane area").
- Pointer actions never change the height of the pane area.

## Proposal

### Pane registry

`screen` gains a `paneSet` that owns every pane and the layout. Only the
loop touches it. A `pane` has an ID (`main`, `details`, or `task:` plus the
task ID), a kind, a `transcriptPane` (blocks, viewport, expanded rows), a
label and title for its title row, and, for a task pane, its `taskFeed`. The
`paneSet` holds the panes by ID, the `tui.SplitState` whose leaves are pane
IDs, the active pane (`main` by default), the zoomed pane, and whether the
one-time key hint has shown.

### Engine seam

The screen does not touch the task host's lock or loader. `Engine` gets
`WatchTask(id, fn) (unwatch func())`, which calls `fn` on the child's
observer goroutine with each child event, and `TaskRecord(ctx, id)`, which
returns the inspection snapshot and recorded child Turns as `/tasks ID`
loads them.

`OnChild` keeps its status-line work, reads the task's watcher from the
host's watch map under `host.mu`, and calls it after releasing the lock
(`enqueue` can block, so calling under the lock could deadlock). Closing the task host (quit, `/new`,
session switch) drops every watcher; `Reset` closes every side pane.

### Task feed

1. The watcher posts `childEvent{task, event}` through the screen's
   `enqueue` path (one `flushEvent` per batch).
2. `drain` on the loop routes each `childEvent` to `pane.task.apply`.
3. `taskFeed` keeps one `Renderer` per child Turn, created from the Turn's
   first step (turn number and reasoning setting; no load). `apply` renders
   into the pane's own `Transcript`, invalidating only changed rows.
4. On a step with a stop, the feed builds the footer from the Turn's steps it
   kept (`dashi.Turn{ID, Steps}.View()`) and applies `Renderer.Footer`, as
   `Engine.finished` does for main, so the footer lands before the next
   Turn's blocks. It also starts a facts-only load.

Ordering. The feed keeps `lastSeq` per child Turn, as `observedSeq` does
for status lines, but per pane so it does not suppress them. `Step.Seq` has
no holes, so for steps:

- `Seq <= lastSeq`: already shown; drop.
- `Seq == lastSeq+1`: apply, then apply held events that now follow.
- `Seq > lastSeq+1`: hold it. Overlapping observer calls on one Turn (a
  `Continue` before the last call drains) can deliver out of order, and a
  step whose commit outcome is unknown is only in the record, never in
  `OnChild`. If the missing step has not arrived within `taskGapWait`
  (100 ms of clock time, not a count of flushes), reload the Turn: call `TaskRecord` off the loop with the pane's token, and keep
  holding events. On success, drop that Turn's blocks, make a fresh
  `Renderer`, and replay its recorded steps as in opening step 3. Never
  feed recorded steps into a `Renderer` that has already rendered them. A
  failed reload keeps the Turn's blocks and follows the load-failure rule
  below from the first held step.

Previews (delta, progress) follow the same order. A preview with
`Seq > lastSeq` is held with the steps and applies once `lastSeq` reaches
it. A preview at or below `lastSeq` is dropped if it is stale (its `Seq`
is below `lastSeq`, or its message's step has committed). A `Gap` drops
that message's preview text and ignores its deltas until its
`model_call_finished` step, as `dashi.Event` documents. Past 4,096
unapplied previews for one pane, `drain` drops that pane's previews and
records a gap. Steps are never dropped.

### Opening and recovery

Opening a task pane (click, "Open task", bare `/task ID`) runs on the loop:

1. Add the pane with a new open token, mark it `replaying`, and call
   `WatchTask`. Live events queue.
2. Off the loop, call `TaskRecord` and post the result with the token.
3. On the loop, drop the result if the pane is gone or its token differs.
   Otherwise apply the header line (the task's description), then each
   ended Turn's blocks (`RenderTurn`). Feed the latest Turn's recorded
   steps through a fresh `Renderer` that the feed keeps, so live events
   continue its call state. Set `lastSeq` from that Turn's last step.
4. Clear `replaying` and apply queued events by the ordering rules.

A step reaches `OnChild` only after the recorder saved it, and the watch
starts before the load, so no step falls between record and stream. The
same path serves a task recovered after restart and one opened mid-run.

If an open or a reload fails to load, the pane shows a warning block that
names the error and marks the gap. The feed sets `lastSeq` to one before
the first held or next live step and applies from there; it does not retry
the load. After a reload, a step still held past the record counts as
lost only if the record ends before the last step the reload asked for;
otherwise it is a new gap and waits again. A reload also replays later
Turns from the record and reapplies their steps already shown live. The
header refreshes when a child Turn ends and when `/task ID`
runs again. The full facts (`taskDetailFacts`) show in the details pane for
`/tasks ID` and the title menu's "Task details". Closing a pane calls `unwatch` and drops its queued
events; a later load result finds no pane and is dropped. The task keeps
running.

### Title state

The title reads task ID · state · spend · model calls · subagent; tool
calls stay on the task line. It is read from the task's status lines as they pass through `drain` (the loop never
takes the task host's locks), so it refreshes on each step and `OnTask`
event.
"Needs approval" comes from the approval broker, not `TaskView`. Add
`waitingOwners()` (active and queued owners) beside `activeOwner`, use it
in `/tasks` too, and refresh on `promptEvent` and `endPromptEvent`.

### Layout operations

Each `paneSet` operation ends by resetting `active` to `main` and `zoom` to
`""` if they no longer name a leaf.

- **open(id)**: with only main, `split.Append(id, SplitColumns)`, weights
  3:2. Otherwise add `id` as a row in the right column, above `details` if
  present, with the column's average weight. If the column cannot give
  every row eight lines, first close the oldest ended task pane that is
  neither active nor waiting for approval, with a hint naming it. If none
  qualifies, do not open; the hint says to close a pane. Opening ends zoom
  and activates the pane. The first open in a session shows the key hint.
- **close(id)**: refuse `main`. `split.Remove(id)`; closing the last side
  pane restores one leaf. `active` moves to main.
- **zoom(id)**: toggle `zoom`; cancel a gutter drag first
  (`HandleMouse(MouseDragCancel)`), as corral does.
- **equalize()**: `split.Equalize()`, then main back to 3:2.

`split.MinSize` is 4: a title and three lines. Each `ViewportState` keeps
its offset and follow mode across resizes.

### Drawing

With one pane, `View()` draws today's tree. With two or more and width at
least 120, the transcript area is
`tui.Split(&split, paneView).GutterStyle(faintStyle).ActiveGutterStyle(accentStyle)`;
at 120 columns, 3:2 leaves the side pane about 47 cells. `paneView` stacks a
one-line title on the padded viewport, such as
`▸ t_k3f2 · running · $0.0123 · 3 model calls · explore ×`: "needs
approval" replaces the state; a zoomed title ends `zoomed · Alt+Z`, kept
when the rest is cut; side panes end with a `×` (its cells recorded on the
pane) that closes them; the details title adds the capture time. Warning
color when the task waits for approval, else accent when active, else
muted. The `▸` marks the active pane, so NO_COLOR keeps it. A row with
another row under it keeps its last row blank, so a turn rule never sits on
the row separator, and a task pane's turn rules read `t_k3f2 turn 1`.

Zoom draws a one-leaf `SplitState` holding the zoomed pane. Below 120
columns (narrow mode), the area is a tab strip plus the active pane alone.
Tabs are `HitMap` areas in `split.Leaves()` order, padded with spaces; the
active tab is reverse video, the others muted, and the active pane's title
fills the right of the strip. Tabs that do not fit collapse into `+N`,
which opens a `MenuState` of hidden panes. While another pane is active,
the composer band shows a muted `→ main`. The tree,
weights, and zoom survive narrow mode.

In fullscreen, `transcriptPane.liveBlock` replaces a task line's inspect hint
with "click to watch", or "watching" while its pane is open; opening or
closing a task pane redraws that task's lines. The record and inline output
keep the inspect hint; task lines start with the task ID in every mode.
The first task line of a session adds a one-time muted hint naming its ID.

**Stable pane area.** Pointer actions, focus changes, and selections never
add or remove a footer row, since rows that come and go mid-drag move the
panes under the pointer. Each pane draws its lines-below mark (`↓ 12 more`,
plus `· Ctrl+End` on the active pane) over its viewport's bottom row, in a
`ZStack` canvas that reads the viewport after it draws; a press on the mark
returns that pane to live output. The copy hint replaces `/help` on the
status line, in a short form when the long one does not fit. While two or
more panes sit side by side, the terminal reports pointer motion (`?1003`)
so the gutter highlights on hover; otherwise it reports drags only.

### Mouse and keys

Mouse routing, in order: open menu; while a dialog is open, the wheel off
the panes scrolls its details, and every other pointer event continues, so
panes, task lines, and menus work during an approval (a dialog's status
field still blocks menus); `split.HandleMouse` (gutters); a title's `×`;
title and tab areas (left click activates; right click
opens Zoom/Unzoom, Equalize, Close, with Close disabled on main); then the
pane under the pointer (`split.PaneAt`). The wheel scrolls that pane
without activating it. A press activates it, then runs today's transcript
handling on it. A click on a task line opens its pane; under a tool row,
the hit is the row's last rendered lines. Tool and diff item menus gain
"Show in details"; task lines gain "Open task".

`inputKey` takes Alt+arrows, Alt+Z, and Alt+W (either case) before the
composer field and posts them to the loop. While any dialog is open, they
are swallowed and do nothing.

- Alt+arrow: end zoom, then `split.Neighbor(active, dx, dy)`. In narrow
  mode, Alt+←/↑ and Alt+→/↓ step through tabs. With stale bounds, step in
  `Leaves()` order.
- Alt+Z, Alt+W: zoom or close the active pane. Alt+W on main does nothing.
- PgUp, PgDn, Home/End, and Ctrl+End use the active pane; each pane has
  its own lines-below mark.
- Esc: cancels running work as today; otherwise clears the selection;
  otherwise scrolls the active pane to the bottom.
- `/pane close|zoom|equalize|next` and `/pane focus main|ID|details` act as
  above; a bare `/pane` lists the panes as a hint, and Tab completes
  actions and names. Inline and plain modes reply that panes need
  fullscreen and leave `/pane` out of help and completion.

Caveats, covered by mouse and `/pane` and named in the guide: macOS Option
without "Option as Meta" sends `Ω` and `∑` (ordinary text); some profiles
map Option+←/→ to `ESC b` / `ESC f` (ignored); with mouse capture off (F2),
native selection crosses pane edges.

### Selection, copy, and details

Each pane's `ViewportState` holds its own selection; starting one clears
the others, and `copySelection` asks the pane that holds it. `/copy N`, `/copy all`, `/scrollback`, and the exit
transcript stay main-only.

Fullscreen `/tasks`, `/tasks ID`, `/status`, `/settings`, `/help`, "Show in
details" (finished tool rows, expanded, and diffs), and "Task details" call
`screen.ShowDetails(detailsUpdate)` through the optional `panesUI`
interface (`Controller.showDetails`). Inline mode runs on the same screen
with `screen.inline` set, so `Panes()` reports false and the Controller
prints instead. It opens `details` if needed, replaces
its blocks, scrolls to the top, and activates it. An update with `add` (the
spend after the `/tasks` list) appends only while the pane still shows the
same title. Without room, the blocks print in main. Other UIs keep printing
notices; a command with nothing to show answers in main. Only a bare
`/task ID` opens or activates the task pane; `/task ID continue|done|discard`
keep today's behavior and do not change `active`. The `ChooseTaskDetails`
viewer stays for inline mode and for tasks the session does not list.

## Wonton gap

[wonton#227](https://github.com/deepnoodle-ai/wonton/pull/227) (v0.13.0)
decodes `ESC ESC [D` (rxvt, some iTerm2 profiles) as Alt+←, not Escape then
←, which would cancel running work.

## Alternatives and tradeoffs

- Rejected: rendering on the child goroutine, and a transcript for every
  task from its first event (memory for unwatched tasks; still needs
  replay).
- Cost accepted: a long task pane appears after its records load, and a
  pane opened mid-stream shows the current message from the open point.
  Viewports draw only visible, changed rows; hidden panes do not draw.

## Test plan

- **Unit**: layout operations and `active` repair; every `taskFeed` rule
  above, including a failed open load and a failed reload; approval titles.
- **Frames**: one pane equals today's frames at 120×40, 80×24, and 40×16,
  except the `›`; two and three panes, tabs, zoom, approval, NO_COLOR.
- **Events and PTY**: mouse routing, Alt keys (with `ESC ESC [D`), Esc,
  side-pane copy, `/pane`, resize across 120 columns, three streaming
  tasks with keystroke echo under two seconds, reopen after restart.

## Rollout

No flag; panes appear only on request. Docs: `docs/guides/cli.md` (Panes),
`docs/guides/cli-tasks.md`, `/help`, and `CHANGELOG.md`.
