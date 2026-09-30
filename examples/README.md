# Examples

Small programs that each show one part of Dashi. Run them from the repository
root with `go run ./examples/<name>`. Every example but `chat` and
`compaction` makes paid requests; the package comment at the top of each
`main.go` names the environment variables it needs, and which of them are
optional. Every example but those two needs a provider key.

| Example             | Provider  | Shows                                                          |
| ------------------- | --------- | -------------------------------------------------------------- |
| `chat`              | None      | A session: two messages, a tool call, and a denied call        |
| `compaction`        | None      | A session that runs a compaction turn when its window fills    |
| `agent`             | OpenAI    | One recorded agent turn                                        |
| `basic`             | OpenAI    | One streamed model call, without an agent                      |
| `reasoning`         | Anthropic | A reasoning effort; thinking streamed apart from the answer    |
| `tools`             | Anthropic | Typed Go function tools, with the turn's usage and cost        |
| `hooks`             | Anthropic | Hooks that deliver context, deny a call, and nudge an answer   |
| `skills`            | Anthropic | Host-selected skill catalog and recorded load tool             |
| `approval`          | Anthropic | A person approves or denies a tool call during the run         |
| `redact`            | Anthropic | Masking a secret in a tool result before the model sees it     |
| `bigresult`         | Anthropic | A large tool result: the model sees a bounded view             |
| `budget`            | Anthropic | A turn stopped by its cost cap, then continued with a raise    |
| `durable_approval`  | Anthropic | Approvals recorded between runs, as a server would             |
| `coding`            | OpenAI    | A coding agent in one directory, with approval prompts         |
| `tool_progress`     | Anthropic | A slow tool reporting live progress while the turn streams     |
| `delegate`          | OpenAI    | The agent starts background subagents and gets results later   |
| `steering`          | Anthropic | A chat CLI where lines typed during a turn steer the agent     |
| `resume`            | Anthropic | A turn that waits in one process and finishes in another       |
| `live_follow`       | Anthropic | Two readers following one running turn, with streamed text     |
| `hosted_tools`      | Anthropic | Provider-hosted web search and code execution, with sources    |
| `structured_output` | OpenAI    | A final answer that matches a JSON schema, decoded into a type |
| `providers`         | Any       | The same call on each provider; only the client changes        |
| `vision`            | Anthropic | Describing an image from a URL or a local file                 |
| `documents`         | Anthropic | Asking about a text or PDF document, with cited passages       |
| `attachment_refs`   | Anthropic | A stored upload named by ID and digest, resolved per request   |
| `batch_speech`      | OpenAI    | Synthesizing a short speech clip, then transcribing it         |

The MCP examples are in their own Go module. From `mcp/`, run
`go run ./example/keyless` to start a stdio MCP server and call its `add` tool.
It uses fixed model replies and needs no API key or config file.
Run `go run ./example` to use Anthropic and the servers in a `.mcp.json`.

For example:

```sh
export ANTHROPIC_API_KEY=...
go run ./examples/tools
go run ./examples/resume -mode=start
go run ./examples/resume -mode=resume
go run ./examples/providers -provider openrouter
```
