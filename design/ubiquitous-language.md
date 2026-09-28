# Ubiquitous language

Public concepts grounded in the [conceptual model][model] and
[Dive's current API][dive]. These name what callers configure, supply, inspect,
and act on. Foundation terms follow the [accepted terminology][terminology];
the agent, runner, and command names follow [decision 30](decisions.md).
Session contracts remain proposed.

## Define an agent

- **Agent**: Reusable agent configuration (`kernel.Agent`): identity,
  revision, instructions, model, settings, output schema, and limits. It is
  plain JSON data, recorded when a turn starts, not a running object.
- **Runner**: The value that runs turns (`kernel.Runner`): an agent plus its
  long-lived services, such as the client, tools, recorder, policy, and
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
  Some SDKs call one model call plus its tool calls a "turn"; Kernel calls that
  a **model attempt** and never uses "turn" for a loop iteration.
- **Session**: Reserved for a future conversation of many turns and their
  saved history. Avoid "thread" and "conversation" as type names.
- **Turn outcome**: The returned turn, its **stop** (why execution paused or
  ended), and recording status.
- **Subagent**: An agent performing delegated work as a **child turn** linked
  to its parent turn.

Use the five **turn commands** as the shared verbs. Each carries a
**command ID** (`CommandID`), an idempotency key: repeating it with the same
payload acknowledges the recorded step instead of repeating the effect.

- **Start**: Begin a new turn with input.
- **Deliver**: Add context to an existing turn without advancing execution.
- **Continue**: Advance an eligible existing turn, optionally with more context.
- **Settle**: Record a host-supplied tool outcome, approval answer, hosted
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
