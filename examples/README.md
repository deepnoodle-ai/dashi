# Examples

Small programs that each show one part of Dashi. Run them from the repository
root with `go run ./examples/<name>`. Every example makes paid requests; the
package comment at the top of each `main.go` names the environment variables
it needs, and which of them are optional. Every example needs a provider key.

| Example             | Provider  | Shows                                                          |
| ------------------- | --------- | -------------------------------------------------------------- |
| `basic`             | OpenAI    | One streamed model call, without an agent                      |
| `agent`             | OpenAI    | One recorded agent turn                                        |
| `tools`             | Anthropic | Typed Go function tools, with the turn's usage and cost        |
| `skills`            | Anthropic | Host-selected skill catalog and recorded load tool             |
| `approval`          | Anthropic | A person approves or denies a tool call during the run         |
| `redact`            | Anthropic | Masking a secret in a tool result before the model sees it     |
| `bigresult`         | Anthropic | A large tool result: the model sees a bounded view             |
| `durable_approval`  | Anthropic | Approvals recorded between runs, as a server would             |
| `coding`            | OpenAI    | A coding agent in one directory, with approval prompts         |
| `tool_progress`     | Anthropic | A slow tool reporting live progress while the turn streams     |
| `delegate`          | OpenAI    | The agent starts background subagents and gets results later   |
| `steering`          | Anthropic | A chat CLI where lines typed during a turn steer the agent     |
| `resume`            | Anthropic | A turn that waits in one process and finishes in another       |
| `live_follow`       | Anthropic | Two readers following one running turn, with streamed text     |
| `hosted_tools`      | Anthropic | Provider-hosted web search and code execution, with sources    |
| `compaction`        | OpenAI    | A history shortened by a recorded summary, then a follow-up    |
| `structured_output` | OpenAI    | A final answer that matches a JSON schema, decoded into a type |
| `providers`         | Any       | The same call on each provider; only the client changes        |
| `vision`            | Anthropic | Describing an image from a URL or a local file                 |
| `documents`         | Anthropic | Asking about a text or PDF document, with cited passages       |
| `attachment_refs`   | Anthropic | A stored upload named by ID and digest, resolved per request   |

For example:

```sh
export ANTHROPIC_API_KEY=...
go run ./examples/tools
go run ./examples/resume -mode=start
go run ./examples/resume -mode=resume
go run ./examples/providers -provider openrouter
```
