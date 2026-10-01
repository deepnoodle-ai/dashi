# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Changed

- **A `Runner` needs a `Client` only to fill in an agent or call the model.**
  A Start with an already-closed `WithPause` records ("admits") a turn without
  a `Client` when its agent names its provider, API, and model. A run that
  would call the model without one fails with "client is required to call the
  model" before recording anything.
- **The `dashi` CLI reads more clearly.** `/help` groups commands under
  headings, lists skills in their own section, marks commands that also work
  during a turn with `•`, and shows keys in a short aligned list. Secondary
  text is a lighter gray, and notices such as `/status` output use the normal
  foreground. The end-of-turn footer is a rule line
  (`── turn 3 · model · 2 model calls · 12,345 tokens · $0.0421`). The idle
  "Ask anything" hint is gone; the status line shows "/help lists commands"
  when it fits. The mouse wheel scrolls three lines per notch, and also
  scrolls dialog details.

- **The CLI shows delegated tasks on their tool lines.** The `task` tool line
  reads `task  explore · find the parser · t_k3f2`. One status line under it
  follows the task, such as `⎿ completed · 1 model call · $0.0069 · /task
  t_k3f2 to inspect`, in the tool line colors. It replaces the separate block
  that repeated the task ID, description, counts, and raw spend. `/tasks`
  prints one line per task in the same words, and counts are pluralized.

### Fixed

- **The CLI scrolls up when a mouse selection is dragged to the top row.** In
  fullscreen mode, holding a selection drag on the transcript's top row now
  scrolls up and extends the selection, as dragging below the transcript
  already scrolled down. The transcript starts on the screen's first row, so
  the terminal could never report the pointer above it.
- **A task result no longer shows an empty user line in the CLI.** The turn
  that a finished task's result starts now opens with `↳ task t_k3f2
  (explore) completed · result delivered`, live, after a reopen, inline, and
  in plain output.

## [0.2.0] - 2026-10-01

### Changed

- **`Session` moves to package `session`.** Import
  `github.com/deepnoodle-ai/dashi/session` and write `session.Session`. It is
  built only on the public `Runner` API, which any host can use. Names that
  only a session uses move with it, without the redundant prefix:
  `SessionState` is `session.State`, `SessionIndex` is `session.Index`,
  `ErrSessionNotFound` is `session.ErrNotFound`, and `ErrSessionExists` is
  `session.ErrExists`. `TurnID`, `Liveness` and its constants,
  `TurnLiveness`, `TurnBusyError`, `ErrTurnBusy`, `Queued`, `Compactor`,
  `ErrNothingToCompact`, `CloseReason`, and the warning codes `turn_closed`,
  `memory_only`, `queue_blocked`, and `compaction_failed` keep their names in
  package `session`. The session's own error messages begin with `session:`
  instead of `dashi:`. `Say`, `Inbox`, `WarningTurnUnreadable`, and
  `WarningCompactionMismatch` stay in `dashi`.

### Added

- **Helpers for hosts that drive `Runner` directly.** `Runner.CacheKey` sets
  a conversation's prompt cache key; `Recorded(err)` tells a failure after
  recording began from a rejected command; `Turn.Clone` deep-copies a turn;
  `ContextItem.Validate` checks context input; `TurnView.ReplacementHistory`
  returns a compaction turn's replacement history; `Inbox.Has` reports a held
  steer ID; and `DerivedRevisionPrefix` marks a revision the runner derived.

## [0.1.1] - 2026-10-01

### Changed

- **Installable CLI.** `go install github.com/deepnoodle-ai/dashi/cmd/dashi@latest`
  now works. A committed `go.work` replaces the sub-modules' `replace`
  directives, which `go install` refuses. (#163)
- **Release tooling.** `make release-prep`, `release-notes`, `tag-modules` and
  `release-publish` cut a release of all four modules together. (#162)

## [0.1.0] - 2026-10-01

### Added

- **Command identity and JSON codec.** `CommandIDOf`, `CommandKindOf`,
  `MarshalCommand` and `UnmarshalCommand` give stored commands a public
  identity and a lossless JSON format. (#157)
- **Turns without a model client.** `Cancel`, `Settle` and `Deliver` run
  without one, and an already-closed `WithPause` admits a Turn without running
  it. (#155)
- **`llm.ModelInfo.CanBoundCost`.** (#155)
- **Per-attempt spend.** `TurnView.Attempts` reports spend per attempt, and
  `AttemptSpendOf` settles it inside a recorder. (#156)
- **`ProjectHistory`** builds history across Turns. (#158)
- **A guide to running Turns without a `Session`.** (#160)

### Changed

- **One compaction path.** `Compactor.Start` is the only way to compact; the
  uncharged summarizer is removed. (#158)
- **CLI.** Quieter resource warnings and a readable muted text color. (#159)
