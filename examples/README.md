# Examples

Small programs that each show one part of Dashi. Run them from the repository
root with `go run ./examples/<name>`. The comment at the top of each `main.go`
says what it shows and which environment variables it needs. Every example
makes paid requests except `chat`, `compaction`, `delegate`, and the Decide
recipes, which use scripted stand-ins and need no API key.

## Start here

| Example | Provider  | Shows                                                 |
| ------- | --------- | ----------------------------------------------------- |
| `agent` | OpenAI    | One agent turn and its reply                          |
| `basic` | OpenAI    | One streamed model call, without an agent             |
| `tools` | Anthropic | Go functions as tools, with the turn's usage and cost |
| `chat`  | None      | A conversation through a session, with a tool call    |

## Controlling a turn

| Example             | Provider  | Shows                                                      |
| ------------------- | --------- | ---------------------------------------------------------- |
| `hooks`             | Anthropic | Hooks that add a note, block a call, and ask for more      |
| `approval`          | Anthropic | A person approves or denies a tool call at the terminal    |
| `redact`            | Anthropic | Masking a secret in a tool result before the model sees it |
| `bigresult`         | Anthropic | A long tool result shortened for the model                 |
| `budget`            | Anthropic | A turn stopped by its spending cap, then continued         |
| `reasoning`         | Anthropic | The model's thinking streamed apart from its reply         |
| `structured_output` | OpenAI    | A reply that matches a JSON schema, decoded into a struct  |
| `steering`          | Anthropic | A chat CLI where lines typed during a turn steer the agent |
| `skills`            | Anthropic | Skills and slash commands loaded from folders              |
| `tool_progress`     | Anthropic | A slow tool reporting live progress                        |
| `compaction`        | None      | A session that summarizes old messages as its window fills |
| `coding`            | OpenAI    | A coding agent in one directory, with approval prompts     |

## Turns that outlive a process

| Example            | Provider  | Shows                                                    |
| ------------------ | --------- | -------------------------------------------------------- |
| `durable_approval` | Anthropic | Approvals recorded between runs, as a server would       |
| `resume`           | Anthropic | A turn that waits in one process and finishes in another |
| `live_follow`      | Anthropic | Following a running turn, with its text as it streams    |
| `delegate`         | None      | Background subagents whose results arrive later          |

## Providers and media

| Example           | Provider  | Shows                                                   |
| ----------------- | --------- | ------------------------------------------------------- |
| `providers`       | Any       | The same call on each provider; only the client changes |
| `provider_tools`  | Anthropic | Web search and code execution run by the provider       |
| `vision`          | Anthropic | Describing an image from a URL or a local file          |
| `documents`       | Anthropic | Asking about a text or PDF document, with citations     |
| `attachment_refs` | Anthropic | A stored file named by ID, loaded for each request      |
| `batch_speech`    | OpenAI    | Synthesizing a short speech clip, then transcribing it  |

## Advanced recipes

The [Decide recipes](decide/README.md) are longer programs for readers who
know hooks already. Each uses a decision model as a judge inside a hook: a
risk check on tool calls, a prompt-injection screen on tool results, and
narrowing a large tool list. They run without API keys by default.

The MCP examples are in their own Go module. From `mcp/`, run
`go run ./example/keyless` to start a local MCP server and call its `add` tool
with no API key. Run `go run ./example` to use Anthropic and the servers in a
`.mcp.json`.

## Running

```sh
export ANTHROPIC_API_KEY=...
go run ./examples/tools
go run ./examples/resume -mode=start
go run ./examples/resume -mode=resume
go run ./examples/providers -provider openrouter
```
