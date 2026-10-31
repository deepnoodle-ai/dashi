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
- One pane draws today's frame, except the task-line `›`.

## Proposal

### Pane registry

`screen` gains a `paneSet` that owns every pane and the layout. Only the
loop touches it.

```go
type pane struct {
	id    string          // "main", "details", or "task:" + task ID
	kind  paneKind        // paneMain, paneTask, paneDetails
	view  *transcriptPane // blocks, viewport, expanded rows
	title string          // details: what it shows, e.g. "/tasks"
	task  *taskFeed       // task panes only
}

type paneSet struct {
	panes  map[string]*pane
	split  tui.SplitState // leaves are pane IDs
	active string         // the focused pane; "main" by default
	zoom   string         // "" or the zoomed pane
	hinted bool           // the one-time key hint has shown
}
```

### Engine seam

The screen does not touch the task host's lock or loader. `Engine` gets:

```go
// WatchTask calls fn with each child event of task id until unwatch runs.
// fn runs on the child's observer goroutine and must return promptly.
func (e *Engine) WatchTask(id string, fn func(delegate.ChildEvent)) (unwatch func())

// TaskRecord returns the task's inspection snapshot and its recorded
// child Turns, as /task ID and /tasks ID load them today.
func (e *Engine) TaskRecord(ctx context.Context, id string) (taskSnapshot, []dashi.Turn, error)
```

`OnChild` keeps its status-line work, then calls the task's watcher from
the host's watch map under `host.mu`. Closing the task host (quit, `/new`,
session switch) drops every watcher; `Reset` closes every side pane.

### Task feed

1. The watcher posts `childEvent{task, event}` through the screen's
   `enqueue` path (one `flushEvent` per batch).
2. `drain` on the loop routes each `childEvent` to `pane.task.apply`.
3. `taskFeed` keeps one `Renderer` per child Turn, created from the Turn's
   first step (turn number and reasoning setting; no load). `apply` renders
   into the pane's own `Transcript`, invalidating only changed rows.
4. On a step with a stop, the feed loads that Turn with `TaskRecord` off the
   loop, then applies `Renderer.Footer`, as `Engine.finished` does for main.

Ordering. The feed keeps `lastSeq` per child Turn, as `observedSeq` does
for status lines, but per pane so it does not suppress them. `Step.Seq` has
no holes, so for steps:

- `Seq <= lastSeq`: already shown; drop.
- `Seq == lastSeq+1`: apply, then apply held events that now follow.
- `Seq > lastSeq+1`: hold it. Overlapping observer calls on one Turn (a
  `Continue` before the last call drains) can deliver out of order, and a
  step whose commit outcome is unknown is only in the record, never in
  `OnChild`. If the missing step has not arrived by the next flush, reload
  the Turn: call `TaskRecord` off the loop with the pane's token, and keep
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
   Otherwise apply the facts block (`taskDetails(snapshot)`), then each
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
the load. The facts block refreshes when a child Turn ends and when
`/task ID` runs again. Closing a pane calls `unwatch` and drops its queued
events; a later load result finds no pane and is dropped. The task keeps
running.

### Title state

The title shows task ID, subagent, state, model calls, and spend from the
latest `delegate.TaskView`, refreshed on each step and `OnTask` event.
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
one-line `HitMap` title on the padded viewport, such as
`▸ t_k3f2 · explore · running · 3 model calls · $0.0123`: warning color when
the task waits for approval, else accent when active, else muted. The `▸`
marks the active pane, so NO_COLOR keeps it.

Zoom draws a one-leaf `SplitState` holding the zoomed pane. Below 120
columns (narrow mode), the area is a tab strip plus the active pane alone.
Tabs are `HitMap` areas in `split.Leaves()` order; tabs that do not fit
collapse into `+N`, which opens a `MenuState` of hidden panes. The tree,
weights, and zoom survive narrow mode.

The task-line `›` is a faint fullscreen decoration, not source, so copying
does not change.

### Mouse and keys

Mouse routing, in order: open menu; dialog; `split.HandleMouse` (gutters);
title and tab areas (left click activates; right click
opens Zoom/Unzoom, Equalize, Close, with Close disabled on main); then the
pane under the pointer (`split.PaneAt`). The wheel scrolls that pane
without activating it. A press activates it, then runs today's transcript
handling on it. A click on a task line opens its pane; under a tool row,
the hit is the row's last rendered lines. Tool and diff item menus gain
"Open in side pane"; task lines gain "Open task".

`inputKey` takes Alt+arrows, Alt+Z, and Alt+W (either case) before the
composer field and posts them to the loop. When a dialog text field has
focus, it swallows them and does nothing.

- Alt+arrow: end zoom, then `split.Neighbor(active, dx, dy)`. In narrow
  mode, Alt+←/↑ and Alt+→/↓ step through tabs. With stale bounds, step in
  `Leaves()` order.
- Alt+Z, Alt+W: zoom or close the active pane. Alt+W on main does nothing.
- PgUp, PgDn, Home/End, Ctrl+End, and "N lines below" use the active pane.
- Esc: cancels running work as today; otherwise clears the selection;
  otherwise scrolls the active pane to the bottom.
- `/pane close|zoom|equalize|next` and `/pane focus main|ID|details` act as
  above. Inline and plain modes reply that panes need fullscreen.

Wonton decodes xterm-modified arrows, `ESC z`, and kitty `CSI u` with
Alt. Caveats, covered by mouse and `/pane` and named in the guide: macOS
Option without "Option as Meta" sends `Ω` and `∑` (ordinary text); some
profiles map Option+←/→ to `ESC b` / `ESC f` (ignored); legacy
`ESC ESC [D` needs the Wonton fix; with mouse capture off (F2), native
selection crosses pane edges.

### Selection, copy, and details

Each pane's `ViewportState` holds its own selection; starting one clears
the others, and `copySelection` asks the pane that holds it. `/copy N`, `/copy all`, `/scrollback`, and the exit
transcript stay main-only.

Fullscreen `/tasks`, `/tasks ID`, `/status`, `/settings`, and "Open in side
pane" call `screen.ShowDetails(title, blocks)` through an optional UI
interface. It opens `details` if needed, replaces its blocks, scrolls to
the top, and activates it. Other UIs keep printing notices. Only a bare
`/task ID` opens or activates the task pane; `/task ID continue|done|discard`
keep today's behavior and do not change `active`. The `ChooseTaskDetails`
viewer goes away.

## Wonton gap

[wonton#227](https://github.com/deepnoodle-ai/wonton/pull/227) has merged;
v0.12.1 must be released before the implementation merges. Without it,
`ESC ESC [D` (rxvt, some iTerm2 profiles) decodes as Escape, then ←, so
Alt+← would cancel running work. The fix decodes `ESC ESC` plus a complete
CSI or SS3 key as that key with `Alt`, only when the bytes arrive together
or within the 50 ms escape timeout. A person pressing Esc, then ←, sends
separate reads further apart, so Esc still cancels.

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

No flag; panes appear only on request. Update `docs/guides/cli-tasks.md`,
`docs/guides/cli.md` (keys, macOS Option caveat, fallbacks), `/help`, and
`CHANGELOG.md`.
