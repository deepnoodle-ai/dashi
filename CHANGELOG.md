# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Added

- **The CLI shows a command menu while you type a slash command.** Under the
  composer it lists up to five matching commands and skills with their
  descriptions, ordered as Tab completion orders them. ↑/↓ move the
  selection, Tab completes the name, Enter runs a command that needs no
  argument or fills in one that does, and Esc closes the menu and keeps the
  draft.

### Changed (breaking)

- **`mcp`: tool names longer than 64 characters change.** A name of the form
  `mcp__<server>__<tool>` that is over 64 characters is cut and given a hash
  suffix. The suffix now hashes the raw server and tool names, not the forms
  with characters replaced by `_`, so two long names that differ only in those
  characters no longer collide. Every shortened name therefore differs from
  v0.8.0: update permission rules, hooks, and saved references that name one.
  Names of 64 characters or fewer are unchanged.

### Removed

- **The CLI no longer prints the instructions and skill counts at startup.**
  `/resources` still shows sources and access scopes.

### Fixed

- Grok voice no longer emits `EventInterrupted` when the user starts
  speaking after a response has already finished; only a response still in
  progress is interrupted.
- Realtime `Send` and `Receive` called with an already-expired context
  return `ErrorCanceled` and leave the session open. A write or read that
  the context cuts off partway still closes the session.
- `realtime.Error.Error()` now includes `Code` and `Cause` when set.
- **Gemini video Files without an API key**: `InteractionsVideoJobClient`
  `PollVideo` and `FetchVideo` now fail before dispatch with an error that
  wraps `llm.ErrNoAPIKey`, as `SubmitVideo` does, instead of sending a
  request with an empty key header to the public endpoint.
- **`workspace.Policy` no longer skips the prompt for an `edit_file` call
  because its text is missing from the file.** `BeforeTool` runs while
  earlier calls of the batch still run or wait for approval, so in a batch
  such as `write_file a.go "v1"` then `edit_file a.go "v1" -> "v2"` the edit
  was allowed unasked and then changed the file. Only an edit that fails
  whatever the file holds (empty `old_string`, or one equal to `new_string`)
  now runs without a prompt. The `Hook` and `ApprovalRequest.Preview` docs
  say that a hook and a preview see the files before earlier calls of the
  batch take effect.
- **`mcp`: stdio servers survive a host Ctrl-C.** Each stdio server now runs
  in its own process group (a new console process group on Windows), so a
  terminal interrupt meant for the host no longer stops it. `Close` still
  stops the server, and on Unix also kills any children it left behind.
- **`mcp`: an empty image or audio result no longer ends the turn.** It
  becomes a text note and a status warning instead of a media block that
  failed validation.
- **`mcp`: `Refresh` keeps the tool snapshot in step with `Status`** when its
  context ends partway, and returns an error instead of reconnecting servers
  after `Close`.
- **`mcp`: `Status` no longer reports servers as connected after `Close`.**
  Each server that was connected now shows `StateFailed` with the error "the
  MCP toolset is closed".
- **`delegate`**: a child turn interrupted while `Tasks` is open, such as by
  a pause, now reports its notice, and mail that missed it starts the next
  turn. Before, the notice was dropped as if `Close` had interrupted it, and
  the unread mail kept `Tasks.Wait` and `RunUntilQuiet` waiting forever.
- **`delegate`**: `message_parent` and `task_send` match a turn to its task
  by the whole turn number, so a turn of the child for call `call-1` no
  longer counts as a turn of the child for call `call`.
- **`delegate`**: `NewTasks` now extends a `Delivered` function the parent
  session already has instead of leaving it alone, so a host that set
  `DeliveredNotices` still gets agent messages dropped from queue turns.
- **`dashi` CLI diffs after shell calls** show only what the call changed.
  Changes already in the working tree when the call started are no longer
  shown again after every command, so a read-only command such as `ls`
  shows no diff. A file the call reverted is listed as no longer changed.
- **`dashi` CLI skills named like a built-in command** such as `/status`,
  `/tasks`, `/attach`, or an alias such as `/q`, are now reported by
  `/resources` as shadowed by the built-in, and a skill can no longer take
  a built-in alias's name in the command list.
- A turn whose structured output was recorded now completes when
  `Limits.Deadline` passes before its stop is written, instead of stopping
  `limited` at the deadline. This holds both in the running worker and in
  `Runner.Advance` after a worker died between the output and its stop. A
  late `Cancel` already behaved this way.
- `Runner.Advance` and `Runner.Drive` now end a turn that recorded an
  interruption at its `Limits.Resumes` limit: they run the `Continue` that
  records the `resumes` limit, as they already did for a crashed turn out of
  resumes. Before, the turn stayed at `NextFix` and `Drive` waited on it.
- **A `task` call with `wait_seconds` returns the answer of a task that
  ends quickly.** A task that ended before the call returned got the
  "already ended" text instead of its answer, and the notice was not marked
  delivered.
- **The CLI no longer races or panics when a task result arrives while it
  waits for background work.** Waiting for idle used a `sync.WaitGroup` that
  task wakes reused during a wait, which could panic with "WaitGroup is
  reused before previous Wait has returned".
- **Chat Completions refusals are no longer lost.** A `refusal` in a response
  or stream becomes `llm.ContentRefusal`, streams as text deltas, and stops
  with `StopRefusal` when the service reports a plain `stop`. Before, the
  reply was empty and looked like a normal finish. A refusal in history
  replays as the assistant's `refusal` field to OpenAI and as assistant text
  to compatible services.
- **Chat Completions streams accept `reasoning_details`.** OpenRouter's
  structured reasoning now streams instead of failing the call. Fragments of
  one reasoning item are merged by `index`, so the result carries the same
  replayable content a complete response does.
- **`session`: missed steers keep their order.** A steer sent while a turn
  ran, that the turn never saw, now takes its arrival place in the queue:
  behind input queued before it was sent, and ahead of input queued after,
  so a new Start that closes the turn sends them in the order they were
  typed.
- **`session.Resolve` default command IDs name their kind.** A Resolve
  without a command ID now gets `"approval:"`, `"result:"`,
  `"provider-tool-approval:"`, or `"model:"` and its call ID, as `Pending`'s
  builders derive theirs, instead of `"resolve:"` and the call ID. A call's
  approval and the later result of its detached work no longer share one
  command ID, which failed with `ErrCommandIDReused`. A provider tool
  approval without a `CallID` uses its request ID. A Resolve first sent
  before this change and retried after it gets a new ID, so the retry is
  not recognized as the same command.
- **An urgent steer no longer cuts an active-compaction summary call.**
  Cutting it spent the turn's summary, so a turn already over its window
  ended `limited` with `context_window`. Steers, urgent or not, now wait for
  the ordinary model call after the summary.

## [0.8.0] - 2026-10-13

### Added

- **`Compactor.StartIfNeeded`** answers, for a durable host, whether a
  compaction turn is due after its stored turns, and returns its Start with
  command ID `"compact:"` and the last turn's ID, so a retry, a restarted
  worker, or a second worker finds the same turn. It applies the session's
  rule: after any ended turn but a compaction turn, when the last turn
  overflowed or `Needed` says so, and never when nothing can be summarized
  or the replacement would itself be due. It makes no model call and writes
  nothing.
- **`CompactionFact.SummaryFallback`** is a stand-in for the summary that
  `Compactor.Start` builds with no model call: a note that N earlier messages
  were removed, then their user messages, the names of the tools called, and
  the agent's last reply, cut to half the summary's output allowance.
- **`dashi.WarningSummaryFallback`** (`summary_fallback`, with the turn's ID)
  is recorded once, on the stop that ends a compaction turn without a
  summary, so it is in that turn's `RunResult` and `TurnView.Warnings`.
  `BuildHistory` does not repeat it; `session` passes it on in the next
  turn's `SessionWarnings`.
- **`Policy.DueTokens`** sets the due count in tokens; when positive it
  overrides `Fraction`, and it is capped at the window only when the window
  is known.
- **`gemini-4-argon` in the model catalog,** generally available, with its
  1,048,576-token context window, 65,536-token output limit for synchronous
  calls, and prices of $2 input, $10 output, and $0.10 cache read per MTok at
  any prompt length, plus $14 per 1,000 search requests. Temperature is marked
  unsupported, since Google ignores it on this model. Google doubles these
  rates on 2026-12-01; the entry notes it.
- **`muse-wonder-1.0` in the model catalog** as a preview model on Meta's
  Responses endpoint, with its 1,048,576-token context window, `minimal` to
  `max` effort ladder, image, PDF, and tool support, and prices of $4 input,
  $20 output, and $0.20 cached input per MTok, plus $2.50 per 1,000 search
  queries. Audio input is left unknown.
- **`llm.FeatureAssistantPrefill`** (`assistant_prefill`) records whether a
  model accepts a history that ends with an assistant message to continue.
  The catalog marks it unsupported for Claude Haiku 5.5, Opus 4.6 and later,
  Sonnet 4.6 and later, and the Fable and Mythos models, as Anthropic's
  migration guides document.

### Changed

- **`gemini-3.5-flash` and `gemini-3.7-flash` are marked deprecated.** Google
  routes their requests to `gemini-3.6-flash` and `gemini-3.8-flash`, and
  names no shutdown date.
- **A prefill on a model that rejects one fails during preparation** with an
  `ErrorUnsupported` error that says to end the history with a user message,
  under both capability policies, instead of a 400 from the API. Dashi does
  not drop the message, because that would change the answer. On Claude
  Haiku 5.5 this applies with or without reasoning; before, Messages
  rejected a prefill only when reasoning was on.
- **A failed summary no longer stops compaction.** Every ended compaction
  turn replaces the history: with its summary when it completed with one,
  otherwise with its `SummaryFallback` (no frame), or a fixed note when that
  is empty.
  `IsSummary` recognizes the stand-in. A compaction turn that has not ended
  replaces nothing.
- **A compaction turn ends itself.** The engine records `Stop.Ended` on
  every stop of a compaction turn except a host hold (a host limit without
  `Ends`) and an interruption with no model call in flight: a failure, a
  refusal, its own budget or model-call cap, or an empty reply ends it, so
  its fallback stands in with no host step. Run, Continue, Advance, and
  Drive apply the same rule, and replay checks it. A host only waits while
  the last turn has not ended.
- **`Compactor.Start` never fails on size.** When transcript items do not fit
  the summary model even at 1 KB each, it drops whole items from the middle,
  keeping the oldest and newest, behind one "[N items omitted]" marker.
- **Compaction is due at a fifth of the window.** `DefaultFraction` is now
  0.2 (was 0.8) and `DefaultWindow` 200,000 (was 128,000). The kept tail and
  the summary's output allowance are shares of the room below the due count
  after the fixed part (system and developer messages, and what the last
  call sent besides the history), together at most half of it; the
  allowance is also at most a quarter of the summary model's window.
  `Compactor.TailFraction` is now a share of that room. When the fixed part
  takes more than half the due count, the due count rises to the fixed part
  plus half of it, up to the window. `Needed` is false, and `Start` returns
  `ErrNothingToCompact`, only when the fixed part leaves no room.
- **`session.Compactor.Start` takes the last call's usage**, as `Needed`
  does, so a compactor can size the fixed part.
- **Sessions compact after any ended turn**, including one the new Start
  closed or one ended at a limit, not only after a completed turn. After a
  compaction turn held for budget (a host limit without `Ends`), a new Start
  refuses with a `*TurnBusyError`. A compactor `Start` error, or a compaction turn that
  cannot be recorded, is returned instead of running the next turn with
  the history that was due.
- **The CLI** compacts at 20% of the window by default;
  `--compaction-threshold` sets `Policy.DueTokens`. It warns once when a
  compaction turn ends without a summary.

### Removed

- **`session.WarningCompactionFailed`** (`compaction_failed` session
  warning). `dashi.WarningSummaryFallback` replaces it; no path keeps the old
  history any more.

### Fixed

- **Meta calls get a cost estimate again.** Meta reports service tier `auto`
  on every pay-as-you-go response, and the catalog did not price it, so
  `catalog.Cost` estimated nothing for Muse Spark or Muse Wonder. The catalog
  now prices `auto` for Meta at the model's standard rate.

## [0.7.0] - 2026-10-12

### Added

- **`Start.Items`** records initial context items, such as reminders, with
  the Start step. The first model request sends them after `Input`, later
  requests and active compaction keep them, and `Step.Items()` returns them.
  They are checked like `AddMessages` items.
- **`Runner.Record`** saves a Start without running it: it commits the Start
  step alone, with no model request, tool, steering read, or content load,
  and needs no Client when the agent names its provider, API, and model.
- **`ModelFact.ErrorCategory`, `HTTPStatus`, `Retryable`, and `RetryAfter`**
  copy the classified `llm.Error` of a failed model call, beside its
  original error text.
- **`Warning.TurnID`** names the turn of a `turn_unreadable` or
  `compaction_mismatch` history warning.
- **`Runner.Advance` and `Runner.Drive`** run a saved turn for a host:
  Advance loads the turn, applies the commands waiting in its inbox, and
  runs it until it must wait, with `Continue` IDs derived from the record
  (`continue:<LastSeq>`), so no outbox is needed. It never continues from a
  stop it recorded itself without new input, and always leaves the turn at
  a stop step. Drive repeats Advance, waiting for the inbox's signal, a
  15-second poll, or the turn's deadline, until the turn ends or ctx ends.
  `WithHistory` supplies the Continue's history, and `WithHistoryMismatch`
  sets how the Continues it runs treat a differing one (warn by default, or
  reject). A turn held by a host limit without `Ends`, such as a balance too
  low, is retried on every pass: Advance prepares the held call and asks
  `BeforeModel` and `BeforeSend` again, sending and recording nothing while
  they refuse with the same host limit code, and runs the turn once they
  allow it. The host only tops up the balance. A hold the recorder made
  with a `LimitError` is retried at most once per call and per inbox signal.
- **`FollowNext`** yields a turn's view now, one working view
  (`NextRunning`) when work starts after a stop, and a view at each later
  stop, ending after the stop that ends the turn, so a watcher never acts
  on an intermediate state and can show "running" without its own watcher.
- **`LimitDetail.Ends`** makes a host limit end the turn; without it a host
  limit waits (`NextFix`). **`Limits.NoRaise`** makes model-call, cost, and
  token limits end the turn and rejects raises.
- **`LimitError`**: a recorder that refuses a `model_call_started` step with
  `&CommitError{NothingWritten: true, Err: &LimitError{...}}` gets the
  limited stop recorded, as if `BeforeSend` had returned it.
- **`CommitRequest.ModelCall`** carries the started or finished call's
  `ModelCallCost`, so a recorder reserves and settles without loading the
  turn. **`CommitRequest.Delivered`** lists the inbox commands a step
  records as `DeliveredCommand{ID, Kind}`, so a host that keeps steers apart
  from other commands needs no guessing.
- **`mcp.ConvertResult` maps an MCP tool result to a Dashi outcome** for
  applications that call MCP tools themselves, including results saved as JSON
  and read back. It is the mapping `Toolset` uses; warnings come back as a
  slice. Image and audio media types are now lowercased, and `image/jpg`
  becomes `image/jpeg`.
- **`InboxRefuser`** hears the commands a turn did not take; **`WarningInbox`**
  records them on an open turn.

### Fixed

- **A model call is charged only the usage its provider reported.** A call
  with no reported usage (worker stopped, cut by a steer, result unknown)
  counts zero instead of its input estimate, and a call cut off after some
  usage arrived counts only that usage, with no visible-text output estimate.
  A steered call under a cost cap is no longer charged its worst case. Such
  phantom charges stalled turns on budget holds.
- **The input estimate behind `WorstCaseCost` is realistic.**
  `llm.EstimatedInputTokens` counts about four bytes per text token, a fixed
  amount per image, document page, audio, or video instead of its base64
  bytes, and never more than the model's context window. The next-call
  estimate starts from the previous call's reported input.

### Changed

- **`Steering` is now `Inbox`, and returns commands.** `Inbox.Waiting`
  replaces `Steers`; a `Steer` is a command whose ID is its item ID, and
  `MarshalCommand` encodes it (`CommandSteer`). `WithSteering` is
  `WithInbox`, `Runner.Inbox` sets one for every run, the in-memory `Inbox`
  is `MemoryInbox` (`NewMemoryInbox`, with `Put(turnID, command)`; its
  `Cancel(id)` is now `Withdraw(id)`),
  `SteeringReleaser` is `InboxReleaser`, and `SteeringNotifier` is
  `InboxNotifier`, whose `Ready` takes the turn ID and also returns an
  `unwatch` func that runs and `Drive` call when they stop watching, so an
  inbox can drop the turn's channel. `session.Session.Steering`
  is `Session.Inbox`. A `Cancel` in the inbox ends a running turn, unless
  the turn's output is recorded, when it is refused; other commands wait
  for Advance. `Run` rejects a `Steer`: steers reach a turn only through an
  inbox. Inbox command IDs starting with `continue:` are reserved for
  Advance. A failing inbox or invalid steer records `WarningInbox` instead
  of `WarningSteering`.
- **A model-call limit stop's `Stop.Reason` is prose**, such as "model call
  limit: 8 of 8 calls used", instead of `model_calls`; read
  `Stop.Limit.Kind` to recognize it.
- **A context-window stop ends the turn** and always records
  `Limit{Kind: LimitContextWindow}`; start a successor turn.
- **A turn whose only step is its Start is ready.** `Next` gives
  `NextContinue`, not `NextRecover`, and `Continue` counts no resume and adds
  no crash reminder.
- **A Start with an already-closed `WithPause` no longer records a turn
  without a Client.** Use `Runner.Record`. `Run` now requires a Client for
  every Start and Continue.
- **An ended turn refuses tool results.** Once a turn ends, its
  conversation is closed: a `Resolve` with a tool outcome gives
  `ErrTurnEnded`, and replay rejects a tool result after the ending stop.
  Only a late model result or failure is still accepted, because it corrects
  the turn's spend.
- **`Spend.Estimated` is now `Spend.CallsWithoutFinalUsage`** (JSON
  `calls_without_final_usage`).
- **`UsageSourceEstimated` is replaced by `UsageSourceCutOff`
  (`cut_off`) and `UsageSourceMissing` (`missing`).**
- **`ModelFact.Cost` and `ModelFact.EstimatedUsage` are removed.** Replay
  derives every charge with `ModelCallCostOf`; records written by earlier
  versions still load, and their stored charges are ignored. An interrupted
  call recorded by an earlier version keeps the visible-text output estimate
  that version wrote into its `Result.Usage`.
- **`llm.InputTokenUpperBound`** keeps the old one-token-per-byte bound for
  context-window fitting. **`llm.EstimatedMessageTokens`** estimates messages
  before they are in a request, and **`RequestSnapshot.EstimatedInputTokens`**
  lets a preparer supply its own count.

## [0.6.0] - 2026-10-10

### Added

- **`realtime` supports bidirectional voice sessions** through Gemini Live
  (`providers/google/geminilive`) and Grok Voice (`providers/grok/voice`).
  Sessions expose audio, transcripts, interruptions, tool calls, completion,
  and available usage through bounded transport. Applications execute tools.

## [0.5.0] - 2026-10-08

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
- **`catalog.Cost(client, result)`** returns a model call's cost: the
  provider-reported `Result.Cost`, or else a catalog list-price estimate only
  when the client's price is the unmodified catalog entry, the provider served
  that model, and the service tier is one the catalog prices. Its error says
  why there is no cost. Catalog files gain `priced_service_tiers` for that
  check.
- **`llm.Cost.String`** formats a cost for people, such as
  `0.0012 USD (list_price_estimate)`, starting with "at least" and listing
  unpriced requests when the cost is partial.
- **`dashi.Pending.HandleID`** returns the ID a detached call returned with
  `tool.Detached`, or "" when it has none.
- **`session.StoppedForContext(view)`** reports whether a Turn stopped
  because its history no longer fit or its summary failed.
- **`dashitest.Client.Hold(release)`** makes each `Send` wait until `release`
  is closed, so a test or example decides when scripted replies arrive.

### Changed

- **Decide v0.4.0.** The `command-risk` check now flags deploys and releases
  through Decide's new `publish` question, which replaces `external`.
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

### Removed

- **`decide/selective` is removed.** In testing it kept fewer details than
  simply keeping the newest messages word for word, so it was not worth a
  Decide call. `dashi --decide selective_compaction` is now an unknown
  feature; in `~/.dashi/settings.json` it is ignored with a warning.

### Fixed

- **Automatic compaction starts at the intended fill.** `Compactor.Needed`
  counted one token per byte, so sessions compacted at about a quarter to a
  third of the threshold.
- **A failing command check no longer pauses checks for other commands.**
  If Decide errors while checking a command, that command needs approval,
  even in bypass mode. Checks pause only when Decide isn't set up (for
  example, a missing key). `toolcheck.IsOutage` is now `IsPaused`.
- `toolcheck.Verdict.Requests` is how many requests the check sent to
  Decide, not counting automatic retries. `Verdict.Usage` keeps tokens when
  a check fails.
- **OpenAI Responses results include the service tier** the request ran on
  (`Result.ServiceTier`), in blocking and streaming calls.
- **Tools with no arguments now run on Anthropic when streaming.**
- **Conversations that used tools on Anthropic can continue on another
  provider.** Tool calls made by Anthropic's code execution tool still can't be moved.
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
