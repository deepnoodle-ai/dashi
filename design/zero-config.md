# Zero-config first turn

**Status:** Proposal · **Date:** 2026-09-28 · **Addresses:**
[gaps/F](gaps/F-example-ports.md) 1.1, 3.1, 1.2, 1.3, 2.1 · **Relates to:**
decision 31 ([hooks.md](hooks.md)), [turn.md](turn.md)

## The problem

The shortest useful program today needs an agent `ID` and `Revision`, a model
name, a turn ID, a `CommandID`, and `Policy: dashi.AllowLocalPolicy{}` if it
has tools. Leave out the policy and every tool call is denied without an
error. Leave out the key and the first call fails with a 401. Each value
matters for recovery, but a one-process program has to invent them.

After this change:

```go
client, err := messages.New(messages.Config{}) // errors now if no key is set
runner := dashi.Runner{Client: client, Tools: tools}
out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
	Input: []llm.Message{llm.UserMessage(llm.Text("What's the weather in Tokyo?"))},
})
// out.Turn.ID, the step's CommandID, and TurnView.Agent show what was filled in.
```

## 1.1 Identity defaults

At the start of `Run` and `Stream`, after Provider and API come from the
client, the runner fills empty values. It never overwrites a set value.

| Value | When empty on a new turn | When empty on an existing turn |
| --- | --- | --- |
| Turn ID | `dashi.NewID()` for `Start`; an error for any other command | (not empty) |
| `CommandID` | `dashi.NewID()` | `dashi.NewID()` |
| `Agent.ID` | `"default"` | copied from the recorded agent |
| `Agent.Model` | the catalog default (1.2) | copied from the recorded agent |
| `Agent.Revision` | `"auto:"` + 16 hex of the agent's digest | copied from the recorded agent |

The filled values are recorded in the start fact like explicit ones, so
`Replay` and a later `Continue` see them. `Stream`'s final event carries the
generated turn ID.

**Why these choices.** A random agent ID or revision would change on every
process start. `Continue` compares the whole agent with the recorded one, so
a random value would fail with `ErrAgentChanged` on the next run. A fixed
`"default"` ID keeps the meaning "one agent, many revisions". The derived
revision is a digest of the resolved agent (instructions, model, settings,
schema, limits, provider, API; not ID or Revision), so it changes exactly
when behavior changes. The `auto:` prefix marks it as derived, not
author-chosen. On an existing turn, empty fields come from the record
instead of the defaults. A later catalog or library-default change therefore
cannot make an old turn look changed. The other fields are still compared,
so a real change is still `ErrAgentChanged`.

`NewID` returns a UUIDv7 string: time-ordered, and it fits a Postgres `uuid`
column. It is exported for hosts that must choose an ID before a call (below).

**What a generated `CommandID` cannot do.** A `CommandID` removes duplicates
only when a retry uses the *same* ID. A caller cannot know an ID that was
generated during a call that crashed. So a retried command with no
`CommandID` is not recognized as a repeat. The guide says this plainly:
*to make retries safe across restarts, choose the turn ID and each
CommandID yourself, save them before the call, and reuse them on retry.*
`dashi.NewID()` helps with that.

The cost of a missing ID is smaller than it seems, because step sequence
fencing still applies. A retry that uses an old turn snapshot writes at a
step number that is already taken. The recorder rejects it with
`ErrRevisionConflict`, so no model call or tool runs twice. The caller gets
an error instead of the recorded result. The real risks are:

- a `Start` retried with no turn ID creates a second turn (a second model
  call and its cost);
- a `Deliver` sent again after reloading the turn is recorded twice.

**Refuse, warn, or allow?** Allow, and warn when it can matter. A command
with a generated ID keeps working. When `Runner.Recorder` is set to
something other than `*MemoryRecorder` (a recorder that is likely durable),
the first step that command commits records `WarningGeneratedID`. The
warning names the generated turn or command ID. It appears in
`Outcome.Warnings` and on replay. An in-memory program sees no warning. The
library cannot detect durability exactly, and a durable-recorder interface
would add a concept to fix one warning. The type check is a cheap
heuristic; a wrapped memory recorder would warn, which is harmless.

`Agent.Validate` no longer requires ID, Revision, Provider, API, or Model:
`Agent{}` is valid. The runner returns an error only when no model can be
found (a provider without a catalog default and no `Model` set).

## 3.1 Bound tools run by default

A nil `Policy` becomes an unexported `defaultPolicy`. It allows every
registered local and external tool, and it records revision `default-v1` on
each decision, so an audit can tell "allowed by default" from "allowed by
the host's policy" (`allow-local-v1`). `denyToolPolicy` is removed. This is
decision 31 applied early to `Policy`. The hooks design removes `Policy`
later and keeps this default.

Is allowing external tools the safe choice? An external tool is not run by
the runner. Allowing it records a `tool_waiting` step and stops the turn
until the host sends `Settle` with a result. The host registered the tool
and runs it itself, so allowing it gives the model no new ability. Denying
it would bring back the silent failure for external tools. Provider-hosted
tools never reach `AuthorizeTool`. Approval stays opt-in: a policy (later, a
hook) that returns `DecisionDefer`. `AllowLocalPolicy` stays exported for
embedding in a custom policy. `examples/tools` and `examples/resume` drop
their `Policy` line.

## 1.2 Default model

An empty `Agent.Model` resolves to `catalog.Default(agent.Provider)`. The
runner uses it only when the client's API is one of that provider's catalog
destinations. That covers Anthropic, OpenAI, Google, xAI, Mistral, and Meta.
OpenRouter, DeepInfra, Ollama, and custom endpoints have no default and need
`Model`. The error says so.

The runner defaults to the general-purpose model, not the cheapest one. An
agent that silently runs on a small model gives worse answers, and cost is
visible in the turn anyway. A **small** default is still worth adding for
examples and tests: `catalog.Small(provider) (string, bool)`, read from a new
`"small"` field in each catalog file (for example `claude-haiku-4-5` or
`gpt-5.4-mini`). It is one line per file, checked by the existing
catalog-validity test. Without it, examples and live tests hardcode model
names that go stale. Nothing selects it implicitly.

## 1.3 Missing API key

Each provider's `New` returns an error when no key is found and the endpoint
is the provider's public default. The error wraps a new `llm.ErrNoAPIKey` and
names the environment variables it checked:
`messages: no API key: set ANTHROPIC_API_KEY or Config.APIKey`. `openai/images`
already does this. A custom endpoint may omit the key, because a proxy or a
custom `http.Client` may supply credentials. Ollama and Gemini on Vertex need
no key.

This is an error, not a warning. "Warn and adjust" needs a sensible
adjustment, and there is none for a missing credential. Every call would
fail. The library also has no logger to warn through. `errors.Is(err,
llm.ErrNoAPIKey)` lets a program that picks providers at runtime skip the
ones that are not configured.

## 2.1 Choosing a provider at runtime

Constructors keep returning concrete types (`*messages.Client`). Go advises
returning concrete types from constructors ("return structs"). Some clients
also implement media interfaces, such as the OpenAI images client. An
interface return would hide those. The problem was only a nil pointer
wrapped in a non-nil `llm.Client` interface. The documented pattern avoids
it without a generic helper:

```go
func newClient(name string) (llm.Client, error) {
	var client llm.Client
	var err error
	switch name {
	case "anthropic":
		client, err = messages.New(messages.Config{})
	case "openai":
		client, err = responses.New(responses.Config{})
	default:
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	if err != nil {
		return nil, err
	}
	return client, nil
}
```

`examples/providers` uses this pattern and drops `asClient`. The
model-calls guide shows it.

**No name registry.** A registry (`llm.Open("anthropic")`) would need either
imports made only for their side effects (as `database/sql` drivers are) or
one config type shared by all providers. The configs differ: Vertex project,
OpenRouter site headers, Gemini's separate module. A shared config would
reduce each provider to a key and an endpoint. The switch above is short and
explicit, and it type-checks. A registry can be added later without breaking
anything if hosts ask for config-driven selection.

## Recorded-format changes

- Tool decisions allowed by default record revision `default-v1`.
- A new warning code, `generated_id`, is recorded on steps.
- Start facts may hold `"default"` and `auto:…` agent identities.

There are no users, so there is no migration.

## Tests

- A turn started with `Turn{}`, no `CommandID`, and `Agent{}` completes. The
  generated values are recorded and are the ones returned. A `Continue` with
  the same empty agent succeeds; a changed `Instructions` still fails with
  `ErrAgentChanged`.
- The derived revision is stable for equal agents and changes when behavior
  changes. A non-`Start` command with an empty turn ID fails.
- `WarningGeneratedID` is recorded with a custom recorder, and not with a nil
  recorder or a `*MemoryRecorder`.
- With a nil policy, a local tool runs, an external tool waits, and the
  decision revision is `default-v1`.
- An empty model resolves to the catalog default. A provider without a
  catalog default gives a clear error. `catalog.Small` exists for every
  catalog provider and names a catalog entry.
- Missing-key table over providers: the public endpoint returns an error that
  matches `llm.ErrNoAPIKey`, and a custom endpoint, Ollama, and Vertex
  succeed. Tests that build clients without a key get a test key.

## Files

`runner.go` (the defaults call and removing `denyToolPolicy`; kept small),
new `defaults.go` (`NewID`, revision digest, filling), `turn.go` (docs,
`Validate`, a command-ID setter, the warning code), `policy.go`,
`catalog/*`, provider `New` functions and `internal/providerhttp`, the Gemini
module, `llm` (the sentinel), examples, `docs/guides/running-turns.md`,
`docs/guides/model-calls.md`, `docs/agent-turns.md`, `design/turn.md`, and a
decision entry.

## Risks and coordination

- Outcome/approvals edits `runner.go` and `Settle`. My `runner.go` edits are
  limited to the validation and defaults block at the top of `run` and the
  final event in `Stream`. Their `Settle` builders can leave out `CommandID`.
- Hooks will replace `Policy` and must keep `defaultPolicy`'s behavior.
- A `Limits.Deadline` is part of the agent, so a derived revision changes
  with each deadline. Set `Revision` if that matters.
