# Tools

This guide shows how to give an agent tools: local Go functions, tools that
run on the provider's servers, and the outcomes a tool returns. See
[running turns](running-turns.md) for how the runner executes them.

## Try it

Set `ANTHROPIC_API_KEY` and `ANTHROPIC_MODEL`, then run
`go run ./examples/tools` or `go run ./examples/hosted_tools`.

```go
weather, err := tool.Func(
    "get_weather",
    "Get the current weather for a city.",
    func(ctx context.Context, call tool.Call, in WeatherInput) (tool.Outcome, error) {
        return tool.Succeeded("18°C, partly cloudy"), nil
    },
)
// Handle err, then:
search, err := messages.NewWebSearchTool(messages.WebSearchToolOptions{})
// Handle err, then:
runner.Tools = tool.List{weather, search}
```

`tool.List` is a fixed `tool.Toolset`. Implement `Toolset` yourself when the
available tools change between model calls.

## Defining tools

`tool.Func` generates the tool's input schema from its argument type `T` once,
when the tool is built, and validates every call against it before your
function runs. The rules:

- `T` must be a struct (or a pointer to one); `tool.Func` returns an error
  otherwise.
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
- `nullable` is written to the schema, but validation still refuses `null`
  for a pointer field.
- Nested structs are closed at every level: unknown properties are refused.
  Fields of an embedded struct are flattened, and optional when it is
  embedded by pointer.
- `[]byte` is a base64 string, `time.Time` a date-time string, and `any` or
  `json.RawMessage` accept any value. Maps need string keys; channels,
  functions, and complex numbers fail when the tool is built. The `,string`
  option is not honored.

A call whose arguments do not match is not run: it is recorded as
`not_executed` with an `invalid input: …` message the model sees, so it can
retry.

## Outcomes and errors

Return `tool.Succeeded(text)` for the common case. `tool.Detached(id)` hands
work to another system whose result the host records later with `Settle`, and
`tool.Waiting(reason)` pauses the call until the host settles it. `Waiting("")`
and `Detached("")` are invalid; the runner stops the turn if an executor
returns them.

To report a failure, return an error: the model sees its text, and the runner
keeps it for the host. Wrap it with `tool.Fatal` to stop the turn instead.
`tool.Failed(text)` is for settling an external or detached call, or for
attaching a `Cost` to a failure.

## Hosted tools

Provider packages construct tools that the provider runs inside the model
call, such as `messages.NewWebSearchTool` or `gemini.NewGoogleSearchTool`.
Each takes an options struct whose zero value works where the provider allows
it, and returns an error only when its options can be invalid. The result is a
`tool.Tool` with no executor: the runner never dispatches it and no `Policy`
is consulted. For a direct model call, pass its `Definition` in
`llm.Input.Tools`.

A hosted tool is bound to one configured destination, named by the client's
`Provider()`. Anthropic and Gemini options have a `Provider` field; leave it
empty for the default destination and set it for one with a custom name. An
empty `Provider` on a Gemini tool without options, such as Google Search, also
works with a Vertex client.
