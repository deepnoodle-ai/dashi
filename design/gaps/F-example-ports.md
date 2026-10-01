# F. Findings from porting Dive's examples

Review of Dashi main at c591198 on 2026-09-28, written while porting nine of
Dive's example programs to `examples/`. Findings are directions, not
decisions. Several overlap with [B](B-developer-experience.md); this report
confirms them from real example code and adds new ones.

## How to read this

Each item has a **severity** and a **scope**.

- **High:** blocks a common task, silently does the wrong thing, or roughly
  doubles the code a common task needs.
- **Medium:** friction nearly every user hits, with a workaround.
- **Low:** polish.
- **Design:** changes public behavior or adds a concept; needs a design note,
  PRD, or decision before code.
- **Small:** a helper, a field, a doc comment, or a local fix.

Overall: Dashi is more correct than Dive (approvals bind to exact arguments,
turns resume across processes, cost says where each number came from) but asks
too much for simple cases. The approval example is about 220 lines against
Dive's 60.

## Status (2026-09-28)

Every finding in sections 1–11 is fixed on main; the findings below are kept
as written for the record. The approval example is now 99 lines, and the
durable (settle-later) approval example is 86.

| PR | Findings | What changed |
| --- | --- | --- |
| #46 | 1.1, 1.2, 1.3, 2.1, 3.1 | Generated IDs; `Agent{}` works; default and `catalog.Small` models; a nil Policy allows registered tools; `llm.ErrNoAPIKey`. Also fixed provider keys sent to other providers. |
| #45 | 4.1, 4.2, 4.3, 8.1, 8.2, 9.4, 9.5 | One error (`*CommitError`); `Outcome` carries the view, `Answer`, typed `Pending`, and `Spend`; `Pending.Approve()`/`Deny(reason)`; `RequireApproval`; `Turn.Citations()`, `Turn.HostedUses()`. |
| #41 | 3.2, 3.3, 3.4, 3.5 | `tool.List`; `tool.Succeeded`/`Failed`/`Waiting`/`Detached`; documented `tool.Func` rules; one hosted-tool constructor shape; tools guide. |
| #40 | 10.1 | `tool.Report` and `Event.Progress`, live-only, latest-wins per call. |
| #42 | 7.1, 7.2 | `filerecorder` package; exported `Step.Equal`. |
| #43 | 5.1, 9.1, 9.2, 9.3, 9.6, 9.7, 9.8 | `llm.Settings.Citations`; Anthropic text documents; `llm.Image`/`llm.Document`; `MustOutputSchema[T]`, `DecodeOutput[T]`; MIME detection; `ContextOrigin` constants. |
| #44 | 11.1 | Hosted requests priced by meter key; `Cost.Partial()` and `Spend.CostPartial` mark the unknown part. |

New from this work:

- **11.2 Medium · Design. Hosted tools cannot be admitted under a cost cap.**
  Under `MaxCost`, a request that declares hosted tools stops `limited` before
  it is sent, since its worst case has no bound. Live Anthropic web search showed input is rebilled on
  each internal pass (27,702 input tokens billed against a 16,037-token final
  context) and `max_uses` caps billed uses, not passes. A safe bound needs a
  provider-enforced pass limit (xAI `max_turns` is one). See
  [hosted tool pricing](../hosted-tool-pricing.md).

Follow-ups found while fixing (Low · Small unless noted):

- The schema generator behind `tool.Func` marks pointer fields `nullable`,
  which the draft 2020-12 validator ignores, so `null` is refused; it should
  emit `type: [X, "null"]`. It also fails on recursive collections, which
  `tool.Func` now refuses up front.
- Gemini hosted tools on a custom-named Vertex client need `Provider` set;
  only the default-named client works with zero options.
- Anthropic `CodeExecutionToolOptions.Type` exposes the legacy sandbox type.
  Newer web search and fetch versions make code execution free; Dashi still
  sends the older versions.
- OpenAI `image_generation` image tokens are not priced.
- Anthropic URL documents with no MIME type are assumed to be PDF; the URL's
  extension could be checked at prepare.
- OpenAI hosted tools are bound to the `openai` provider name, so
  OpenAI-compatible destinations cannot use them.
- The Gemini live tests have not been run (no key available).

## Priorities

| #    | Item                                         | Severity | Scope  |
| ---- | -------------------------------------------- | -------- | ------ |
| 1.1  | Required IDs for the simplest turn           | High     | Design |
| 8.1  | Approving a call takes six steps             | High     | Design |
| 4.2  | Outcome, Turn, and TurnView split the result | High     | Design |
| 4.1  | Two error channels on every Run              | High     | Design |
| 3.1  | Tools are silently denied without a Policy   | High     | Design |
| 10.1 | Tools cannot report progress                 | High     | Design |
| 11.1 | Hosted tool use makes a call's cost unknown  | High     | Design |
| 9.1  | Document citations cannot be requested       | High     | Design |
| 8.2  | A denial reaches the model without a reason  | High     | Small  |

## 1. Agent definition and configuration

- **1.1 High · Design. Too many required IDs for a first program.** A turn
  needs an agent `ID`, agent `Revision`, turn ID, and a `CommandID` on every
  command. Each matters for recovery, but a one-process program must invent
  them (`"example"`, `"1"`, `"example-start"`). Direction: generate them when
  empty; require them only with a durable recorder or when recovering.
- **1.2 Low · Small. No default model.** `Model` is always required, and
  `catalog.Default` returns flagship models, not inexpensive ones, so examples
  hardcode names. Direction: default from the catalog, and consider a
  "cheap" default for examples and tests.
- **1.3 Low · Small. A missing API key surfaces as a 401 on the first call.**
  Constructors succeed without a key. Direction: fail or warn in `New`.

## 2. Model interaction

- **2.1 Medium · Small. Provider constructors return different concrete
  types.** Choosing a provider at runtime needs a generic `asClient` helper to
  get an `llm.Client` and to avoid wrapping a nil pointer in a non-nil
  interface (`examples/providers`). Direction: document the pattern or ship
  the helper; a name-based provider registry would be a Design item.

## 3. Tool definition and execution

- **3.1 High · Design. A nil Policy denies every tool call, silently.** Every
  tool example sets `Policy: dashi.AllowLocalPolicy{}` with a comment
  explaining why. This conflicts with "works by default". Direction: allow
  registered local tools by default and make approval opt-in; at minimum,
  warn when `Tools` is set without a policy.
- **3.2 Medium · Small. No built-in fixed tool list.** `Runner.Tools` takes a
  `tool.Toolset` interface; all five tool examples and
  `internal/providertest` define the same three-line slice type. Direction:
  `tool.List` (a slice type) or `tool.Static(...)`.
- **3.3 Medium · Small. Returning success is verbose.**
  `tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput(s))}`,
  and the Go 1.26 `new(expr)` form is unfamiliar. Direction:
  `tool.Succeeded(text)` and similar constructors.
- **3.4 Low · Small. `tool.Func` schema rules are undocumented.** Fields are
  required unless tagged `omitempty`; `description` and `enum` tags are read.
  This is learned only from the schema generator's source. Direction: state it
  in the `tool.Func` doc comment.
- **3.5 Low · Small. Hosted tool construction is inconsistent.** A hosted
  tool is a `tool.Tool` with only `Definition` set, which does not explain
  itself. `messages.NewWebSearchTool` returns an error but
  `NewCodeExecutionTool("")` does not, and the empty-string argument is
  unexplained. Direction: one constructor shape, and a doc comment on what a
  definition-only tool means.

## 4. Execution lifecycle and control

- **4.1 High · Design. Two error channels.** Every example checks the error
  from `Run` and then `outcome.RecordingErr()`; the second is easy to forget.
  Direction: return recording failures as the error, still with a usable
  outcome, or otherwise make the split impossible to miss.
- **4.2 High · Design. The result is split across three nouns.** The answer is
  on `outcome.Turn.AnswerText()`, pending calls are bare IDs on `Outcome`, and
  details and spend need `dashi.Replay(turn)` to get a `TurnView`. For a
  library that sells cost tracking, `outcome.Spend` should exist. Direction:
  `Outcome` carries the view; `Replay` becomes a recovery tool.
- **4.3 Low · Small. Restart deduplication is not obvious.** Re-running a start
  returns the recorded result without re-running tools only when the same
  recorder and the same `CommandID` are supplied. Direction: say so in the
  running-turns guide.

## 5. Context construction and management

- **5.1 Low · Small. `ContextItem.Origin` is an untyped string.** Values such
  as `"user"`, `"tool"`, and `"operator"` have no constants, unlike
  `StartOperator`. See B's Origin row for the naming direction.

## 7. Execution history and recovery

- **7.1 Medium · Design. No durable recorder ships.** Resuming in another
  process means writing one; `examples/resume` needed about 150 lines. The
  conformance suite made it quick to verify. Direction: decide whether to ship
  a reference file or Postgres recorder, or promote the example's.
- **7.2 Medium · Small. The canonical JSON comparison is internal.** A custom
  recorder must compare steps by JSON meaning, but the helper is unexported.
  Direction: export it for recorder authors.

## 8. Policy, authorization, and human interaction

- **8.1 High · Design. Approving a call takes six steps.** Replay the turn;
  look up `view.Tools[id]`; copy `ArgsDigest` and `Decision.Revision`; pass
  `CallID` twice (on `Resolve` and `ApprovalDecision`); `Resolve` with its own
  `CommandID`; check `ReadyToContinue`; `Continue`. The binding is right; the
  caller should not have to know it. Direction: approval items on the outcome
  with `Approve()` and `Deny(reason)` that build the command.
- **8.2 High · Small. A denial reaches the model without a reason.** A denied
  call is recorded as "not executed" with no text, and the model guessed at
  network trouble and offered to retry. `examples/approval` works around it
  with a `AddMessages` note. Direction: a `Reason` on the approval, shown to the
  model. (A recorded-format change, which is free before release.)

## 9. Content and outputs

- **9.1 High · Design. Document citations cannot be requested.** Anthropic
  returns document citations only when the request enables them per document;
  `content.Document` has no such field, and the provider's raw-field escape
  hatch needs an unexported destination identity. Reading citations works.
  This is a regression from Dive's `citations_example`.
- **9.2 Medium · Small. Plain-text documents fail on Anthropic.** Inline
  documents are always sent as base64, which Anthropic accepts only for PDFs;
  text needs its text-source form. The failure appears at send, not at
  prepare.
- **9.3 Medium · Small. Content construction is layered.**
  `llm.UserMessage(llm.Block(content.ImageBlock(src)))` stacks constructors
  from two packages. Direction: `llm.Image(src)` and `llm.Document(src,
title)` beside `llm.Text`.
- **9.4 Medium · Small. `AnswerText()` joins text blocks with no separator.**
  Around hosted tool calls the output reads `…53^4.**Tanzania's…`.
- **9.5 Medium · Small. No helper for citations or hosted activity across a
  turn.** Callers walk `turn.Steps`, filter `StepModelCallFinished`, and search
  content by hand.
- **9.6 Medium · Small. No schema-from-struct helper for structured output.**
  `Agent.OutputSchema` takes raw JSON, so users write it by hand or import the
  schema generator `tool.Func` already uses internally.
- **9.7 Low · Small. `content.DocumentBlock` takes no title.** The example sets
  `block.Document.Title` afterward.
- **9.8 Low · Small. Inline bytes need a MIME type.** Users call
  `http.DetectContentType` themselves; a bad guess is caught only by the
  provider at send.

## 10. Streaming and observability

- **10.1 High · Design. Tools cannot report progress while running.**
  `Execute` returns only a final outcome, and `Event` carries no tool
  progress; `tool_progress_example` could not be ported. See the
  [tool progress proposal](../tool-progress.md).

## 11. Resource limits and accounting

- **11.1 High · Design. Any hosted tool use makes a call's cost unknown.** The
  catalog prices tokens but not hosted requests, so `Pricing.Estimate` reports
  the whole call as unknown when `Usage.Requests` has an unpriced entry, even
  though the token cost is known. Request counts are keyed by the name the
  model called (`web_search`, `bash_code_execution`), not by tool type, so a
  price sheet would have to match those names. Direction: price hosted
  requests in the catalog, key requests by tool type, and report a known
  partial cost with the unknown part marked.
- See 4.2 for `Spend` missing from `Outcome`.

## 12–13. Not ported, because Dashi has no equivalent yet

Each is Design scope; listed so the gaps stay visible.

- **Tool progress** (`tool_progress_example`): see 10.1.
- **Local MCP servers** (`mcp_servers_example`): Dashi reaches MCP only
  through provider-hosted connectors.
- **Hooks** (`hooks_example`): PRD in [hooks](../hooks.md).
- **Skills** (`skills_example`) and **subagents** (`subagent_example`).
- **Background tools** (`background_example`, `background_polling_example`):
  detached calls cover part of this.
- **Speech** (`openai_text_to_speech_example`, `google_text_to_speech_example`,
  `transcription_example`): `speech` has values but no transport.
- **Gemini** (`google_example`, `google_tool_example`): supported, but a
  separate module, so left out of `examples/providers`.
- **OpenTelemetry**, **A2A**, and **OAuth client** examples.
