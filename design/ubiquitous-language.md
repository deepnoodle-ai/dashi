# Ubiquitous language

Public concepts grounded in the [conceptual model][model] and
[Dive's current API][dive]. These name what callers configure, supply, inspect,
and act on. Foundation terms follow the [accepted terminology][terminology];
the agent, runner, and command names follow [decision 30](decisions.md).
Session contracts remain proposed; see the
[sessions PRD](roadmap-research/14-session-continuity-prd.md).

## Define an agent

- **Agent**: Reusable agent configuration (`dashi.Agent`): identity,
  revision, instructions, model, settings, output schema, and limits. It is
  plain JSON data, recorded when a turn starts, not a running object.
- **Runner**: The value that runs turns (`dashi.Runner`): an agent plus its
  long-lived services, such as the client, tools, recorder, hooks, and
  observer. `Run` collects a turn's result; `Stream` yields it as it happens.
- **Model and settings**: The selected model and controls for generation,
  such as reasoning, output tokens, and tool choice.
- **Model reference**: A configured provider identity paired with a model name.
- **Provider identity**: The application's stable name for a configured
  destination, such as `azure-production`.
- **Client**: A configured object that prepares model input and sends requests.
- **API**: The wire format a client speaks, such as `openai.responses`.
- **Tool**: A named operation the model can request, with a **tool definition**
  describing its purpose and input schema.
- **Toolset**: A named source of tools resolved before each model call.
- **Skill**: Named task instructions loaded into context when invoked.

## Work with turns and sessions

- **Turn**: One input processed until the agent settles, possibly across many
  model calls, with its calls, usage, and state persisted across continuation.
  Some SDKs call one model call plus its tool calls a "turn"; Dashi calls that
  a **model attempt** and never uses "turn" for a loop iteration.
- **Session**: A conversation of many turns, named by a **session ID**. It holds
  the whole conversation history plus the other events and information around
  it: stops, approvals, steers, compaction summaries, spend, warnings, and
  closed turns. It is stored as its turns' records and has no record of its own.
  Avoid "thread" and "conversation" as type names. (Proposed; see the [sessions
  PRD](roadmap-research/14-session-continuity-prd.md).)
- **Session handle**: The Go value (`session.Session`) that drives one session in
  one process or request, with its runner, steering, and cache. It holds no
  durable state: anything that must outlast it belongs in the turn records. Many
  handles, in many processes, can refer to one session over its life.
- **Turn outcome** (`dashi.Outcome`): The returned turn record and its current
  view, including its stop, answer, spend, and pending work. The separate Go
  error reports command and recording failures.
- **Turn stop** (`dashi.Stop`): Why turn execution paused or ended. `Terminal`
  says whether the turn can ever run again. `TurnView.Next()` gives the host's
  next action.
- **Next action**: What the host does next with a turn, read from its recorded
  stop (`TurnView.Next`): nothing, running (a hook's view), resume, answer,
  continue, raise a budget or the model-attempt limit, or resolve a cause
  first.
- **Ended turn**: A turn whose stop is terminal; no command can run it again
  (`ErrTurnEnded`). A session **closes** a stopped turn that has not ended by
  canceling it when a new message arrives, and refuses while the turn is
  **busy**: live, possibly live (an expired lease included), waiting on the
  host, or holding an unsettled uncertain call, whatever its liveness. Only
  proof that no worker still drives it makes a turn **stopped**; that proves
  nothing about effects the worker started. (Proposed; decision 38.)
- **Subagent**: An agent performing delegated work, linked to the parent
  turn's call that started it. A background subagent is a **task** that runs
  in a **child session** of one or more child turns. (Proposed; decision 42;
  was "a child turn".)
- **Task**: One subagent the parent started with the `task` tool, named by a
  **task ID** such as `t_k3f2`. It runs in the background and can run more
  than one turn. (Proposed; decision 42.)
- **Child session**: The session a task runs in. Its first turn is the
  delegated work; each later turn starts from a message the parent sent, with
  the child's own history. (Proposed; decision 42.)
- **Agent message**: Text a parent sends to one of its tasks, or a task sends
  to its parent, delivered as a steer or a queued message. The user says
  "message"; the longer name keeps it apart from `llm.Message`. (Proposed;
  decision 42.)
- **Sender**: Who sent an agent message: `parent`, or a task ID with its
  subagent name. Dashi sets it from the sending turn, never from the text.
  (Proposed; decision 43.)
- **Notice**: The agent message Dashi sends for a task when one of its turns
  ends, carrying the answer or the stop reason.

Use the five **turn commands** as the shared verbs. Each carries a
**command ID** (`CommandID`), an idempotency key: repeating it with the same
payload acknowledges the recorded step instead of repeating the effect.

- **Start**: Begin a new turn with input.
- **AddMessages**: Add context to an existing turn without advancing execution.
- **Continue**: Advance an eligible existing turn, optionally with more context.
- **Resolve**: Record a host-supplied tool outcome, approval answer, hosted
  approval, or model result. It never runs the turn; `Continue` does.
- **Cancel**: Request that the turn stop.

These replace the lifecycle meanings currently carried by Dive's `WithInput`,
`WithContinue`, `WithResume`, `WithToolResults`, and `WithBackgroundResults`.

## Supply input and context

- **Turn input**: Context items that start a turn, such as a request with
  attachments.
- **Context item**: Named information with origin, authority, lifetime, and
  accumulation or replacement behavior; generalizes Dive's typed reminders.
- **Message and content**: Role-attributed text, media, and tool calls/results.
- **Steer**: Input that reaches a running turn before its next model call,
  never between a tool call and its result. With no turn running, it is
  queued. An **urgent** steer, which only the user sends, may cut the model
  call in flight.
- **Queued input**: Input held for the next turn. When the session is idle,
  queued items start a **queue turn**.
- **Conversation history**: Every message across a session's turns: user and
  assistant messages, tool calls, and tool results. It is complete and never
  shortened; compaction does not remove anything from it.
- **Projected history**: The messages sent to the model for one call: the
  conversation history after compaction and projection. `Start.History` and
  `Continue.History` carry the projected history of earlier turns.
- **Compaction**: Summarizing the projected history while retaining the
  conversation history and the execution record.
- **Replacement history**: What a compaction turn records to stand in for the
  projected history of the turns it covers: the summary plus the kept messages.
  Every reader builds later turns' projected history from it. (Proposed;
  decision 39.)

## Inspect calls and results

- **Model input**: The caller's model selection, instructions, messages, tools,
  settings, and stream selection before preparation.
- **Model request**: Immutable, destination-bound input, effective settings,
  decisions, evidence, and exact body produced by preparation.
- **Request snapshot**: The recordable form of a model request; carries no
  execution status.
- **Attempt**: One execution try using a model request. Retries are distinct
  attempts; preparation and entering `Send` do not prove dispatch or receipt.
- **Model capabilities**: Known support and limits, with evidence and a date;
  unknown support remains explicit. **Pricing** is separate cost information.
- **Model result** (`llm.Result`): One attempt's message, stop reason, and
  usage, whether streamed or returned whole.
- **Model stop** (`llm.Stop`): Why one model response ended, with its classified
  kind and raw provider reason. A model stop alone does not end the turn.
- **Tool call**: An identified request naming a tool and its requested and
  effective arguments.
- **Tool outcome** (`tool.Outcome`): A tool call's execution state and permitted
  payload. **Tool output** (`tool.Output`) is its returned content and display
  text. `ToolView.Result()` gives the model-facing projection. An unobserved
  effect remains uncertain in `ToolView.Status`.
- **Usage and limits**: Accumulated token and call counts, and bounds on calls,
  concurrency, and time.
- **Prompt cache**: The provider's store of a request's leading part (tools,
  system prompt, earlier messages), reused by a later request that starts with
  exactly the same content. A reuse is a **cache read**; storing is a **cache
  write**. (Proposed; see [prompt-cache.md](prompt-cache.md).)
- **Cache point**: A place in a request where the provider writes the prefix up
  to it (Anthropic's `cache_control`). Dashi places them; callers do not.
- **Cache lifetime (TTL)**: How long the provider keeps an unused entry. The
  caller asks for a minimum; Dashi picks the shortest lifetime the route offers
  that meets it.
- **Cache key**: An application label, such as a session ID, that groups calls
  sharing a prefix so the provider can route them to the same cache. Dashi sends
  it as given, hashed only when too long for the API.

## Guide execution and follow progress

- **Hook**: A named application function called at one point of a turn
  (`BeforeModel`, `BeforeSend`, `BeforeTool`, `BeforeFinish`) to shape or
  decide it; its answer is recorded with the step it leads to.
- **Dialog**: A prompt asking a person for approval, selection, or text input.
- **Event and observer**: Live text, tool progress, and execution updates
  delivered to an observer callback.
- **Step**: A recorded fact within a turn, such as a model request or tool
  completion.

[model]: ../tmp/research/design/2026-09-26-dive-v2-conceptual-model.md
[dive]: ../tmp/research/dive-api/README.md
[terminology]: terminology-proposal.md
