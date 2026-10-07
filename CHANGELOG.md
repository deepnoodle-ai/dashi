# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Added

- **`dashi --session-id UUID`** starts a new session with an ID the caller
  chose. An ID that already names a session is refused; `--resume UUID`
  reopens it.
- **`dashi --turn-events PATH`** appends a JSON line when a turn starts
  (`UserPromptSubmit`), ends (`Stop` or `StopFailure`), or waits for you
  (`Notification`), in the field names of Claude Code hook inputs. The end
  is written before dashi takes the next prompt.
- **`claude-haiku-5-5` in the model catalog,** with its 1M context window,
  128K output limit, `low` to `max` effort ladder, and prices that depend on
  prompt length: $0.10 input, $0.50 output, $0.01 cache read, $0.125
  five-minute and $0.20 one-hour cache writes per MTok, rising to $0.50,
  $2.50, $0.05, $0.625, and $1 above 100,000 input tokens, plus $10 per 1,000
  web searches. Temperature and budget thinking are marked unsupported.
- **`mistral-large-4` and `mistral-large-4-0` in the model catalog** as
  preview models, with the 524,288-token context window Mistral's models
  endpoint reports and list prices of $1.36 input, $0.14 cached input, and
  $4.18 output per MTok. The launch discount of 50% until about 2026-10-20 is
  left as a note.
- **`mistral-medium-2604` and `mistral-medium-3-5` in the model catalog,**
  the Mistral Medium 3.5 model that `mistral-medium-latest` and
  `magistral-medium-latest` now name as their snapshot.

### Changed

- **`catalog.Small("anthropic")` is now `claude-haiku-5-5`,** replacing
  `claude-haiku-4-5`. The vision, documents, and providers examples use it.
- **Deprecated models are marked in the catalog:** `claude-sonnet-4-5` and
  `claude-sonnet-4-5-20250929` (retire 2026-11-30; use `claude-sonnet-5-5`),
  and `gpt-5.3-codex`, `gpt-5.1`, and `gpt-5.4-nano` (removed 2027-04-01; use
  `gpt-6-sol`, `gpt-6-sol`, and `gpt-6-luna`).
- **Mistral cache reads cost a tenth of input,** as Mistral's pricing page now
  lists; the catalog previously stated the input rate. Per MTok: $0.15 on
  `mistral-medium-latest` and `magistral-medium-latest`; $0.015 on
  `mistral-small-latest`, `magistral-small-latest`, and `mistral-small-2603`;
  $0.05 on `mistral-large-latest`; $0.02, $0.015, and $0.01 on the
  `ministral-14b`, `ministral-8b`, and `ministral-3b` models (`-latest` and
  `-2512`); and $0.03 on `codestral-latest`.

### Fixed

- **Command-specific Decide failures no longer pause every command check.**
  `toolcheck.CommandRisk` confirms server and connection failures with one
  fixed benign request under the original deadline. An unconfirmed failure
  requires approval, including in CLI bypass mode. Commands send at most
  eight parts plus one confirmation request, without SDK retries.
- **`toolcheck.Verdict.Requests` counts logical Decide calls initiated,**
  including command outage confirmation. `Usage` retains observed tokens
  on failed checks. The CLI uses these observations instead of guessing
  request counts from error types.
- OpenAI Responses preserves the served service tier in blocking and
  streaming results, so callers can verify the pricing tier.
- Selective compaction bounds focus text after JSON encoding, so escaping
  does not cause unnecessary Decide fallbacks.
- Recorded selective evaluation judges saved case needles in keyless runs.
  Paid runs require a conservative shared cost reserve. Preflight, explicit
  cheaper-model overrides, and output limits make comparisons reviewable.
  Reports omit private needle text and source session identifiers.
- **Anthropic tools with no arguments accept empty stream fragments.** A
  completed empty-object MCP call retains `{}` and runs normally. Malformed
  nonempty JSON and interrupted tool blocks still fail without execution.
- **Retained Anthropic direct-tool history can continue on another provider.**
  Preparation recognizes `caller: {"type":"direct"}`, preserves saved
  messages, and records portable replay stripping. Programmatic callers and
  unfamiliar replay dependencies remain rejected.
- **If a conversation outgrows the model's context window, the next message
  now summarizes older history first** instead of failing every time. Library
  sessions need a `Compactor` for this; the CLI always has one, including
  `dashi run -c`. History size now counts large tool results
  added after the last model call.
- **If a history summary is cut off for length, Dashi asks once for a shorter
  one.**
- **Print and plain modes show "Compaction completed." once** instead of
  repeating it.

- **The Anthropic Messages client sends reasoning effort `xhigh`** as
  `output_config.effort`. It refused `xhigh` before, although the catalog
  lists it for models such as Opus 5.5 and Sonnet 5.5. A model whose catalog
  entry omits `xhigh` still gets `high` under the adjust policy, or an error
  under strict.

## [0.4.0] - 2026-10-06

### Added

- **`decide/toolcheck` checks risky commands and outside content.**
  `CommandRisk{}` holds a shell command that could cause severe harm, such as
  `rm -rf ~`, for approval; `PromptInjection{}` warns the model about a tool
  result that likely tries to take over the agent.
- **`toolcheck` fails open only on an outage.** When the input breaks a check,
  such as by a timeout, the command is held and the result is marked untrusted.
  `Check`, `ReadResultRecord`, and `CauseOf` serve hosts with their own hooks.
- **`dashi --decide`** turns on decision-model checks, each off by default:
  `command_risk`, `prompt_injection` (MCP results; `gh`, `curl`, `wget`, `http`
  output), and `turn_watcher` (repeated failing calls). `DASHI_DECIDE` and
  `"decide"` in `~/.dashi/settings.json` work too; project settings cannot.
- **A flagged shell command always asks,** offering allow once or deny, even
  with `--dangerously-skip-permissions`; `dashi run --yes` refuses it. Tool
  rows, the turn footer, and `/settings` show what decide checked and found.
- **Missing decide credentials give one warning and turn the checks off;**
  `dashi run --yes` refuses to start instead.
- **Decide packages can use Cloudflare.** With a nil `Client` they read
  `DECIDE_PROVIDER` as the `decide` command does; `defaultclient.Load` reports
  the provider, or an `ErrSetup` error a host can warn about at startup.
- **`turnwatch.ReadReminder`** reads a recorded reminder (question,
  probability, trigger) without parsing its reason.
- **`selective.Selection.Cause` and `Selection.Requests`** name why the
  selective compactor fell back to its `Base` and count every request started.
- **`workspace.AskConcurrently()`** lets a `Policy` run its approval callback
  for several tool calls at once. The CLI uses it, so a decide check in bypass
  mode no longer holds up other tool calls.
- **`Warning.Hook`** names the hook behind a `hook_failed` warning.
- **An `AfterTool` hook can record a result it kept:** a `ResultDecision` with
  a `Reason` or `RulesVersion` and no replacement is recorded as
  `VerdictNoChange`.
- **`FailOpen` on `BeforeTool` hooks** makes a failing hook count as no
  opinion, with a `hook_failed` warning, instead of denying the call. Use it
  only for advice, never for a permission policy.

### Changed

- **The `decide` packages are part of the root module,** on Decide v0.3.0.
  `go get github.com/deepnoodle-ai/dashi` is enough; import paths are
  unchanged. Every Dashi module now needs Go 1.27.
- **Open-world hints are stricter.** Workspace file tools, `ask_user`,
  `delegate` task tools, and `message_parent` report false; MCP tools always
  report true, so `PromptInjection` checks every MCP result.
- **`dashi --decide selective_compaction` stays off,** and its warning now
  says why: on recorded sessions it kept no more needed details than keeping
  the newest messages.
- **The terminal interface uses a quieter layout:** a compact header, shaded
  prompt and input bands, and indented tool details. Narrow terminals keep
  approval choices visible.
- **Ctrl-C copies selected transcript text,** and F2 toggles native selection
  without losing the draft.
- **Large pastes stay editable until Enter,** including pasted slash commands.
  Ctrl-U, Ctrl-K, and Ctrl-W no longer stall on long lines (Wonton v0.7.2).

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
