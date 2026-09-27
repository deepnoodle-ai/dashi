# Ubiquitous language

Public concepts grounded in the [conceptual model][model] and
[Dive's current API][dive]. These name what callers configure, supply, inspect,
and act on. Foundation terms follow the [accepted terminology][terminology];
agent and session contracts remain proposed.

## Define an agent

- **Agent definition**: Reusable agent configuration: identity, version,
  instructions, model, tools, and limits.
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

- **Turn**: Identified work whose calls, usage, and state persist across
  continuation, with or without a session.
- **Session**: A named conversation organizing turns and their saved history.
- **Turn outcome**: The returned turn, its **stop** (why execution paused or
  ended), and recording status.
- **Subagent**: An agent performing delegated work as a **child turn** linked
  to its parent turn.

Use the proposal's five **turn commands** as the shared verbs:

- **Start**: Begin a new turn with input.
- **Deliver**: Add context to an existing turn without advancing execution.
- **Continue**: Advance an eligible existing turn, optionally with more context.
- **Accept**: Record a tool call's delayed or reconciled outcome; advance when
  ready.
- **Cancel**: Request that the turn stop.

These replace the lifecycle meanings currently carried by Dive's `WithInput`,
`WithContinue`, `WithResume`, `WithToolResults`, and `WithBackgroundResults`.

## Supply input and context

- **Turn input**: Context items that start a turn, such as a request with
  attachments.
- **Context item**: Named information with origin, authority, lifetime, and
  accumulation or replacement behavior; generalizes Dive's typed reminders.
- **Message and content**: Role-attributed text, media, and tool calls/results.
  **Conversation history** is the message view across turns.
- **Compaction**: Summarizing model-facing history while retaining the
  execution record.

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
- **Model result**: One attempt's message, stop reason, and usage, whether streamed
  or returned whole.
- **Tool call**: An identified request naming a tool and its requested and
  effective arguments.
- **Tool outcome**: A call's execution state; **tool output** is its returned
  content and display text. Uncertainty remains distinct from known failure.
- **Usage and limits**: Accumulated token and call counts, and bounds on calls,
  concurrency, and time.

## Guide execution and follow progress

- **Policy**: Application decisions about model requests, tool authorization,
  and whether a turn should continue.
- **Dialog**: A prompt asking a person for approval, selection, or text input.
- **Event and observer**: Live text, tool progress, and execution updates
  delivered to an observer callback.
- **Step**: A recorded fact within a turn, such as a model request or tool
  completion.

[model]: ../tmp/research/design/2026-09-26-dive-v2-conceptual-model.md
[dive]: ../tmp/research/dive-api/README.md
[terminology]: terminology-proposal.md
