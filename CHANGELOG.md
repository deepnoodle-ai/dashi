# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Changed

- **Dialogs draw on the prompt's band.** Approvals, questions, pickers, the
  session picker, the MCP startup review, and the inspector that opens over
  a pending dialog take the composer's place and its gray background,
  instead of starting with a rule. Each title starts with a glyph in its
  role color: a yellow `?` when Dashi needs an answer, a blue `◆` for a
  choice you opened. The title itself is plain bold. The selected row has
  a blue `❯` and a bold label, and approval keys read `y Allow once` with
  the letter in blue and no brackets. Key hints sit below the band.

## [0.13.0] - 2026-10-24

### Changed

- **The CLI has one color theme with a blue accent.** Headings, links,
  selections, and running work use one blue; yellow now marks only what
  needs you, such as approvals, questions, and limit stops. Denied calls,
  interrupts, queued steering, and footer costs are quieter. Inline code
  shows on a gray background, and code blocks use matching syntax colors.
- **Markdown replies render more of their formatting.** Code blocks are
  gray panels with their language on the first line, quotes have a bar at
  the left, and `~~strikethrough~~` is struck through instead of showing
  its tildes. Dashi now uses Wonton v0.9.0.
- **Tool rows say what happened.** A glyph carries the state (green `●`
  done, red `✗` failed, `⊘` denied, a spinner while running), so a
  successful call shows only its time. A result line under each row gives
  the outcome: your answer to a question, `214 lines` for a read, `+3 -1`
  for an edit, a command's last output line, or the error of a failed call.
  It offers `click to expand` only when the full output has more. Replies
  and tool rows now stay within 100 columns on wide terminals.

## [0.12.2] - 2026-10-24

### Added

- **Install the CLI with Homebrew:** `brew install deepnoodle-ai/tap/dashi`.
  Each release now attaches `dashi` binaries for macOS, Linux, and Windows,
  and `dashi --version` reports the release in those builds.

## [0.12.1] - 2026-10-24

### Fixed

- **The CLI picks up Wonton v0.8.1's terminal fixes.** A width-3 or width-4
  dash (`⸺`, `⸻`) in a reply now clears the columns it covers, so text drawn
  there before no longer shows through.

## [0.12.0] - 2026-10-24

### Added

- **`session.Session.StreamQueueUnlessUserQueued`** is
  `RunQueueUnlessUserQueued` with events, as `Stream` is `Run` with events.

### Fixed

- **Schemas include fields from embedded structs of unexported type.** For
  a type such as `type input struct{ base }`, `tool.Func` and
  `MustOutputSchema` now list `base`'s exported fields, as `encoding/json`
  decodes them. Before, a strict tool or output schema rejected every input
  that set them. Wonton v0.8.0 brings the fix, and also stops a recursive map
  or slice type from crashing schema generation.
- **Gemini Live sessions report only real responses.** An interruption or
  turn end with no response in progress no longer sends `EventInterrupted`
  or `EventResponseCompleted` for the previous response, and does not drop
  the next response's audio. Tool calls that arrive after an interruption
  are discarded with the rest of that turn.
- **Grok voice `CommandRespond` no longer gets stuck.** A provider error
  after `response.create` lets the caller ask again, and speech before the
  new response starts no longer interrupts the finished one.
  `CommandInterrupt` sent after `CommandRespond` but before the response
  starts now cancels the requested response.
- **Realtime sessions survive an expired call context.** A `Send` or
  `Receive` whose context expires while it waits for another call, or after a
  frame that produced no event, returns `ErrorCanceled` and keeps the
  session. After a session fails, `Receive` returns `ErrClosed` instead of
  events queued before the failure.
- **Realtime setup timeouts are transport errors.** A provider that never
  acknowledges setup within 20 seconds is `ErrorTransport`, not
  `ErrorCanceled`.
- **Media responses with `"usage": null` are accepted.** Gemini media, Grok
  Imagine, and OpenAI Images treat it as unreported usage instead of failing
  with `ErrorProtocol`. Provider packages use the new `media.ReportedUsage`
  to fill `RawUsage`.
- **Grok Imagine checks more of what it sends and receives.** `SubmitVideo`
  reports `SubmissionRejected` for failures before dispatch, such as a
  missing key or canceled context. `PollVideo` rejects the job IDs `.` and
  `..`. Image responses with a non-image `mime_type` fail, a zero or
  pre-1970 output expiry counts as no expiry, and preparing an image with a
  catalog video model (or the reverse) fails with `ErrorUnsupported`.
- **OpenAI Images rejects catalog models without image capabilities**, and
  OpenAI speech reports a call canceled while reading an error response as
  `ErrorCanceled`.
- **Resolve refuses a model result the turn would reject before saving it,**
  at any stop, not only at an open limited stop. A partly streamed call
  resolved as a failure, or a result that repeats a tool call ID, no longer
  reaches a recorder that does not check what it stores.
- **A summary call can be resolved with a result that holds no usable
  summary.** Resolve records it as a failed summary, as the runner does, so
  Continue retries it or falls back instead of leaving the turn stuck at
  `StopResultUnknown`.
- **A hook or recorder that stops a turn at a cost or token limit the
  turn's budget does not set no longer wedges it.** The stop is a hold:
  `Next` gives `NextFix` and `Continue` retries without a raise.
- **Continue retries a failed model call after an interrupted stop**
  instead of failing again with the stale error.
- **A resumed run whose cut-off summary covers a content reference falls
  back to a summary fallback** instead of failing the turn.
- **Active compaction keeps a provider pause chain that asked for a
  provider tool approval,** so the approval response is never orphaned.
- **BeforeSend hooks see the context items BeforeModel added** for the
  same call.
- `Turn.Citations` copies each citation's `Location`, and a summary
  fallback no longer repeats an assistant reply the user already answered
  as `[your last reply]`.
- The replay cache no longer shares a provider tool approval order between
  copies when a provider reuses a resolved approval ID.
- **Advance and Drive keep steers the turn did not record.** A steer that
  arrives as the turn ends is no longer refused and deleted; it stays in the
  inbox for the next turn (`MemoryInbox.Leftovers`). When Advance returns,
  an `InboxReleaser` gets back the steers no run took, so `Withdraw` works
  on them again. A steer in a write whose result is unknown comes back as
  in doubt, never as unrecorded.
- **A Continue from the inbox counts as Advance's own run.** Advance no
  longer runs a second Continue, or retries a hold, right after it.
- **Active compaction's size estimate never records a stop.** A content
  reference that fails to load during the estimate no longer writes
  `StepTurnStopped` mid-turn or breaks the turn record. If the request is
  too large to send without compaction, the turn stops with that
  reference's failure, not with a context-window limit.
- **A crash right after a provider pause resumes cleanly.** The restart
  reminder is left out while a provider tool call is unfinished, so the
  request still ends with the paused result.
- **BeforeModel hook settings are checked against the call they apply
  to.** A tool choice that takes away the structured-output submission
  tool skips the hook with `WarningHookFailed` instead of failing the turn.
  A named tool choice on a summary or reserved final-reply call is cleared,
  as the runner clears it, instead of failing the hook.
- **BeforeModel items without an ID are recorded once per model call.**
  The runner names each one from the turn, the ordinary model call, the
  hook, and the item's position. When an urgent steer or active compaction
  sends a pass back, or a held turn continues, the hooks' next pass no
  longer adds the item a second time.
- `feed.Follow` waits for the record's own `StepWaiter.Wait` before it
  returns. A summary call's start no longer carries BeforeModel results
  from the ordinary call it replaced. A long tool argument error is cut at
  a rune boundary.
- **An unpriced model call that ran provider tools counts as unknown
  cost,** even when the provider reported no usage, so a cost cap no longer
  treats that work as free.
- **The next call's cost estimate bounds `max_tokens` by the output tokens
  left in the budget,** as the runner does before sending, so steers and
  hook messages are not refused for a call that would fit.
- **The task budget sends `Remaining` after any active compaction,** even
  one whose replaced messages counted no tokens.
- **`Agent.Validate` checks `Agent.Settings`.** A NaN or infinite
  temperature, a negative `MaxTokens`, or an invalid task budget now fails
  at Start instead of at every model call.
- **`edit_file` edits CRLF files.** A multi-line `old_string` copied from
  `read_file` uses `\n`; the edit now matches the file's `\r\n` lines and keeps
  them.
- **Edit previews show final-newline changes and hide `\r`.** The diff marks a
  last line with no newline, as git does, and drops `\r` from CRLF lines.
- **`grep` refuses a bad glob without rg too**, and `glob` no longer stalls on
  repeated `**` segments.
- **`toolcheck` reads every case variant of the `command` key.** Decoders
  differ on which of `"command"`, `"Command"`, or a repeated key they run, so
  `CommandRisk` and `PromptInjection` now check all of them, and a case alias
  can no longer hide the command a shell runs.
- **`judge_items` validates Decide answers** even when the host's client turns
  validation off, so an out-of-range probability fails its item.
- **`turnwatch` sees its earlier reminders after a summary** replaced the tool
  calls before them.
- **`contextrank` marks a candidate whose content is not valid UTF-8 as
  invalid**, instead of the runner refusing every chosen item.
- **MCP keeps the transport the user approved.** An entry with a command and a
  url stays stdio when the command is an unset variable; it fails instead.
- **MCP `Refresh` with an ended context keeps a failed server's error and
  stderr**, and a tool-list change that arrives during a listing stays marked.
- **Mentions keep the line reference of a quoted path.** `@"main.go:42"`
  becomes `[file: main.go]:42` instead of garbled text.
- **`mention.ReadFile` reports a size limit below the file signature as too
  large**, not as a signature mismatch.
- **A cut-off OpenAI Chat Completions stream no longer returns runnable tool
  calls.** Every tool call in a stream that ended before its finish reason is
  partial opaque content, even when its arguments happen to be valid JSON.
- **A call that was definitely not sent has no stop kind.** Anthropic
  Messages, OpenAI Responses, and OpenAI Chat Completions omit `StopIncomplete`
  only when the error's send status is `llm.SendStatusNotSent`, such as a
  missing key or a context cancelled before dispatch, so such a call is not
  mistaken for a dispatched one. Any HTTP status, a connection dropped after
  the request was written, or a response-start timeout may have reached the
  provider, so those calls keep `StopIncomplete`.
- **Malformed provider output is a protocol error.** A stream event that is
  not JSON or exceeds the size limit, and inner content that does not decode,
  now fail with `llm.ErrorProtocol` instead of `llm.ErrorTransport` or a bare
  error.
- **Stream error events carry the provider's message.** Anthropic and OpenAI
  Responses `error` events keep the provider's message in `err.Error()`, and
  Responses also keeps its error `Code`.
- **Anthropic and OpenAI Responses reject duplicate tool IDs.** A response
  that reuses a tool call, provider tool use, or approval request ID fails as
  a protocol error, as Chat Completions already did, instead of recording
  history that no later request can send.
- **Interrupted OpenAI Responses streams keep message parts and report
  incomplete usage.** Parts that arrive with `response.output_item.added` are
  kept, a text delta cannot overwrite an unknown part, and usage seen before a
  terminal event has `RequestsComplete` false.
- **Anthropic streams keep block types and usage fields.** An interrupted
  `server_tool_use` or `mcp_tool_use` block keeps its own type in its partial
  data, tool arguments for a non-tool block fail the call, and a null usage
  field in `message_delta` no longer erases the value from `message_start`.
- **OpenAI strict output schemas check enum values against their type.** An
  enum value the node's type cannot hold, or an enum on an object or array,
  fails `Prepare` instead of the request.
- **Gemini tools with no arguments work.** Gemini omits `args`, or sends
  null, when it calls such a tool. The call now carries `{}`, so the tool runs
  and the next request replays the call.
- **Gemini streams keep every part.** A code-execution part that carries a
  thought signature after text is no longer merged into the text and lost. A
  stream whose response fails to decode reports a protocol error instead of
  an empty result. Delta indexes on both stream paths now match the parts of
  the final message.
- **Gemini errors are classified like other providers' errors.** The status,
  `Reason`, and `RetryAfter` come from the HTTP response, whatever shape its
  body has, so a context overflow in an array or plain-text body now reports
  `ReasonContextWindowExceeded`. A call canceled before dispatch reports
  `SendStatusNotSent`. An error inside a stream Google accepted is no longer
  `SendStatusProviderRejected`. Only a call that was definitely not sent has
  no `Stop.Kind`; a connection dropped after dispatch keeps
  `StopIncomplete`.
- **Gemini results invent no provenance.** An empty response has no
  `Message.Source`, and `Served` stays empty when Google names no model.
- **Gemini rejects tool calls left unanswered before the next model turn**
  at `Prepare`, instead of sending a history Google rejects.
- **Gemini calls have HTTP timeouts like the other providers.**
  `gemini.Config` has a `Timeouts llm.HTTPTimeouts` field. The zero value
  bounds each call to five minutes. A supplied `Client` with a nil `Total`
  keeps only its own timeouts, and an explicit zero `Total` removes the bound.
  Timeouts report `ReasonResponseStartTimeout`, `ReasonStreamIdleTimeout`, or
  `ReasonCallTimeout`.
- **Vertex AI uses the right host for the `us` and `eu` multi-regions**, and
  sends the credential's quota project as `X-Goog-User-Project`.
- **A missing API key keeps the result's facts.** Gemini, Grok, Meta,
  DeepInfra, Mistral, and OpenRouter return a result that names the requested
  model and repeats the request's warnings. Gemini media errors name the
  configured provider. DeepInfra's `Prepare` reports cancellation and invalid
  input before its document check.
- **CLI turns started by a finished background task stream live.** They now
  show output as it arrives and write `--turn-events` lines, bash git diffs,
  and session warnings, like the turns you start.
- **CLI approval prompts keep their mode when an earlier prompt is
  canceled.** The next prompt opens only after the canceled one's dialog has
  closed, so Ctrl-C and `--plain` answers reach the open prompt.
- **`/new` starts from your launch defaults.** It no longer keeps the model
  and generation controls of a session opened with `--continue`, `--resume`,
  or `/sessions`. A session switch also applies the selected session's
  preferences before it opens, and `/settings` no longer repeats overridden
  sources after each switch.
- **`--plain` no longer prints your prompts twice after `/new` or
  `/sessions`.**
- **`dashi run -c` skips a session whose first turn never recorded**, such
  as one a failed first attachment request left, and reopens the latest
  session with turns.
- **`dashi run -c` with a new request no longer outputs the reply of the
  turn it closes first.**
- **`/task ID continue` runs the decide command-risk check on a stopped
  task's recorded bash approval**, also with
  `--dangerously-skip-permissions`, and asks about a flagged command. The
  prompt shows the check's result, but the turn record does not keep it as
  it does for a live call: a recorded approval decision has no field for it.
- **`--turn-events` with redirected input now fails with a hint to add
  `--plain`**, instead of being ignored.
- **CLI attachment drafts survive more edits.** A slash command written in the
  external editor (Ctrl+G) no longer clears the draft's staged `@` mentions.
  `/attach PATH:START-END` on a file already mentioned in the draft keeps its
  line range once the mention text is removed.
- **Clearing the composer keeps staged `@` mentions.** After Ctrl+U you can
  run a slash command such as `/attachments` and get the draft back, or type
  a new prompt that the mentioned files go with. Enter on the cleared line
  sends nothing and shows a hint. A submit or `/detach` drops the mentions.
  Enter on an empty composer after `/attach` still sends the selections
  alone.
- **Session steers are no longer stranded.** A steer sent to a turn that
  never started, or to a turn that ended without the handle seeing it end
  (a lost acknowledgement, or another worker), now moves to the queue.
- **A turn a `session.Session` handle is starting counts as running.** Until
  its first step is recorded, `Open` reports `NextRunning`, and a second
  Start, a `Continue`, `Compact`, and `RunQueue` wait for it instead of
  planning the same turn.
- **`WarningTurnClosed` arrives exactly once.** It was lost when the
  Compactor found nothing to compact, and repeated in `Stream` when a
  compaction turn ran.
- **Compaction keeps provider tool approval pairs together.** The kept tail
  no longer starts at a `ProviderToolApprovalResponse` whose request was
  summarized, and tail sizes no longer overflow on huge media references.
- **`compaction.Compactor.StartIfNeeded` can use the conversation's model.**
  With an empty `Model` and the new `Compactor.Client` set to the runner's
  client, the Start uses the newest user turn's provider, API, and model
  while they match that client, as a Session does. Without `Client` they
  stay empty, and the runner fills them.
- **`delegate.Tasks.Run` reports history warnings.** The warnings of
  building a child's earlier history are in the result's `SessionWarnings`.
- **`delegate.Tasks.Run` continues later child turns with their history,**
  and a retried `Continue` that sets `OnHistoryMismatch` is a retry, not a
  reused command ID.
- **The parent learns when `delegate.Tasks.Run` finishes a child turn,**
  even when the turn's earlier waiting stop already used its notice. Each
  later stop gets a `TaskEnded` event and a notice of its own, whose ID is
  the new `TaskView.NoticeID`; `task_status` records that ID as delivered.
  Format change: the `task_status` outcome field `delivered_turn_id` is now
  `delivered_notice_id`, and a read recorded by 0.10.0 or 0.11.0 no longer
  counts as delivered, so `Recover` may send that notice again.
  The explicit run also reads the child's inbox, and mail it leaves unread
  starts the next turn, as after any turn.
- **A reused session command ID must repeat the caller's input exactly.**
  `Start.QueuedInputCount` records how many queued user messages a
  `session.Session` put ahead of the caller's input, so a Start whose input
  is only a suffix of the recorded input, or empty, is `ErrCommandIDReused`
  rather than a retry. A turn recorded before this change has no count, so
  a retry of a command that had queued messages ahead of its input is now
  `ErrCommandIDReused` too.
- **`filerecorder.Recorder.Wait` sees a commit that replaces a torn tail of
  the same length,** and lock waits report `context.Canceled` rather than
  the context's cause.

## [0.11.0] - 2026-10-23

### Added

- **Realtime sessions can start with earlier conversation.**
  `realtime.Config.History` takes `realtime.HistoryItem{Speaker, Text}` lines,
  sent after setup without asking for a response: one Gemini `clientContent`
  under `historyConfig.initialHistoryInClientContent`, or one Grok
  `conversation.item.create` per line. `realtime.MaxHistoryBytes` (256 KiB)
  bounds the total text.
- **Gemini Live on Vertex AI.** `geminilive.Config.Vertex` takes a
  `geminilive.VertexConfig{Project, Location, TokenSource}`; Connect dials the
  regional Vertex endpoint with a bearer token from the `oauth2.TokenSource`
  and names the project-scoped model. The API key path is unchanged.
  `geminilive.Config` is now a struct of its own rather than an alias, with the
  same fields plus `Vertex`.

### Changed

- **Larger realtime tool and instruction limits.** `realtime.Config.MaxTools`
  and `MaxInstructionBytes` set the bounds; zero means
  `realtime.DefaultMaxTools` (128, was 32) and
  `realtime.DefaultMaxInstructionBytes` (256 KiB, was 64 KiB).
  `realtime.MaxPendingTools` (32) now bounds only tool calls awaiting results.
- **Gemini text commands no longer interrupt the model.** `CommandText` is sent
  as `realtimeInput.text` instead of `clientContent`.
- **Realtime usage carries modality detail, so audio and text can be priced
  apart.** Grok usage is read from beside `response` in `response.done`,
  where xAI sends it (it was never reported before), falling back to
  `response.usage`; its text, audio and grok token details become
  `llm.Usage.ModalityTokens` under `text`, `audio` and `grok`. Gemini usage
  reads `promptTokensDetails`, `cacheTokensDetails`,
  `toolUsePromptTokensDetails`, and `responseTokensDetails` or Vertex's
  `candidatesTokensDetails`, and `candidatesTokenCount` when
  `responseTokenCount` is absent. Gemini buckets match the batch Gemini
  provider: thoughts count as output and as `ReasoningTokens` (attributed to
  text), cached prompt tokens as cache reads. The modality-incomplete flags
  are false only when the detail adds up to the totals.
- **Grok voice defaults to `grok-voice-3.0`.** `voice.DefaultModel` was
  `grok-voice-think-fast-2.0`, which xAI no longer lists and silently serves
  as the deprecated `grok-voice-2.0`. A session's `Info().Model` now reports
  the model named in Grok's `session.updated`, which is the model actually
  serving, and keeps the requested id only when the echo omits it. A
  `Config.Model` naming the old id now fails with `ErrorUnsupported`.
- **A provider's close reason reaches the caller.** A WebSocket close with a
  status other than normal, such as Vertex refusing a model at a location,
  returns an `ErrorProvider` with `Code` `close_<status>` and the reason as
  `Cause`, instead of a bare `ErrorTransport`. A Vertex token failure carries
  the OAuth error code, never the error text.

## [0.10.0] - 2026-10-23

### Added

- **Claude task budgets.** `llm.Settings.TaskBudget` gives the model an
  advisory token budget it can see for a whole Turn, sent as Anthropic's
  `output_config.task_budget` with its beta header. Every ordinary call of the
  Turn sends the same budget; summary calls send none, and after active
  compaction the runner sends `Remaining`, computed from the recorded Turn so a
  recovered Turn sends the same value. Models and APIs without task budgets,
  Ollama included, omit the setting with a warning, and a total below
  Anthropic's 20,000-token minimum is raised with a warning; both are errors
  under `CapabilityStrict`. `messages.Config.Unsupported` lets a
  Messages-compatible destination declare features it rejects, and
  `WarningTaskBudgetAdjusted` records a budget the runner adjusted instead of
  failing the Turn.

### Fixed

- **Grok voice sessions no longer end when the provider sends a large audio
  chunk.** A chunk over `realtime.MaxAudioBytes` is split into frames.
  `MaxAudioBytes` rises from 64 KiB to 256 KiB (about 5.5 s of 24 kHz PCM16),
  which also raises the cap on caller audio input by the same amount.
- **Realtime errors say why a session failed.** Grok and Gemini error events
  carry the provider's error code and message, and the new `realtime.Failf`
  names the failing check in protocol errors.

## [0.9.0] - 2026-10-14

### Added

- **The CLI shows a command menu while you type a slash command.** Under the
  composer it lists up to five matching commands and skills with their
  descriptions, ordered as Tab completion orders them. ↑/↓ move the
  selection, Tab completes the name, Enter runs a command that needs no
  argument or fills in one that does, and Esc closes the menu and keeps the
  draft.
- **`dashitest.Client` streams its replies, and `dashitest.CallTools` scripts
  parallel tool calls.** When the input asks to stream, as `Runner.Stream`
  and a runner with an observer do, each reply is first reported as text,
  reasoning, tool-argument, and usage deltas, so live views can be tested
  without a provider. The final result is unchanged.

### Changed (breaking)

- **`mcp`: tool names longer than 64 characters change.** A name of the form
  `mcp__<server>__<tool>` that is over 64 characters is cut and given a hash
  suffix. The suffix now hashes the raw server and tool names, not the forms
  with characters replaced by `_`, so two long names that differ only in those
  characters no longer collide. Every shortened name therefore differs from
  v0.8.0: update permission rules, hooks, and saved references that name one.
  Names of 64 characters or fewer are unchanged.

### Changed

- **Catalog**: OpenAI entries now claim image input, tools, and structured
  output where each model page states them, and Gemini entries claim image,
  audio, PDF document input, tools, structured output, and (on Gemini 2.5)
  a thinking budget. These features were unknown before, so the adjust
  policy sent them and recorded them as unverified; now they are verified,
  and the strict policy accepts them.
- **Catalog**: o3, o3-pro, o3-mini, o4-mini, gpt-5-pro, and gpt-5.3-codex
  have standard-tier price sheets from OpenAI's pricing page, so their calls
  get cost estimates. A turn with a cost limit on one of these models now
  runs under that limit instead of stopping for want of a price.
  gpt-5-codex-mini is still unpriced because the page lists no rate for it.
- **`llm.Request` accessors no longer decode JSON on every call.** The
  snapshot is decoded once, on first use, and `Snapshot`, `Input`,
  `Effective`, `Adjustments`, `Warnings`, and `ContentChanges` return deep
  copies of it, so callers still own what they get. In
  `BenchmarkRequestAccessors` (the accessor calls one model call makes, one
  Apple M4 Max), they ran about 20 times faster with a sixth of the memory.
- **Views, events, and hook plans encode to snake_case JSON.** `TurnView`,
  `ToolView`, `RunResult`, `Event`, `ToolProgress`, and the hook `*Plan` and
  `*Decision` types now have `json` tags matching their field names, like the
  step facts, so an encoded view reads `"model_calls"` instead of
  `"ModelCalls"`. Optional fields are omitted when empty.
  `FinishPlan.OutputErr` encodes as the string `"output_err"`.
  `SendPlan.Request` is not encoded yet, because `llm.Request` has no
  exported fields.
- **`llm.Usage.Copy` is now `llm.Usage.Clone`**, matching every other deep
  copy in the library.
- **Long tool-calling turns run much faster.** A Run and the
  `MemoryRecorder` now rebuild a turn's state from only the steps added
  since the last rebuild, instead of replaying the whole record after each
  tool call and commit, and views given to hooks are copied directly rather
  than through JSON. A 396-step turn runs about 18x faster (8.0s to 0.44s
  in the benchmark). `Turn.View` still replays the whole record.

- **A compaction turn ends itself.** The engine records `Stop.Ended` on
  every stop of a compaction turn except a host hold (a host limit without
  `Ends`) and an interruption with no model call in flight. A failed
  summary therefore needs no host `Cancel`: its fallback stands in. A host
  only waits while the last turn has not ended, and `session` no longer
  closes a stopped compaction turn.
- **`summary_fallback` is recorded once**, on the stop that ends a
  compaction turn without a summary, so it is in that turn's `RunResult`
  and `TurnView.Warnings`. `BuildHistory` no longer raises it on every
  replay; `session` passes it on once, in the next turn's `SessionWarnings`.
- **Active compaction falls back instead of failing the turn.** When a turn
  cannot use the summary of its own finished work (a send failure, an
  invalid reply, or no budget for the shorter retry), it swaps in the
  summary fallback, records a `summary_fallback` warning on the
  `context_compacted` step, and keeps going. `ContextCompactionFact` gains
  `SummaryFallback`; exactly one of it and `Summary` is set.

### Removed

- **The CLI no longer prints the instructions and skill counts at startup.**
  `/resources` still shows sources and access scopes.

### Fixed

- **The CLI no longer lets model or tool text write terminal escape
  sequences.** A link or line containing control characters could clear the
  screen, set the title, or write the clipboard (Wonton v0.7.3).
- **`dashitest.Client.Inputs` returns independent copies.** Editing a
  returned input no longer changes what later calls report.
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
  the ordinary model call after the summary, so an urgent steer sent during
  a summary takes effect only once the summary finishes. A steer still
  waiting when the turn stops is not recorded; `MemoryInbox.Leftovers`
  returns it.
- A hook stop can no longer use the reasons Dashi gives its own failed stops
  `compaction_failed`, `empty_response`, or `incomplete_model_result`; like
  the other reserved reasons, such a stop is ignored with a hook warning.
- Comparing two values that do not encode as JSON (such as two different
  invalid argument texts) no longer reports them as the same; they match only
  when deeply equal.
- `MemoryInbox` now deep-copies queued `Start`, `AddMessages`, `Continue`, and
  `Resolve` commands on `Put` and on claim, as it already did for steers, so
  a caller changing its slices cannot change the queued command. `Put` now
  refuses a command that does not encode as JSON, which a turn could not
  record anyway.
- A video file reference from another provider scope is now refused when
  adapting history, as image, document, and audio references already were.
- Non-streaming model responses are read up to 256 MiB, and one streamed
  event (line or joined data) up to 64 MiB; a larger body fails the call
  instead of exhausting memory. Gemini bounds its response copy at 256 MiB
  and reports a larger response as a protocol error, as the other providers
  do.
- **`turnwatch`** counts a call that never ran (`tool.StateNotExecuted`),
  such as one with invalid arguments or one a hook denied, as failed, so
  repeating it unchanged triggers `failed_again`. A check with no recent
  activity to judge sends no Decide request; a turn whose messages hold
  none of its tool calls sends its latest reply instead. A P(yes) that is
  NaN or outside 0 to 1 fails the check rather than being compared.
- **`contextrank`** treats a P(yes) that is NaN or outside 0 to 1 as
  unanswered (`CauseUnanswered`). Before, with no floor, a NaN candidate
  was chosen.
- **`listtool`**: `judge_items` sets its open-world hint to false, so
  `toolcheck.PromptInjection` no longer sends its results to Decide. Its
  results now hold only item IDs, question keys, answers, and fixed
  messages: they no longer quote the query or the list name the model
  sent, and a failed read says "Could not read <list>." with the host's
  error kept in `Result.Err`. Question keys and `find` keys must be short
  identifiers (1 to 40 lowercase letters, digits, or underscores, starting
  with a letter), so an error that names a key carries no free text, and a
  bad type or `find` answer is not quoted back. `List.ID` should return a
  key the host assigns, not outside text. Both tools read their arguments with exact,
  case-sensitive keys, as the runner's schema check does, so a key such as
  `"List"` no longer replaces the checked `"list"`, and a repeated key
  fails the call as invalid arguments.
- **`filerecorder` keeps its in-process index current after a commit.** A
  commit now adds the step it wrote to the index, so the next commit and
  `Wait` need not read it back.

- **`workspace` tools bound their work.** `glob` and `grep` expand nested
  `{a,b}` groups and fail past 64 patterns; `read_file` cuts lines at 2000
  bytes and results at 256 KiB; `edit_file` refuses files over 32 MiB; the
  file tools refuse named pipes; walks and reads stop when canceled; and a
  shell `DefaultTimeout` is capped at `MaxTimeout`.
- OpenAI image edit and transcription requests now build the same body for
  the same input: the multipart boundary is derived from the form values
  instead of chosen at random.

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
  is raised each time a history is built from a compaction turn that ended
  without a summary.
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
  closed or one ended at a limit, not only after a completed turn. A
  stopped compaction turn is closed unless held for budget (a host limit
  without `Ends`); a held one waits, and a new Start refuses with a
  `*TurnBusyError`. A compactor `Start` error, or a compaction turn that
  cannot be recorded, is returned instead of running the next turn with
  the history that was due.
- **The CLI** compacts at 20% of the window by default;
  `--compaction-threshold` sets `Policy.DueTokens`. It warns once per
  compaction turn that ended without a summary.

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
