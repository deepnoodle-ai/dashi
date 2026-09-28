# Zero-config first turn

**Status:** Accepted · **Date:** 2026-09-28 · **Addresses:**
[gaps/F](gaps/F-example-ports.md) 1.1, 3.1, 1.2, 1.3, 2.1 · **Relates to:**
decision 31 ([hooks.md](hooks.md)), [turn.md](turn.md)

> **Superseded API:** hooks ([hooks.md](hooks.md), decision 31) replaced
> `Policy`. Its `BeforeModel`, `BeforeSend`, `AuthorizeTool` and `OnStop`
> are now the `BeforeModel`, `BeforeSend`, `BeforeTool` and `BeforeFinish`
> hook points; `ModelAuthorization` is `SendDecision`; approvals no longer
> carry a policy revision; and a tool with no hook opinion runs.

## The problem

The shortest useful program needed an agent `ID` and `Revision`, a model name,
a turn ID, a `CommandID`, and `Policy: dashi.AllowLocalPolicy{}` if it had
tools. Without the policy, every tool call was denied silently; without a key,
the first call failed with a bare 401. After this change:

```go
client, err := messages.New(messages.Config{})
runner := dashi.Runner{Client: client, Tools: tools}
out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
	Input: []llm.Message{llm.UserMessage(llm.Text("Weather in Tokyo?"))},
})
// out.Turn.ID, each step's CommandID, and TurnView.Agent show what was filled.
```

## 1.1 Identity defaults

Before replay, loading, or any commit, the runner fills empty values. It never
overwrites a set value.

| Value | New turn | Existing turn |
| --- | --- | --- |
| Turn ID | `dashi.NewID()` for `Start`; an error for other commands | (set) |
| `CommandID` | `dashi.NewID()` | `dashi.NewID()` |
| Whole `Agent` empty | defaults below | the recorded agent, whole |
| `Agent.ID` | stays empty | recorded value |
| `Agent.Model` | catalog default (1.2) | recorded value |
| `Agent.Revision` | `"auto:"` + 16 hex of the agent digest | recorded value |

The turn ID is filled before the recorder is chosen or loaded, so every
emitted step and the final `Stream` event carry it, and a host can save it
from the first step event, before the model call. The `CommandID` is filled
before the command digest is computed, because the ID is part of the digest.

**Why.** `Continue` compares the whole agent with the recorded one, so a random
revision would fail with `ErrAgentChanged` in the next process. The derived
revision is a digest of the recorded behavior fields (instructions, provider,
API, model, settings, output schema, limits), excluding ID, Revision, and
`Limits.Deadline` (a per-turn time that would make every revision unique). It
changes exactly when behavior changes. Tools are not part of the agent and are
not compared on `Continue`; hosts that version tools set `Revision` themselves.
A whole-empty agent on an existing turn uses the record, so a later catalog or
library-default change, or edited instructions, cannot block `Continue` or
`Cancel` of an old turn. A partly set agent is still compared field by field,
and `ErrAgentChanged` now names the differing fields and the fix: pass the
recorded agent (`TurnView.Agent`), or cancel and start a new turn.

`NewID` returns a UUIDv7 string: time-ordered and fit for a Postgres `uuid`
column. Go 1.26 has no UUID package, so it is about fifteen lines over
`crypto/rand` and `time`, with no new dependency.

**Retries.** A `CommandID` removes a duplicate only when a retry reuses it,
and nobody can reuse an ID generated inside a call that crashed. To make
retries safe across restarts, choose the turn ID and each `CommandID`, save
them before the call, and reuse them. Without that, step numbering still
protects the record: a retry from an old turn snapshot writes at an occupied
sequence number, the recorder rejects the write, and no model call or tool
runs twice; the caller sees a rejected write rather than the recorded result.
What remains possible is a second turn from a `Start` retried with no turn ID,
and a duplicate `Deliver` sent after reloading. No warning is recorded for
generated IDs, and there is no durability interface: the guide says this.

`Agent{}` is valid; `Validate` no longer requires identity fields or a model.

## 3.1 Bound tools run by default

A nil `Policy` becomes an unexported `defaultPolicy` that allows every
registered local and external tool, recording revision `default-v1`, so an
audit can tell a default allow from an explicit `AllowLocalPolicy`
(`allow-local-v1`). `denyToolPolicy` is removed. This is decision 31 applied
early; hooks later replace `Policy` and keep this default. Allowing an external
tool records a `tool_waiting` step until the host settles it, so it grants the
model nothing the host does not run itself. Provider-hosted tools never reach
`AuthorizeTool`. Approval is opt-in: a policy that returns `DecisionDefer`.

## 1.2 Default model

The runner resolves an empty `Agent.Model` to `catalog.Default(provider)`
when the client knows that model (its `ModelInfo` reports it). Other providers
(OpenRouter, DeepInfra, Ollama, custom endpoints) need `Model`, and the error
says so. The runner picks the general-purpose model, not a cheap one.
`catalog.Small(provider) (string, bool)` reads a new optional `"small"` field,
for examples and tests; it returns false where a catalog names none (Meta). A
test keeps its input price at or below the default's; for OpenAI the two are
the same model, since `gpt-6-luna` is already the cheapest current one. Nothing
selects it implicitly. The `llm` layer does not default: an
empty `Input.Model` is an error that points to `catalog.Default`.

## 1.3 Missing API key

Constructors keep succeeding without a key, because `Prepare` and `ModelInfo`
work offline (tests, dry runs, cost estimates). The first `Send` fails before
dispatch with an `*llm.Error` of category authentication, disposition
not-dispatched, wrapping `llm.ErrNoAPIKey` and naming the environment
variables checked. The check is skipped when a custom endpoint or a custom
`http.Client` is set, since a proxy or transport may supply credentials.
Ollama and Gemini on Vertex need no key. The media clients (OpenAI Images,
Gemini media and Interactions, Grok Imagine) match: their sends fail with a
`media.Error` wrapping `llm.ErrNoAPIKey`. Each client reads its provider's
environment variable only when `Provider` is that provider, so a key never
reaches another destination (before this, OpenAI-format wrappers sent
`OPENAI_API_KEY` to xAI and others when their own variable was unset).

## 2.1 Choosing a provider at runtime

Constructors keep returning concrete types: idiomatic Go, and some clients
also implement media interfaces an `llm.Client` return would hide. The
documented pattern avoids wrapping a nil pointer in a non-nil interface:

```go
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
```

No name registry: provider configs differ too much to share one, and a
registry would need side-effect imports. It can be added later if hosts ask.

## Recorded-format changes

Default tool decisions record revision `default-v1`; start facts may hold an
empty agent ID and an `auto:` revision. There is no migration.
