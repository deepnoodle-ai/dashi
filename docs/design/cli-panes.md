# CLI panes

Status: Draft · Author: Panes track · Date: 2026-10-31\
Workflow: spec, then build in parallel (stacked on the PRD PR and on the
`transcriptPane` refactor).\
Requirements: [Watch delegated tasks beside the conversation](../prds/cli-panes.md) · PRD PR: #357

## Context

The [PRD](../prds/cli-panes.md) adds side-by-side panes to the fullscreen
CLI: a main pane, one task pane per task, and one reused details pane. The
composer, status line, and dialogs stay full width below them. A separate
refactor moves the transcript out of `screen` into a `transcriptPane` (its
`Transcript`, `ViewportState`, expanded rows, and the `ViewportItems` and
`Source` methods) and gives `screen` a one-leaf `tui.SplitState` holding
`main`. This spec builds on that shape. Inline and plain modes do not
change. No library change is needed: `delegate.Config.OnChild` carries every
child event.

## Goals

- Task panes show a child's conversation through the same `Renderer` as
  main, live and after reopen, with each step shown once and none lost.
- `OnChild` stays prompt: it never renders, loads, or blocks on the loop.
- With one pane, `View()` draws today's frame, except the task-line `›`.
- Pane keys and mouse never change what Esc, Ctrl+C, typing, or a dialog do.

Non-goals: typing to a task, layout persistence, a task sidebar, a live diff
pane, and panes in inline or plain mode.

## Proposal

### Pane registry and IDs

`screen` gains a `paneSet` that owns every pane and the layout. Only the
loop touches it.

```go
type pane struct {
	id    string          // "main", "details", or "task:" + task ID
	kind  paneKind        // paneMain, paneTask, paneDetails
	view  *transcriptPane // blocks, viewport, expanded rows
	title string          // details: what it shows, e.g. "/tasks"
	task  *taskFeed       // task panes only (below)
}

type paneSet struct {
	panes  map[string]*pane
	split  tui.SplitState // the refactor's state; leaves are pane IDs
	active string         // the focused pane; "main" by default
	zoom   string         // "" or the zoomed pane
	hinted bool           // the one-time key hint has shown
}
```

Pane IDs are `SplitState` leaf IDs. A task pane's tab label is its task ID,
such as `t_k3f2`. The field is `active`, not `focus`, so it is not confused
with Wonton keyboard focus, which stays on the composer.

### Engine seam

The screen does not touch the task host's lock or loader. `Engine` gets two
methods, both CLI-internal:

```go
// WatchTask calls fn with each child event of task id until unwatch runs.
// fn runs on the child's observer goroutine and must return promptly.
func (e *Engine) WatchTask(id string, fn func(delegate.ChildEvent)) (unwatch func())

// TaskRecord returns the task's inspection snapshot and its recorded
// child Turns, as /task ID and /tasks ID load them today.
func (e *Engine) TaskRecord(ctx context.Context, id string) (taskSnapshot, []dashi.Turn, error)
```

`OnChild` keeps its status-line work. Then it looks up `id` in the host's
watch map under `host.mu` and calls the watcher, if any. An unwatched task
costs one map lookup. Closing the task host (quit, `/new`, session switch)
drops every watcher; the screen's `Reset` closes every side pane.

### Child events: from `OnChild` to a task pane

1. The screen's watcher posts `childEvent{task, event}` through its existing
   `enqueue` path (append under `pendMu`, one `flushEvent` per batch).
   Nothing renders or loads on the child goroutine.
2. `drain` on the loop routes each `childEvent` to `pane.task.apply`.
3. `taskFeed` keeps one `Renderer` per child Turn, created from the Turn's
   first step (turn number and reasoning setting come from it; no load).
   `apply` renders and applies blocks to the pane's own `Transcript`,
   invalidating only changed rows.
4. On a step with a stop, the feed loads that Turn with `TaskRecord` off the
   loop and then applies `Renderer.Footer`, as `Engine.finished` does for
   main. Live and reopened output match.

Ordering. The feed keeps `lastSeq` per child Turn, the rule `observedSeq`
uses for status lines, kept per pane so it does not suppress them.
`Step.Seq` has no holes, so:

- `Seq <= lastSeq`: already shown; drop.
- `Seq == lastSeq+1`: apply, and apply any held steps that now follow.
- `Seq > lastSeq+1`: hold it. The `dashi.Observer` contract allows
  overlapping calls on one Turn (a `Continue` before the last call drains,
  as after `/task ID continue`) to deliver out of order. If the missing
  step has not arrived by the next flush, reload the Turn through the
  replay path below.
- A preview (delta, progress) with `Seq < lastSeq` is stale; drop it. A
  `Gap` drops that message's preview text and ignores its deltas until its
  `model_call_finished` step, as `dashi.Event` documents. Main never sees
  gaps, because `Runner.Stream` does not drop.

Back-pressure: past 4,096 unapplied previews for one pane, `drain` drops
that pane's previews and records a gap. Steps are never dropped.

### Opening and recovery

Opening a task pane (click, "Open task", bare `/task ID`) runs on the loop:

1. Add the pane, mark it `replaying`, and call `WatchTask`. Live events now
   queue in the feed without applying.
2. Off the loop, call `TaskRecord` and post the result.
3. On the loop, apply the facts block (`taskDetails(snapshot)`), then each
   ended Turn's blocks (`RenderTurn`). Feed the latest Turn's recorded steps
   through a kept `Renderer`, so live events continue its call state. Set
   `lastSeq` from that Turn's last step.
4. Clear `replaying` and apply the queued events by the ordering rules.

A step reaches `OnChild` only after the recorder saved it, and the watch
starts before the load, so no step falls between record and stream. The
same path serves a task recovered after restart and one opened mid-run. A
load error shows as a warning block in the pane; live events still apply.
The facts block refreshes when a child Turn ends and when `/task ID` runs
again. Closing a pane calls `unwatch`; the task keeps running.

### Title state

The title shows task ID, subagent, state, model calls, and spend from the
latest `delegate.TaskView`, refreshed on each step and `OnTask` event.
"Needs approval" does not come from `TaskView`: an approval is not a step.
The title reads the approval broker. Today it exposes only `activeOwner`,
which `/tasks` uses for `taskApprovalPending`; add `waitingOwners()` for the
active and queued owners, and use it in `/tasks` too. Refresh on
`promptEvent` and `endPromptEvent`. A task queued behind another approval
shows the warning too.

### Layout operations

Four `paneSet` methods change the layout. Each one ends by resetting
`active` to `main` and `zoom` to `""` if they no longer name a leaf.

- **open(id)**: with only main, `split.Append(id, SplitColumns)` and set
  weights 3:2 (main about 60%). Otherwise add `id` as a row in the right
  column, above `details` if present, with the column's average weight. If
  the column cannot give every row eight lines, first close the oldest
  ended task pane that is neither active nor waiting for approval, and show
  a hint naming it. If none qualifies, do not open; the hint says to close
  a pane. Opening ends zoom and makes the new pane active. The first open
  in a session shows the key hint.
- **close(id)**: refuse `main`. `split.Remove(id)`; Wonton collapses the
  right column, so closing the last side pane restores the one-leaf layout.
  `active` moves to main.
- **zoom(id)**: toggle `zoom`. Cancel a gutter drag first
  (`HandleMouse(MouseDragCancel)`), as corral does.
- **equalize()**: `split.Equalize()`, then main back to 3:2.

`split.MinSize` is 4: a drag leaves a title and three lines. Weights are
relative, so resizes keep proportions, and each pane's `ViewportState` keeps
its offset and follow mode.

### Drawing

With one pane, `View()` draws today's tree: padding, `tui.Viewport`, footer.
With two or more and width at least 100, the transcript area is
`tui.Split(&split, paneView).GutterStyle(faintStyle).ActiveGutterStyle(accentStyle)`.
`paneView` stacks a one-line title on the padded viewport: warning when the
task waits for approval, else accent when active, else muted. Example:
`▸ t_k3f2 · explore · running · 3 model calls · $0.0123`. The `▸` marks the
active pane, so NO_COLOR keeps it. Each title is a `HitMap` area.

Zoom draws a one-leaf `SplitState` holding the zoomed pane, titled
`zoomed`, as corral does. Below 100 columns (narrow mode), the area is a
tab strip row plus the active pane alone, also a one-leaf state. Tabs are
`HitMap` areas in `split.Leaves()` order; tabs that do not fit collapse into
`+N`, which opens a `MenuState` of hidden panes. The tree, weights, and zoom
survive narrow mode, so widening restores them.

The task-line `›` is a faint decoration after the line, fullscreen only.
It is not source, so copying does not change.

### Mouse and keys

Mouse routing, in order: open menu; dialog (wheel scrolls it, as today);
`split.HandleMouse` (gutters, only when the split is drawn); title and tab
areas (left click activates; right click opens Zoom/Unzoom `Alt+Z`,
Equalize, Close `Alt+W`, with Close disabled on main); then the pane under
the pointer (`split.PaneAt`). The wheel scrolls that pane without
activating it. A press activates it, then today's transcript handling runs
on its viewport: selection, tool toggle, item menu, "lines below". A click
on a task line opens its pane; under a tool row, the hit is the row's last
rendered lines, and a click elsewhere still toggles the row. The tool and
diff item menu gains "Open in side pane"; task lines gain "Open task".

Keys: `inputKey` sees keys before the composer field. It takes Alt+arrows,
Alt+Z, and Alt+W (either case) and posts them to the loop. Wonton's text
field ignores `Alt`, so without this it would move the cursor or insert
`z`. When a dialog text field has focus (denial reason, typed answer,
dialog status input), `inputKey` swallows them and does nothing.

- Alt+arrow: end zoom, then `split.Neighbor(active, dx, dy)`. In narrow
  mode, ←/↑ and →/↓ step through tabs. With stale bounds (just after zoom
  or narrow mode), step in `Leaves()` order.
- Alt+Z, Alt+W: zoom or close the active pane. Alt+W on main does nothing.
- PgUp, PgDn, the Home/End rules, and Ctrl+End act on the active pane when
  no dialog takes them. "N lines below" describes the active pane.
- Esc: cancels running work as today; otherwise clears the selection
  wherever it is; otherwise scrolls the active pane to the bottom.
- `/pane close|zoom|equalize|next` and `/pane focus main|ID|details` act as
  above. Inline and plain modes reply that panes need fullscreen.

Decoding. Wonton decodes xterm-modified arrows (`ESC [1;3D`; see
`TestKeyDecoder_ModifiedArrowKeys`), `ESC z`, and kitty `CSI u` with Alt.
Caveats, all covered by mouse and `/pane`, and named in the guide:

- macOS Terminal and iTerm2 without "Option as Meta" send `Ω` for Option+Z
  and `∑` for Option+W. These stay ordinary text.
- Some profiles map Option+←/→ to `ESC b` / `ESC f`. Dashi ignores those.
- Legacy `ESC ESC [D` (rxvt, some iTerm2 profiles) decodes as Escape, then
  ←. See the Wonton gap.
- With mouse capture off (F2), native selection crosses pane edges.

### Selection, copy, and details

Each pane's `ViewportState` holds its own selection; starting one clears
the others. `copySelection` asks the pane that holds a selection, with
today's source rules and async result tracking. `/copy N`, `/copy all`,
`/scrollback`, and the exit transcript stay main-only.

Fullscreen `/tasks`, `/tasks ID`, `/status`, `/settings`, and "Open in side
pane" call `screen.ShowDetails(title, blocks)` through an optional UI
interface, as `ChooseTaskDetails` works today. It opens `details` if
needed, replaces its blocks, scrolls to the top, and activates it. Other
UIs keep printing notices. A bare `/task ID` in fullscreen opens the task
pane, and the `ChooseTaskDetails` viewer goes away.

### Rendering cost

Each `tui.Viewport` draws only visible rows and caches unchanged items.
Deltas from all watched tasks batch into one flush, and a frame redraws only
invalidated rows. Spinner ticks touch visible panes only. Hidden panes apply
blocks but do not draw; unwatched tasks render nothing.

## Wonton gap

One decoder fix, needed before the implementation PR merges. In
`terminal/decoder.go`, both `handleEscapeEvent` paths treat `ESC ESC` as a
lone Escape. Change: when the byte after `ESC` is `ESC` and the next is `[`
or `O`, decode the rest as a CSI or SS3 key with `Alt` set; otherwise keep
today's result. Add `ESC ESC [D` and `ESC ESC OD` to the modified-arrow
test, and release v0.12.1. Without it, Alt+← on those terminals sends
Escape, which cancels running work. Nothing else is missing.

## Alternatives and tradeoffs

- Rendering on the child goroutine needs a locked `Renderer` and string work
  where `OnChild` must return promptly. Rejected.
- A transcript for every task from its first event opens instantly but costs
  memory for unwatched tasks and still needs replay. Rejected.
- Cost accepted: a long task pane appears after its records load, and a
  pane opened mid-stream shows the current message from the open point
  until its committed step replaces it.

## Test plan

- **Unit** (`panes_test.go`): open, close, zoom, equalize, room-making, and
  `active` repair; main at 3:2; details stays the bottom row; the last close
  restores one leaf. `taskFeed`: dedup, held out-of-order steps and reload,
  stale previews, gaps, events queued during replay, a load error, a second
  child Turn. Title warning for active and queued approvals.
- **Screen frames** (`terminal_views_test.go`, `saveTerminalFrame`): one
  pane at 120×40, 80×24, and 40×16 equals the pre-change frames, except
  fixtures with task lines, which are updated on purpose for the `›`. Two
  and three panes at 120×40; tabs at 80×24 and 40×16; zoom; a waiting task;
  NO_COLOR for each.
- **Screen events** (`screen_test.go` harness): click and right-click
  routing, wheel, gutter drag, Alt keys in a dialog text field, Esc
  meanings, Ctrl+C copy from a side pane, `/pane` commands, resize across
  100 columns keeping draft, offsets, and `active`, and `Reset`.
- **PTY** (`panes_pty_test.go`, like `task_pty_test.go`): 80×24 and 120×40;
  open a pane by SGR mouse click; `ESC [1;3D` and `ESC z`; three streaming
  tasks with a keystroke echoed within the existing two-second bound; quit
  and reopen a running task; terminal restored on exit; `ESC ESC [D` after
  Wonton v0.12.1.

## Rollout

No flag; panes appear only on request. Update `docs/guides/cli-tasks.md`,
`docs/guides/cli.md` (fullscreen only, keys, macOS Option caveat,
fallbacks), `/help`, and `CHANGELOG.md`.
