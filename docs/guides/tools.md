# Tools

This guide shows how to give an agent tools: local Go functions, tools that
run on the provider's servers, and the outcomes a tool returns. See
[running turns](running-turns.md) for how the runner executes them.

## Try it

Set `ANTHROPIC_API_KEY` and `ANTHROPIC_MODEL`, then run
`go run ./examples/tools` or `go run ./examples/provider_tools`.

```go
weather := tool.MustFunc(
    "get_weather",
    "Get the current weather for a city.",
    func(
        ctx context.Context,
        call tool.Call,
        in WeatherInput,
    ) (tool.Outcome, error) {
        return tool.Succeeded("18°C, partly cloudy"), nil
    },
)
search, err := messages.NewWebSearchTool(messages.WebSearchToolOptions{})
// Handle err, then:
runner.Tools = tool.List{weather, search}
```

For a coding agent, [coding tools](coding-tools.md) provides file and shell
tools for one directory.

`tool.List` is a fixed `tool.Toolset`. Implement `Toolset` yourself when the
available tools change between model calls.

## Choosing tools per agent

`Runner.Tools` is every tool the process can run. Each agent picks from it by
name with `Agent.Tools`; an empty list means all of them, and
`Agent.NoTools` offers none. One runner can then serve agents with different
tools:

```go
start := dashi.Start{
    Agent: dashi.Agent{Tools: []string{"get_weather"}},
    Input: input,
}
```

The runner applies the selection before each model call, before any
`BeforeModel` hook. A name the runner does not offer is skipped, and the turn
records a `tool_not_offered` warning once per name. The selection is part of
the agent, so the turn records it and it changes the derived `auto:` version.
The turn records the names sorted and without duplicates, so the order of the
list does not matter.

## Defining tools

`tool.Func` generates the tool's input schema from its argument type `T` once,
when the tool is built, and validates every call against it before your
function runs. `tool.MustFunc` is the same but panics instead of returning an
error, for a tool built once at startup, as the examples do. The rules:

- `T` must be a struct or a pointer to one, other than `time.Time`;
  `tool.Func` returns an error otherwise, including for maps and `any`.
- Each exported field is a property named by its `json` tag, or by its Go name
  without one. `json:"-"` skips a field.
- A field is required unless its `json` tag has `omitempty`. A `required`
  tag (`true` or `false`) overrides that; `omitzero` and pointer types do not.
  An omitted field decodes to its zero value.
- `description` and `enum` (comma-separated values) describe the field to the
  model. `format`, `pattern`, `minimum`, `maximum`, `minLength`, `maxLength`,
  `minItems`, and `maxItems` constrain it; a malformed number tag is ignored.
- `default` and `example` are hints to the model only: a default is never
  applied to an omitted field.
- Pointer fields accept an explicit JSON `null` and decode it as nil. A pointer
  field is still required unless its `json` tag has `omitempty` or a
  `required:"false"` tag. An omitted optional pointer also decodes as nil;
  inspect `call.Args` when the distinction between omitted and null matters.
- `nullable:"false"` can forbid null for a pointer with a concrete schema
  type. `nullable:"true"` on a non-pointer field, and `nullable:"false"` on an
  unconstrained `*any` or `*json.RawMessage`, fail when the tool is built.
  Malformed `nullable` tags and tags on flattened embedded structs fail too.
- Structs are open at every level by default: unknown properties are accepted
  by validation and ignored by ordinary Go struct decoding. Pass
  `tool.WithStrictArguments()` to `tool.Func` to reject them at the root and
  in nested structs. An embedded struct without a `json` name is flattened,
  and its fields are optional when it is embedded by pointer; with a name it
  is a regular field.
- In fields, `[]byte` is a base64 string, `time.Time` a date-time string, and
  `any` or `json.RawMessage` accept any value. Maps need string keys;
  channels, functions, complex numbers, and the `json` `,string` option fail
  when the tool is built.
- A recursive type must recurse through a struct, as in
  `type Node struct{ Children []Node }`; `type Nodes []Nodes` fails when the
  tool is built.

The runner checks every call's arguments against the schema before any
`BeforeTool` hook sees it. A call that does not match is not run: it is recorded
as `not_executed` with the reason `invalid arguments`, and the model sees
`Invalid tool arguments: …` so it can retry. An executor called directly refuses
such a call the same way, with an `invalid input: …` message.

### Unknown-key migration note

`tool.Func` now accepts extra object keys by default. For example, a tool with
required `query` and optional `region` accepts
`{"query":"yes","regoin":"us"}`; `region` remains unset. A misspelled
**required** field still fails. Choose `tool.WithStrictArguments()` for a tool
whose exact argument names matter, including a side-effecting tool whose
approval display lists only declared fields:

```go
deploy := tool.MustFunc("deploy", "Deploy a version", deployFn,
    tool.WithStrictArguments())
```

Case-only aliases of declared names are rejected even in default mode:
`{"query":"yes","QUERY":"evil"}` could otherwise validate `query` as
`"yes"` but decode `"evil"` into the Go field. Duplicate object keys keep
their last value, including when a nested object repeats. The typed callback
decodes that same last-key JSON meaning, while `Call.Args` stays raw. Ordinary
surplus keys are still accepted. When the matching typed executor is bound,
case-alias checks run before `BeforeTool` hooks and after hook rewrites. A
definition advertised without its executor can still reach a held approval;
the call cannot gain an executor later in that same run. On resume, the bound
executor checks again before its callback runs. A Toolset lookup error stops
the turn before the hook sees the call.

The schema sent to a provider guides generation and Dashi validates returned
arguments locally. It does not guarantee a provider will sample only declared
keys. `Call.Args` and `BeforeTool` hooks see the effective raw JSON, including
ignored keys; if a hook rewrites it, `Call.RequestedArgs` retains the model's
original JSON. The typed callback's ordinary struct fields do not receive
unknown sibling keys, but `Call.Args`, `any`, `json.RawMessage`, and custom
decoders are not sanitized. Declare a map or write a custom tool when dynamic
keys have meaning. Existing hand-written schemas keep their own
`additionalProperties` policy.

### Read-only and idempotent tools

Pass `tool.ReadOnly()` for a tool that changes nothing, and `tool.Idempotent()`
for one where a repeat has the same effect as one call. They set
`Definition.Annotations.ReadOnly` and `Idempotent`; without them each hint is
nil, meaning unknown.

```go
search := tool.MustFunc("search_notes", "Searches the notes.", searchFn,
    tool.ReadOnly())
```

The hints change behavior. When a crash loses a call's result, a resumed turn
reruns the call only if the tool has either hint; otherwise it does not run the
call again and records that its result is unknown: the earlier run may have
taken effect. A read-only subagent gets only tools marked read-only.

## Outcomes and errors

Return `tool.Succeeded(text)` for the common case. `tool.Detached(id)` hands
work to another system whose result the host records later with `Resolve`, and
`tool.Waiting(reason)` pauses the call until the host resolves it. `Waiting("")`
and `Detached("")` are invalid; the runner stops the turn if an executor
returns them.

To report a failure, return an error: the model sees its text, and the runner
keeps it for the host. Wrap it with `tool.Fatal` to stop the turn instead.
`tool.Failed(text)` is for resolving an external or detached call, or for
attaching a `Cost` to a failure.

A failed, denied, or skipped call reaches the model as a result flagged as an
error. Anthropic and Gemini carry the flag natively. OpenAI Responses and Chat
Completions have no flag, so the result text is preceded by `Tool failed:` and
the prepared request records an `encode_tool_error` content change, not a
warning.

## Large results

A tool can return more text than the model needs. By default, Dashi limits the
text of each tool result in the messages the model sees to 64 KiB. This
includes error results. Set `Agent.Limits.ToolResultBytes` for another limit. A
negative value turns the limit off. A value below 1 KiB becomes 1 KiB, and
`Start` records a `WarningLimitAdjusted`.

When a result is over the limit, the model sees the first half and the last
half of the allowed text. A marker between them gives the original size and the
omitted size. It asks for a smaller part and does not tell the model to run the
call again. The cuts fall on rune boundaries, so the text stays valid UTF-8.
Kept text stays in the block where it was, so text and images keep their
order. The limit also counts the separators that providers add between text
blocks, and Dashi drops empty text blocks. Images and other non-text blocks
stay in place and do not count.

The turn record keeps the full result in `ToolView.Outcome`.
`ToolView.Result()` returns the result as the model sees it, and
`ToolView.Shortened()` returns the sizes and the limit, as recorded when the
call finished. Replay, `dashi.TurnMessagesFull`, and recovery read the
recorded result. They never compute it again, so a new limit applies only to
new turns. `Observer` and `Stream` events carry the full result,
so a live reader can show all of it to a person
([example](../../examples/bigresult/main.go)).

The limit applies to each result. Parallel calls add up: eight results of
64 KiB send 512 KiB. Set a lower limit when `ToolConcurrency` is high.
Shortening is not redaction. Secrets in the kept text stay in the messages the
model sees, and the full result stays in the record.

## Tool progress

A long-running tool can report a status snapshot with `tool.Report`. Each
report replaces the previous one; `Total` of zero means unknown, and `Data`
holds optional JSON for custom displays.

```go
tool.Report(ctx, tool.Progress{
	Message: fmt.Sprintf("processed %d/%d records", i, total),
	Done:    float64(i),
	Total:   float64(total),
})
```

Snapshots arrive as `Event.Progress` on `Runner.Stream` and on
`Runner.Observer`, labeled with the call's `CallID` and tool `Name`. They are
previews: never recorded, never replayed, and never shown to the model, so the
tool's `Outcome` must still carry its full result. `Report` never blocks and
does nothing when no one listens. When the consumer is slower than the tool,
snapshots merge and the latest wins. On `Stream`, the last report before
`Execute` returns arrives before the call's `tool_finished` step. `Observer`
gets progress at the lowest priority and may drop it: progress fills at most
half its preview queue, and deltas evict queued progress when the queue is
full. Nothing for a call follows its `tool_finished` step, and no progress is
delivered once the turn's context ends, such as after breaking a `Stream`
range. A snapshot may wait while the runner authorizes or commits another
call.
Report at human rates, a few times a second at most, and throttle rendering
if needed. To test a tool on its own, install a reporter with
`tool.WithReporter`. See `examples/tool_progress`, and
[live updates](live-updates.md) for serving progress to browsers.

## Provider tools

Provider packages construct tools that the provider runs inside the model
call, such as `messages.NewWebSearchTool` or `gemini.NewGoogleSearchTool`.
Each takes an options struct whose zero value works where the provider allows
it, and returns an error only when its options can be invalid. The result is a
`tool.Tool` with no executor: the runner never runs it and no hook sees
its calls. For a direct model call, pass its `Definition` in
`llm.Input.Tools`.

A provider tool is bound to one configured destination, named by the client's
`Provider()`. Anthropic and Gemini options have a `Provider` field; leave it
empty for the default destination and set it for one with a custom name. An
empty `Provider` on a Gemini tool without options, such as Google Search, also
works with a Vertex client that uses the default provider name; a client with
a custom name needs `Provider` set.
