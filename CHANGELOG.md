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
- **A `Runner` needs a `Client` only to fill in an agent or call the model.**
  A Start with an already-closed `WithPause` records a turn without running it
  and without a `Client` when its agent names its provider, API, and model. A run that
  would call the model without one fails with "client is required to call the
  model" before recording anything.

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
