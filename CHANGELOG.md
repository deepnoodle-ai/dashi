# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Added

- **`decide/toolcheck` checks risky commands and outside content.**
  `toolcheck.CommandRisk{}` holds a shell command that could likely cause
  severe harm, such as `rm -rf ~`, for approval. `toolcheck.PromptInjection{}`
  warns the model about a tool result that likely tries to take over the
  agent. Both fail open when Decide is down; a command whose check the input
  breaks, such as by a timeout, is held for approval. `Check` lets a host
  ask the user in its own approval hook. The `PromptInjection` hook records
  every check it makes, passed, flagged, or unchecked. When the input may
  have broken the check, such as by a timeout, the model reads that the
  result was not checked and is untrusted; the result is never held.
  `toolcheck.ReadResultRecord` reads a record back from the turn, and
  `toolcheck.CauseOf` names why a check could not answer, and `OnStart`
  tells a host that a result waits for its check.
- **`dashi --decide command_risk`** (or `DASHI_DECIDE`, or `"decide":
  ["command_risk"]` in `~/.dashi/settings.json`) checks each shell command
  with a decision model before it runs. The prompt shows the answer, a
  flagged command offers allow once and deny only, and in
  `--dangerously-skip-permissions` mode a flagged command still asks.
  `dashi run --yes` refuses it. Every decide feature is off by default, and a
  project settings file cannot turn one on. `/settings` shows a `decide`
  section and the provider, and tool rows and the turn footer show the
  answers. Missing credentials or an unknown `DECIDE_PROVIDER` give one
  warning at start and turn the checks off; `dashi run --yes` refuses to
  start instead.
- **`turnwatch.ReadReminder`** reads a reminder the watcher recorded: the
  confirmed question (`turnwatch.QuestionRepeating` or
  `QuestionOffRequest`), its probability, and the trigger, so a host need
  not parse the recorded reason.
- **`dashi --decide prompt_injection,turn_watcher`.** `prompt_injection`
  warns the agent about an MCP result, or `gh`, `curl`, `wget`, or `http`
  output, that likely tries to take it over; the row shows the warning, and
  the result is never hidden. `turn_watcher` reminds the agent when it
  repeats a failing call, and the transcript shows the reminder. Both are
  off by default, and `/settings` counts each feature's checks. Each tool
  row names what decide checked: `decide ✓ command, result`, or a line
  such as `decide result: injection 96%, hidden 3% — flagged, agent
  warned`.
- **Cloudflare for every decide package.** With a nil `Client`, the decide
  packages read `DECIDE_PROVIDER` as the `decide` command does:
  `cloudflare` uses `CLOUDFLARE_AUTH_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, and
  `CLOUDFLARE_BASE_URL`; `typesafe`, the default, uses `TYPESAFE_*`.
  `defaultclient.Load` makes that client without a request and reports the
  provider, or an error that wraps `defaultclient.ErrSetup` for missing
  credentials or an unknown provider, so a host can warn at startup.
- **`workspace.AskConcurrently()`** lets a `Policy` call its approval
  callback for several tool calls at once, for a host that queues its own
  prompts or does slow work before it shows one. Calls that one remembered
  answer would cover still wait for each other. The `dashi` CLI uses it, so a
  decide check in bypass mode no longer holds up other tool calls.
- **`Warning.Hook` names the hook of a `hook_failed` warning,** so a host
  that shows a hook's failures its own way can find them without reading
  the message.
- **An `AfterTool` hook can record a result it kept.** A `ResultDecision`
  with no replacement but a `Reason` or `RulesVersion` is recorded as
  `VerdictNoChange`, as for `BeforeModel`, such as a check that passed.
- **`FailOpen` on `BeforeTool` hooks.** A failing hook then counts as no
  opinion, with a `hook_failed` warning, instead of denying the call. Use it
  only for advice, never for a permission policy.

### Changed

- **Large terminal input stays responsive when deleting text.** Wonton v0.7.2
  fixes Ctrl-U, Ctrl-K, and Ctrl-W stalls on long pasted lines.

- **The Dashi terminal interface uses a quieter layout:** a compact header,
  shaded prompt and input bands, indented tool details, and compact status.
  Narrow terminals keep approval choices visible; inline output uses the
  actual terminal width.
- **Copy selected transcript text with Ctrl-C.** Confirmed local copies clear
  selection; failed or unconfirmed requests retain it. F2 toggles native
  selection without replacing the draft. Dialog cancellation stays unchanged.
- **Pasted slash commands stay editable until Enter.** Large ordinary text no
  longer produces a file-selection warning.

- **The workspace file tools and `ask_user` report a false open-world
  hint,** so a check on outside content leaves their results local. `bash`
  stays unset.
- **The `delegate` task tools and `message_parent` report a false
  open-world hint,** so a check of outside content does not send a
  subagent's answer out again; the subagent's own turn checks what it reads.
- **An MCP server can no longer mark its tools closed-world.** The `mcp`
  toolset keeps an `openWorldHint` of true and drops a false one, so
  `toolcheck.PromptInjection` reads every MCP result: the text a server
  returns may come from anyone who can write to its data.

- **The `decide` packages are part of the root module,** on Decide v0.3.0.
  `go get github.com/deepnoodle-ai/dashi` is enough to import
  `dashi/decide/listtool` and the others; import paths are unchanged. Every
  Dashi module now needs Go 1.27.

## [0.3.0] - 2026-10-06

### Added

- **`dashi --dangerously-skip-permissions`** approves every tool call without
  asking, for use in an externally sandboxed environment.
- **`dashi --compaction-threshold N`** (or `DASHI_COMPACTION_THRESHOLD`)
  compacts history at N tokens instead of 80% of the model's window.
- **`claude-sonnet-5-5` and `gpt-6.1-sol` in the model catalog,** with their
  windows, effort ladders, and prices, so Codex calls are now priced.
- **`task_send`** lets the parent agent steer a running task or give an ended
  task a follow-up turn. Each message is delivered once, across `Recover`.
- **`message_parent`** lets a task message its parent, as a steer or a queued
  message. Hosts without a session get it as a `TaskMessage` event.
- **The CLI shows messages between the agent and its tasks** as `→ t_k3f2`
  and `← t_k3f2` lines, and `/tasks ID` shows each task turn's reply.
- **`session.State.QueuedUser` and `Session.RunQueueUnlessUserQueued`** let a
  host wake a session for a task's message without taking the user's queue.
- **`wait_seconds` on `task`** waits up to 60 seconds for the task's answer
  or a message from it.
- **The `decide` module (Go 1.27)** uses decision models as judges:
  `contextrank` adds the context each turn needs, `turnwatch` reminds an agent
  that repeats a failing call, and `listtool` adds `judge_items` and
  `pick_item`.
- **Selective compaction (`decide/selective`), experimental,** keeps the
  older history still needed word for word and summarizes the rest.
- **Decide examples** in `examples/decide`: a tool risk gate, a
  prompt-injection screen, and per-turn tool narrowing.
- **`tool.Outcome.Data`** carries structured JSON about a call for the host;
  the model never sees it.
- **`TurnView.Input`** holds the turn's input, so hooks and tools can see what
  the user asked for.
- **`ModelDecision` and `ResultDecision` carry `Reason` and `RulesVersion`,**
  recorded like `ToolDecision`'s.
- **`compaction.IsSummary`** reports whether a message is a compaction
  summary.
- **`session.Session.Delivered`** drops queued items that the record shows
  were delivered another way.

### Changed

- **Breaking: plain names across the API,** such as `Settle` → `Resolve` and
  "attempt" → "model call". Turns recorded by v0.2.0 do not load. #170 lists
  every rename.
- **Breaking: `session.Compactor.Start` takes a `context.Context`** and runs
  without the session lock, so a slow compactor never blocks `Steer`.
- **Breaking: a new `dashi` command line on Wonton.** `dashi run` replaces
  `-p`; `dashi models` and `dashi completion` join it. A flag wins over its
  `DASHI_*` variable.
- **A `Runner` needs a `Client` only to fill in an agent or call the model.**
- **The CLI approval prompt is easier to read:** a one-line title, `key:
  value` arguments, diffs for edits, and no internal IDs.
- **The CLI's help, footer, and scrolling read more clearly,** and the
  composer highlights a known command name.
- **The CLI shows a delegated task on its `task` tool line,** with one status
  line under it.

### Fixed

- **A task's message reaches the agent in the CLI;** before, the CLI dropped
  `message_parent` events.
- **A task result the agent read with `task_status` is not sent again.**
- **A mouse selection in the CLI survives across a turn,** and dragging to
  the top row scrolls up.
- **A task result no longer shows an empty user line in the CLI.**
- **The CLI says task results wait only when the parent turn really stopped.**
- **Task status lines keep their spacing.**
- **A subagent knows its task ID.**

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
