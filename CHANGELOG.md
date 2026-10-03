# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Changed

- **Plain names across the API.** Names now say what they mean, so a new
  reader does not have to decode them. Recorded values change with the names
  (step kinds, JSON keys, stop and verdict codes), so turns recorded by an
  earlier version do not load. The main renames, old → new:
  - Commands: `Settle` → `Resolve`, `Deliver` → `AddMessages`,
    `Session.Answer` → `Session.Resolve`, `ApprovalFact` →
    `ApprovalDecision`, `ContentResolver` → `ContentLoader` (method `Load`).
  - Results: `Outcome` → `RunResult`, `TurnView.Answer` → `Reply`,
    `Replay(turn)` → `turn.View()`, `TurnView.Revision` → `LastSeq`,
    `Stop.Terminal` → `Ended`, `Stop.Projected` → `Inferred`, `uncertain` →
    `result_unknown`.
  - Next steps: `NextAnswer` → `NextResolvePending`, `NextResolve` →
    `NextFix`, `NextResume` → `NextRecover`, `NextRaiseAttempts` →
    `NextRaiseModelCalls`.
  - Steps: `model_requested` → `model_call_started`, `model_settled` →
    `model_call_finished`, `model_reconciled` → `model_call_finished_by_host`,
    `tool_settled` → `tool_finished`, `approval_settled` →
    `approval_decided`, `context_delivered` → `messages_added`;
    `StepStarted`/`Continued`/`Stopped` → `StepTurnStarted`/`Continued`/`Stopped`.
  - Model calls and cost: "attempt" → "model call" (`Limits.ModelCalls`,
    `ModelCallID`, `ModelCallCost`, `TurnView.ModelCallCosts`), `AtRisk` →
    `WorstCaseCost`, `Charge` → `Cost`, `SpendBasis` → `UsageSource`
    (`reported`/`estimated`/`not_sent`), `llm.FailureDisposition` →
    `llm.SendStatus`, `Admission` → `Reservation`, `Limits.Interruptions` →
    `Resumes`, `FitTokens` → `SuggestedMaxOutputTokens`.
  - Tools: every `Hosted*` name → `ProviderTool*`, `Defer` →
    `NeedsApproval`, a hook decision's `Revision` → `RulesVersion`.
    `OutputFromTool`/`OutputFromText` → `OutputSourceToolCall`/`OutputSourceText`.
  - History and recording: `ProjectContext` → `TurnMessages`, `Project` →
    `TurnMessagesFull`, `ProjectHistory` → `BuildHistory`,
    `Session.ProjectedHistory` → `NextHistory`, digest and fingerprint →
    hash (`HistoryHash`, `ArgsHash`, `RequestHash`, `CommandHash`),
    `Agent.Revision` → `Agent.Version`, `ErrRevisionConflict` →
    `ErrStaleTurn`, `ErrCommandConflict` → `ErrCommandIDReused`,
    `CommitError.Persistence` → `NothingWritten`, `session.Liveness` →
    `WorkerStatus`, `ModelFact.Origin` → `Endpoint`.
  - A limited stop's reason is now its `LimitKind` (for example
    `model_calls`), and every stop-reason code has a constant.
- **The CLI approval prompt is easier to read.** It shows a one-line title,
  such as `Allow task?  explore subagent: messaging test wait`, and the
  choices as highlighted keys: `[y] Allow once  [a] Allow for this Dashi run
  [n] Deny`. Arguments show as `key: value` lines with multi-line text
  indented, not as JSON. A long value shows its first lines and `+N more
  lines`; `e` shows all of it. Shell commands show the command and its
  directory, and file edits show the diff, without repeating those arguments.
  Internal turn and call IDs no longer appear in approvals or questions; a
  prompt from a delegated task names the task. Line mode prints the same
  layout.
- **A `Runner` needs a `Client` only to fill in an agent or call the model.**
  A Start with an already-closed `WithPause` records a turn without running it
  and without a `Client` when its agent names its provider, API, and model. A run that
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
  scrolls dialog details. In the composer, a draft that starts with a known
  command, alias, or skill name (such as `/effort` or `/q`) shows that name
  in bold cyan; a partial or unknown name stays plain.
- **The CLI shows delegated tasks on their tool lines.** The `task` tool line
  reads `task  explore · find the parser · t_k3f2`. One status line under it
  follows the task, such as `⎿  completed · 1 model call · $0.0069 · /task
  t_k3f2 to inspect`, in the tool line colors. It replaces the separate block
  that repeated the task ID, description, counts, and raw spend. `/tasks`
  prints one line per task in the same words, and counts are pluralized.

### Added

- **`tool.Outcome.Data` carries structured detail about a call for the
  host.** It is raw JSON that the turn record keeps and the model never sees.
  Like `Progress.Data`, Data that is not valid JSON or exceeds 64 KiB is
  dropped, and the step records the new `WarningToolDataDropped`; the call
  still succeeds. Data describes the result the tool returned, so it is also
  dropped, with the same warning, when an `AfterTool` hook replaces or
  withholds the result or `Limits.ToolResultBytes` shortens it. A change the
  model does not see, such as one to `Output.Display` only, keeps it.
- **`session.Session.Delivered` drops queued items that the record shows
  delivered another way,** just before a queue turn, so they start no turn.
- **`ModelDecision` and `ResultDecision` carry `Reason` and `RulesVersion`,**
  recorded in the hook's `HookResult` as `ToolDecision`'s are, so a
  `BeforeModel` or `AfterTool` hook can say why it changed something and which
  rules or decision model decided. The model never sees them. A `BeforeModel`
  stop with no `Reason` still records `Stop.Reason`.

### Fixed

- **The CLI scrolls up when a mouse selection is dragged to the top row.** In
  fullscreen mode, holding a selection drag on the transcript's top row now
  scrolls up and extends the selection, as dragging below the transcript
  already scrolled down. The transcript starts on the screen's first row, so
  the terminal could never report the pointer above it. Wonton v0.5.0 now
  handles this edge row.
- **A task result no longer shows an empty user line in the CLI.** The turn
  that a finished task's result starts now opens with `↳ task t_k3f2
  (explore) completed · result delivered`, live, after a reopen, inline, and
  in plain output.
- **A task result the agent read with `task_status` is not sent again.** A
  `task_status` call that returns a task's final result records the child
  turn in its outcome's `Data`, and the new `delegate.DeliveredNotices(turns)`
  counts it as delivery (decision 46). The record is checked just before a
  notice is delivered: on `Recover`, before a queue turn (the new
  `Session.Delivered`, which `NewTasks` sets), and by the CLI, which holds
  notices until the turn ends. So the read starts no extra turn, also after
  a reopen. A notice that reaches a running turn is not checked, so the
  model may see a result twice in that turn. If the parent turn is canceled
  before the read is recorded, the notice goes out on the next turn. Reading
  a running task or the task list delivers nothing, and the user's `/task`
  and `/tasks` never count as delivery. A read whose result an `AfterTool`
  hook changed or the result limit shortened records no `Data`, so it does
  not count, and the notice still delivers the whole reply.
- **The CLI no longer says task results wait for a stopped parent while the
  parent runs.** The line now shows only when the turn really stopped, and
  says what to do: "Task results will be sent when this turn goes on. Press
  Enter to continue it, or type a new prompt." It and the pending-prompt line
  are quiet indented lines.
- **Task status lines keep their spacing.** Two spaces follow `⎿`, and the
  space before ` · ` no longer disappears where a status line wraps.
- **A subagent knows its task ID.** Its instructions end with "Your task ID
  is t_k3f2.", so it can name itself in its reply.
- **A mouse selection in the CLI no longer vanishes across a turn.** In
  fullscreen mode, dragging a selection over a task line or a blank reply
  used to drop the whole selection. A selection now also copies a tool
  line's name and argument, and copies a turn footer without its `──`
  rule, so the copied text matches what is highlighted. Glyphs such as
  `⏺`, `❯`, `↳`, and `⎿`, and a tool's state, are still left out. With
  Wonton v0.6.0, copied text has a blank line between blocks, as on
  screen, each selected row highlights as one continuous band, and a drag
  that starts or ends on a glyph, rule, or gap row snaps to the nearest
  text.

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
