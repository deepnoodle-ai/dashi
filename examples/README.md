# Examples

Small programs that each show one part of Dashi. Run them from the repository
root with `go run ./examples/<name>`. Every example makes paid requests; the
package comment at the top of each `main.go` names the environment variables
it needs. A provider key is required; `ANTHROPIC_MODEL` or `OPENAI_MODEL`
optionally picks the model, and otherwise the model catalog picks one.

| Example             | Provider  | Shows                                                          |
| ------------------- | --------- | -------------------------------------------------------------- |
| `basic`             | OpenAI    | One streamed model call, without an agent                      |
| `agent`             | OpenAI    | One recorded agent turn                                        |
| `tools`             | Anthropic | Typed Go function tools, with the turn's usage and cost        |
| `approval`          | Anthropic | A tool that waits for a person to approve it                   |
| `tool_progress`     | Anthropic | A slow tool reporting live progress while the turn streams     |
| `resume`            | Anthropic | A turn that waits in one process and finishes in another       |
| `hosted_tools`      | Anthropic | Provider-hosted web search and code execution, with sources    |
| `structured_output` | OpenAI    | A final answer that matches a JSON schema, decoded into a type |
| `providers`         | Any       | The same call on each provider; only the client changes        |
| `vision`            | Anthropic | Describing an image from a URL or a local file                 |
| `documents`         | Anthropic | Asking a question about a PDF                                  |

For example:

```sh
export ANTHROPIC_API_KEY=...
go run ./examples/tools
go run ./examples/resume -mode=start
go run ./examples/resume -mode=resume
go run ./examples/providers -provider openrouter
```
