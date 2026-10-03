# Decide examples

These examples connect Dashi hooks to [Decide](https://github.com/deepnoodle-ai/decide),
a Go client for decision models such as TypeSafe's Jev and Cloudflare's Clef.
A decision model answers typed questions, such as yes-or-no (`Noul`), choice,
and score, with probabilities instead of prose. That makes it a cheap, fast
judge at points where a second LLM would be slow, costly, and hard to parse.

| Example | Hook point | Shows |
| --- | --- | --- |
| [`riskgate`](riskgate) | `BeforeTool` | Each tool call is judged before it runs: ordinary calls proceed, a destructive call waits for an operator, a call beyond the request is denied |
| [`injection`](injection) | `AfterTool` | Results from open-world tools are screened for prompt injection; a flagged page is withheld from the model |
| [`toolpick`](toolpick) | `BeforeModel` | A 22-tool catalog is narrowed to the tools relevant to the request, with the same tools hidden on every call of the turn |

Each hook lives in its own file (`gate.go`, `screen.go`, `pick.go`) so it can
be lifted into an application; `main.go` holds the wiring and the scenario.
Every policy goes through Decide's `patterns/gate` or `patterns/rank`. All
thresholds are placeholders: measure your own on labeled data, then pin a
versioned model, since an alias such as `jev-latest` can move.

## Running

This directory is its own Go module because Decide needs Go 1.27 and Dashi's
modules stay on Go 1.26. Its `go.work` builds against the Dashi checkout two
directories up. Run commands from here:

```sh
cd examples/decide
go run ./riskgate            # keyless: scripted model, canned Decide answers
go test ./...
go run ./riskgate -live      # Claude and Jev
```

Without `-live`, a `dashitest` client plays the model and a `decidetest`
server answers from fixed rules, so the output never changes and no key is
needed. The canned probabilities are stand-ins, not real judgments. With
`-live`, set `ANTHROPIC_API_KEY` and `TYPESAFE_API_KEY`.

## Design rules the examples follow

- **Advice only tightens.** A decision model's answer can hold or deny a call,
  but never approves one: the gate's allow is "no opinion", never
  `dashi.Allow`. See [using a model's advice](../../docs/guides/approvals.md#using-a-models-advice).
- **Safety checks fail closed; optimizations fail open.** A failed risk check
  holds the call for a person, and a failed injection screen withholds the
  result. A failed tool pick offers every tool.
- **Decide calls sit outside the turn's accounting.** Their cost is not in the
  turn's spend or budget. Track it in the host's ledger.
