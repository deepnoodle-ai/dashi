# CLI panes

Status: Draft · Author: Panes track · Date: 2026-10-31\
Workflow: spec, then build in parallel (stacked on the PRD PR and on the
`transcriptPane` refactor).\
Requirements: [Watch delegated tasks beside the conversation](../prds/cli-panes.md)

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
  main, live and after reopen, with each step shown once.
- `OnChild` stays prompt: it never renders, loads, or blocks on the loop.
- With one pane, `View()` produces the same frame as before, cell for cell.
- Pane keys and mouse never change what Esc, Ctrl+C, typing, or a dialog do.

## Non-goals

Typing to a task, layout persistence, a task sidebar, a live diff pane, and
panes in inline or plain mode.

## Proposal

### Pane registry and IDs

`screen` gains a `paneSet` that owns every pane and the layout:

```go
type paneKind int // paneMain, paneTask, paneDetails

type pane struct {
	id    string          // "main", "details", or "task:" + task ID
	kind  paneKind
	view  *transcriptPane // blocks, viewport, expanded rows
	title string          // details: what it shows, e.g. "/tasks"
	task  *taskFeed       // task panes only (below)
}

type paneSet struct {
	panes  map[string]*pane
	split  tui.SplitState // the refactor's state; leaves are pane IDs
	focus  string         // always a leaf; "main" by default
	zoom   string         // "" or the zoomed pane
	opened []string       // task panes in open order, for eviction
	hinted bool           // the one-time key hint has shown
}
```

Pane IDs are `SplitState` leaf IDs. A task pane's ID is `task:t_k3f2`; its
tab label is `t_k3f2`. Only the loop touches `paneSet`.

### Child events: from `OnChild` to a task pane

`OnChild` runs on the child's observer goroutine and must return promptly.
The task host gets a `watch` set: the task IDs whose panes are open.

1. `OnChild` keeps its status-line work. Then, under `host.mu`, it checks
   `watch[taskID]`. An unwatched task costs one map lookup. A watched one
   posts a `childEvent{task, event}` with the screen's existing `enqueue`
   path (append under `pendMu`, one `flushEvent` per batch). Nothing renders
   or loads on this goroutine.
2. `drain` on the loop routes a `childEvent` to `pane.task.apply(event)`.
3. `taskFeed` holds one `Renderer` per child Turn, created from the child's
   first `turn_started` step (turn number and reasoning setting come from
   it; no load). `apply` drops stale events, renders the rest, and applies
   the blocks to the pane's own `Transcript`, invalidating changed rows.
4. On a step with a stop, the feed schedules an async load of that child
   Turn and then applies `Renderer.Footer`, as `Engine.finished` does for
   main. Live and reopened output match.

The dedup rule is the one `observedSeq` already uses for status lines, kept
per pane so it does not suppress them: the feed keeps `lastSeq` per child
Turn. A step with `Seq <= lastSeq` is dropped. A preview (delta, progress)
with `Seq < lastSeq` is stale and dropped. A `Gap` event drops that
message's preview text and ignores its deltas until its `model_call_finished`
step, as `dashi.Event` documents; the main path never sees gaps, because
`Runner.Stream` does not drop.

Back-pressure: if a pane holds more than 4,096 unapplied previews, the loop
is behind. `drain` drops previews for that pane and records a gap. Steps are
never dropped.

### Opening and recovery

Opening a task pane (click, menu, `/task ID`) runs on the loop:

1. Add the pane, mark it `replaying`, and add the task to `watch`. From now
   on live events queue in the feed and do not apply.
2. Off the loop, load `inspectTask` facts and each recorded child Turn with
   `host.inputs.loader`, as `taskConversation` does. Post the result.
3. On the loop, apply the facts block, then the blocks of each ended Turn
   (`RenderTurn`). Feed the latest Turn's recorded steps through a kept
   `Renderer`, so live events continue its call state. Set `lastSeq` from
   that Turn's last step.
4. Clear `replaying` and apply the queued live events through the dedup
   rule. Steps already replayed drop; later ones apply.

Because the watch starts before the load, no event falls between the record
and the live stream. This path serves a task recovered after restart and a
task opened mid-run in the same process. A load error shows as a warning
block in the pane; live events still apply.

The facts block is `taskDetails(snapshot)` as one block at the top of the
pane, refreshed when a child Turn ends and when `/task ID` runs again. The
title row is drawn from the latest `delegate.TaskView`, refreshed in the
loop on each step and `OnTask` event. Closing a pane removes it from
`watch`; the task keeps running.

### Layout operations

All layout goes through four `paneSet` methods. Each one ends by checking
that `focus` and `zoom` name leaves, else resets them to `main` and `""`.

- **open(id)**: if only main exists, `split.Append(id, SplitColumns)` and set
  weights 3:2 (main about 60%). Otherwise add `id` as a row in the right
  column, above `details` if present, with the column's average weight
  (edit the right branch directly). If the column cannot give every row
  four lines plus a gutter, first close the oldest task pane in `opened`
  that lacks focus, and show a hint naming it. Opening ends zoom and focuses
  the new pane. The first open in a session shows the key hint once.
- **close(id)**: refuse `main`. `split.Remove(id)`; Wonton collapses the
  right column, so closing the last side pane leaves the one-leaf layout.
  Focus moves to main.
- **zoom(id)**: toggle `zoom`. A drag in progress is cancelled first
  (`HandleMouse(MouseDragCancel)`), as corral does.
- **equalize()**: `split.Equalize()`, then main back to 3:2.

`split.MinSize` is 4, so a drag leaves each pane a title row and three lines.
Weights are relative, so terminal resizes keep proportions. Each pane's
`ViewportState` keeps its scroll offset and follow mode across resizes.

### Drawing

`View()` checks the pane count first. With one pane, it draws exactly
today's tree: padding, `tui.Viewport(main)`, footer. No title, no gutter.

With two or more panes and width at least 100, the transcript area is
`tui.Split(&split, paneView).GutterStyle(faintStyle).ActiveGutterStyle(accentStyle)`.
`paneView` stacks a one-line title row on the pane's padded viewport. The
title is accent when focused, warning when the task waits for the developer
(`taskApprovalPending`), else muted. Text: `▸ t_k3f2 · explore · running ·
3 model calls · $0.0123`; the `▸` marks focus, so NO_COLOR keeps it. Each
title is a `HitMap` area `title:<id>`.

When zoomed, the area is a one-leaf `SplitState` holding the zoomed pane,
with its title showing `zoomed`, the pattern corral uses.

Below 100 columns (narrow mode), the area is a tab strip row plus the
focused pane alone (also a one-leaf state). Tabs are `HitMap` areas
`tab:<id>`, in `split.Leaves()` order. Tabs that do not fit collapse into a
`+N` tab that opens a `MenuState` listing the hidden panes. The split tree,
weights, and zoom survive narrow mode untouched, so widening restores them.

The task line affordance is a faint `›` drawn after the task line in
fullscreen only. It is decoration, not source, so copying does not change.

### Focus, mouse, and keys

Mouse routing, in order: open menu; dialog (wheel scrolls the dialog, as
today); `split.HandleMouse` (gutter drag and hover, only when the split is
drawn); title and tab areas (left click focuses, right click opens the pane
menu: Zoom/Unzoom `Alt+Z`, Equalize, Close `Alt+W`, Close disabled for
main); then the pane under the pointer, found with `split.PaneAt`. A wheel
scrolls that pane without moving focus. A press focuses it, then today's
transcript handling runs on that pane's viewport: selection, tool toggling,
the item menu, and the "lines below" hit. A click on a task line opens its
pane. For a task line drawn under a tool row, the hit is the row's last
rendered lines (measured at render); a click elsewhere on the row still
toggles it. The tool and diff item menu gains "Open in side pane". Task
lines gain "Open task".

Keys: `inputKey` already consumes keys before the composer field sees them.
It also consumes Alt+arrows, Alt+Z, and Alt+W (either case) and posts them
to the loop, unless a dialog text field (denial reason, typed answer,
dialog status input) has focus. This matters: Wonton's text field ignores
`Alt` and would move the cursor or insert `z`. On the loop:

- Alt+arrow: end zoom if set, then `split.Neighbor(focus, dx, dy)`. In
  narrow mode, Alt+←/↑ and Alt+→/↓ step through tabs in order. If bounds are
  stale (just after zoom or narrow mode), step in `Leaves()` order.
- Alt+Z, Alt+W: zoom and close the focused pane. Alt+W on main does nothing.
- PgUp, PgDn, Home/End rules, and Ctrl+End act on the focused pane's
  viewport when no dialog has the keys. The "N lines below" hint describes
  the focused pane.
- Esc: running work is cancelled as today. Otherwise it clears the
  selection wherever it is, else scrolls the focused pane to the bottom.
- `/pane close|zoom|equalize` call the same methods on the focused pane.
  In inline and plain modes `/pane` reports that panes need fullscreen.

Decoding. Wonton decodes Alt+arrows sent as xterm modifiers (`ESC [1;3D`;
tested in `TestKeyDecoder_ModifiedArrowKeys`), Alt+letters sent as `ESC z`, and both as kitty
`CSI u` when the enhanced keyboard is on. Known caveats, all covered by mouse
and `/pane`, and named in the guide:

- macOS Terminal and iTerm2 without "Option as Meta" (Esc+) send `Ω` for
  Option+Z and `∑` for Option+W. These are ordinary text.
- Some profiles map Option+←/→ to word movement (`ESC b`, `ESC f`). Dashi
  does not treat those as pane keys.
- Terminals that send the legacy prefixed form `ESC ESC [D` (rxvt, some
  iTerm2 profiles) decode as Escape and then ←. See the Wonton gap.

### Selection and copy

Each pane's `ViewportState` holds its own selection. A press that starts a
selection in one pane clears every other pane's. `copySelection` asks the
pane that holds a selection, with today's source rules, async result
tracking, and `selectionRevision`. `/copy N`, `/copy all`, `/scrollback`,
and the transcript printed at exit stay main-only.

### Details pane

Fullscreen `/tasks`, `/tasks ID`, `/status`, `/settings`, and "Open in side
pane" call `screen.ShowDetails(title, blocks)`. It opens `details` if needed,
replaces its blocks, scrolls it to the top, and focuses it. The Controller
reaches it through an optional UI interface, as `ChooseTaskDetails` works
today; other UIs keep printing notices. `/task ID` in fullscreen opens the
task pane instead of the `ChooseTaskDetails` viewer, which goes away.

### Rendering cost

Each pane is a `tui.Viewport` that measures and draws only visible rows and
caches unchanged items. Deltas from all watched tasks batch into one
`flushEvent`; one frame redraws only invalidated rows. Spinner ticks
invalidate running tool rows in visible panes only. Hidden panes (narrow
mode, zoom) still apply blocks but do not draw. Unwatched tasks render
nothing. Expected cost: four streaming panes cost about four times one
streaming main transcript, inside the same frame rate. The PTY test below
measures it.

### Reset

`Reset` (for `/new` and session switch) closes every side pane, clears
`watch`, and resets main as today.

## Wonton gap

One small decoder fix, needed before the implementation PR merges. In
`terminal/decoder.go`, both `handleEscapeEvent` paths treat `ESC` followed by
`ESC` as a lone Escape. Change: when the byte after `ESC` is `ESC` and the
next byte is `[` or `O`, decode the rest as a CSI or SS3 key and set
`Alt`; otherwise keep today's result. Add `ESC ESC [D` and `ESC ESC OD`
cases to the Alt arrow test. Release as v0.12.1. Without it, Alt+← on those
terminals sends Escape, which cancels running work.

Nothing else is missing. Zoom and narrow mode use one-leaf states, title
rows and tabs use `HitMap`, and `Neighbor`, `Remove`, and `Equalize` exist.

## Alternatives considered

- **Render child events on the child goroutine.** It would keep the loop
  lighter, but each `Renderer` would need locking, and `OnChild` would do
  string work on a goroutine that must return promptly. Rejected.
- **Keep a transcript for every task from its first event.** Opening would
  be instant, but memory grows with tasks nobody watches, and recovered
  tasks still need the replay path. Rejected for one replay path.
- **Box borders around panes.** Clear edges, but two columns lost per pane
  and borders inside selections. Rejected by the PRD.

## Tradeoffs

- Opening a long task pane loads its records; the pane shows them after
  the load, not instantly.
- A pane opened mid-stream starts the current message's text from the open
  point; the next committed step replaces it with the full text.
- Focus adds a second meaning to PgUp/PgDn and Esc's scroll fallback. The
  `▸` mark and title colors make the target visible.
- Eviction may close a pane the developer meant to keep. The hint names it,
  and the task line reopens it.

## Test plan

- **Unit** (`panes_test.go`): open, close, zoom, equalize, eviction, and
  focus repair on `paneSet`; main at 3:2; details stays the bottom row;
  closing the last side pane leaves a one-leaf tree. `taskFeed`: step and
  preview dedup at `lastSeq`, gaps, queued live events during replay, a load
  error, and a second child Turn.
- **Screen frames** (`terminal_views_test.go` fixtures, `saveTerminalFrame`):
  one pane at 120×40, 80×24, and 40×16 equals the pre-change frames; two and
  three panes at 120×40; narrow tabs at 80×24 and 40×16; zoom; a waiting
  task title; NO_COLOR for each.
- **Screen events** (`screen_test.go` harness): click and right-click
  routing, wheel to the pane under the pointer, gutter drag, Alt keys while
  a dialog text field has focus, Esc meanings, Ctrl+C copy from a side pane,
  resize across 100 columns keeping draft, scroll, and focus, and `Reset`.
- **PTY** (`panes_pty_test.go`, in the style of `task_pty_test.go`): real
  80×24 and 120×40 terminals; open a task pane by SGR mouse click; Alt keys
  as `ESC [1;3D` and `ESC z`; three streaming tasks with a keystroke echoed
  within the existing two-second bound; quit and reopen a running task;
  terminal restored on exit. After Wonton v0.12.1, `ESC ESC [D`.

## Rollout

No flag. Panes appear only on request, so the default screen is unchanged.
Update `docs/guides/cli-tasks.md` and `docs/guides/cli.md` (panes are
fullscreen only, the keys, the macOS Option caveat, the fallbacks), `/help`,
and `CHANGELOG.md`.

## Open questions

None that change the design.
