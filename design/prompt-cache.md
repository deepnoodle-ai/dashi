# Prompt caching: tech spec

**Status:** Proposed, 2026-09-29. **PRD:** [roadmap item
3](roadmap-research/03-prompt-cache-prd.md). **Decision:** [41](decisions.md).
**Workflow:** spec, then build in parallel with review. Terms (prompt cache,
cache point, cache lifetime, cache key) are in the
[glossary](ubiquitous-language.md).

## Context

Dashi measures cache reads and writes but never asks for caching, so Claude
calls pay full input price on every call of a turn. This spec adds one setting,
default cache points on Anthropic Messages, the OpenAI key and retention
fields, a one-hour write bucket and rate, and a rule that keeps the prefix
stable. Provider facts are in the PRD's Context (docs read 2026-09-29).

## The setting

```go
// PromptCache asks the provider to keep the start of the request for reuse.
// The zero value caches where Dashi places cache points (Anthropic Messages
// on its public endpoint), with the provider's default lifetime.
type PromptCache struct {
    // Disable sends no cache request. A provider that caches on its own,
    // such as OpenAI or Gemini, may still cache.
    Disable bool `json:"disable,omitempty"`
    // TTL is the shortest lifetime wanted for unused entries. Zero uses the
    // provider's default. Dashi picks the shortest lifetime the route offers
    // that is at least TTL; see Lifetimes.
    TTL time.Duration `json:"ttl,omitempty"` // JSON: a Go duration string, "1h"
    // Key groups calls that share a prefix, such as one session's, so the
    // provider can route them to the same cache. It is a hint: Dashi sends a
    // hash of it where the API has a key field and ignores it elsewhere.
    Key string `json:"key,omitempty"`
}

type Settings struct {
    // ...existing fields
    PromptCache PromptCache `json:"prompt_cache,omitzero"`
}
```

`SettingPromptCache` names it in adjustments. `Settings.Validate` rejects a
negative `TTL` and `Disable` with a nonzero `TTL` ("prompt_cache: disable and
ttl are both set; remove one"). `Disable` with a `Key` is valid: OpenAI still
uses the key for accounting.

**Wire key.** `"dashi-" + hex(sha256(Key))[:32]`, 38 characters. It is
deterministic across processes, so recreated agents share it. The raw key stays
in `RequestSnapshot.Input` and the recorded agent; a host that must keep a raw
ID out of records passes its own hash.

**Where callers set it.** Direct calls set `Input.Settings.PromptCache`. Agents
set `Agent.Settings.PromptCache`, which reaches `Start.Agent`,
`Session.Agent`, and carry-forward like any setting. A session fills an empty
`Key` with its session ID when it merges the agent for a new turn
(`session_agent.go`, `agentFor`). The automatic agent revision (`defaults.go`)
leaves `PromptCache.Key` out, as it leaves out `Limits.Deadline`, so each
session does not get its own revision. Children of `delegate` keep the key of
the agent they are given.

## Lifetimes

Each model entry in the catalog gains `cache_ttls`, the lifetimes its route
offers, as `llm.Capabilities.CacheTTLs []time.Duration` (JSON: `["5m","1h"]`).
Anthropic's `limits()` states `[5m, 1h]` for the API, since every current model
takes both. Resolution runs in `Fit`, with the other settings:

| Requested `TTL` | Offered lifetimes | Effective | Adjustment |
|---|---|---|---|
| 0 | any | provider default | `default` when Dashi sends one (Anthropic 5m) |
| ≤ some offered value | e.g. `[5m, 1h]` | shortest offered ≥ TTL | none |
| > every offered value | e.g. `[30m]` | longest offered | `clamp` (warning) |
| > 0 | none known, or route has no control | omitted | `omit` (warning) |

Under `CapabilityStrict`, a clamp or omission is an error before dispatch, as
for other settings. Route tables:

- **Anthropic Messages:** `[5m, 1h]`; default 5m.
- **OpenAI Responses before GPT-5.6** (`gpt-5.5`, `5.4`, `5.2`, `5.1`, `5`,
  `4.1`): `[5m, 24h]`, where 5m is sent as `"in_memory"`. Zero sends nothing
  (the provider defaults to `24h` without zero data retention).
- **OpenAI Responses from GPT-5.6:** `[30m]`, the provider's only value. Dashi
  sends no `prompt_cache_options`; a TTL up to 30m is met without a field.
- **Gemini, Chat Completions, and other routes:** none; any nonzero TTL is
  omitted with a warning.

## Anthropic Messages

When caching is on (below), the codec adds two cache points, both
`{"type":"ephemeral"}` with `"ttl":"1h"` when the effective lifetime is 1h:

1. **Fixed point** on the system block built from `Input.System`, or, when
   `System` is empty, on the last tool definition. It covers tools and the
   system prompt. System text hoisted from history messages comes after it.
2. **Automatic point**: the top-level `cache_control`. Anthropic places it on
   the last cacheable block, walks back past ineligible blocks (thinking,
   empty text), and moves it forward each call. Calls in a turn add a few
   lookback positions each, well inside Anthropic's 20-block window; runs of
   `tool_use` and of `tool_result` blocks count as one position.

Both points share one lifetime, which meets Anthropic's rule that longer
lifetimes come first. Two of four slots leave room for a later caller-placed
point without a budget rule now. Below the model's minimum length Anthropic
caches nothing and returns no error, so no retry is needed. The codec copies
blocks and never marks caller content.

**When caching is on.** Zero value: only on the public endpoint
(`api.anthropic.com`, from the catalog's destinations), for any model. A
nonzero `TTL` is an explicit request: elsewhere it follows the normal rules for
`FeaturePromptCache` (sent and recorded as unverified under adjust, an error
under strict). A proxy that rejects the top-level field therefore breaks only
for callers who asked. `Disable` sends no cache points.

## OpenAI Responses

A nonempty `Key` becomes `prompt_cache_key` (the wire key) on every model. An
effective lifetime becomes `prompt_cache_retention` (`"in_memory"` or `"24h"`)
only on models before GPT-5.6. Usage already decodes `cached_tokens` and
`cache_write_tokens` (`llm/usage.go`). Chat Completions and compatible
endpoints get nothing in this slice.

## Gemini

Implicit caching needs no field and cannot be turned off; Dashi sends nothing
and reports `cachedContentTokenCount` as today. A nonzero TTL is omitted with a
warning.

## What the request snapshot records

`Body` holds the exact cache points, wire key, and retention. `Effective`
holds the resolved `PromptCache`: the effective TTL where Dashi sent or met
one, and the caller's key. `Adjustments` holds the default, clamp, or omit
entry; `Unverified` holds `prompt_cache` when sent without evidence. A reader
can therefore tell a call Dashi cached by default (a recorded default) from one
the caller asked for, without parsing the body.

## Usage, pricing, and budgets

```go
type Usage struct {
    // ...
    // CacheCreation1hInputTokens is the part of CacheCreationInputTokens
    // written with a one-hour lifetime. The rest used the default lifetime.
    CacheCreation1hInputTokens int `json:"cache_creation_1h_input_tokens,omitempty"`
}

type Rates struct {
    // ...
    // CacheWrite1h prices one-hour cache writes. Nil means unknown, never
    // free; usage with one-hour writes then has no estimate.
    CacheWrite1h *money.Amount `json:"cache_write_1h,omitempty"`
}
```

- The Anthropic codec decodes `usage.cache_creation.ephemeral_1h_input_tokens`,
  in whole responses and in stream frames (`Absorb` treats it like the other
  cumulative counts). `Usage.Validate` requires the subset not to exceed the
  total. Buckets stay disjoint, so totals and existing records are unchanged.
- `Pricing.Estimate` prices the subset at `CacheWrite1h` and the rest at
  `CacheWrite`, and returns no estimate when the subset is nonzero and the
  rate is nil. `Cost.CacheWrite` holds both parts.
- The catalog states `cache_write_1h` at 2x input on every Anthropic sheet and
  tier. A test pins 2x beside the 1.25x rule, and `model-catalog.md`'s pricing
  paragraph is updated in the same PR.
- The worst-case bound (`llm/risk.go`) adds `CacheWrite1h` to the maximum
  input rate when the effective TTL is 1h; a nil rate makes the bound unknown,
  so a `MaxCost` turn stops as it does for an unpriced model. The 5m default
  needs no change: the bound already uses the highest of the input rates.

## Keeping the prefix stable

The cache pays only if call *n+1* starts with the exact bytes of call *n*.
Decision 41 makes these rules binding for all features:

- **System prompt:** fixed within a turn. `modelSystem` already always sends
  the reminder instruction. Per-call facts (time, budget left, file lists) go
  in reminders, never in instructions.
- **Reminders:** user messages appended at the end, so they extend the prefix.
  A `NextCallOnly` item, the crash-resume reminder, and a replaced `Snapshot`
  item leave the projection later; the prefix then breaks where the item was,
  and the next call rewrites from there. For items delivered just before a
  call, that is one model step; a snapshot item replaced long after delivery
  costs more, and the reminders guide says so.
- **Tools:** the order the toolset returns, after selection and hooks. Dashi
  does not sort; built-in toolsets (MCP included) must return a stable order,
  checked by a test. A `BeforeModel` hook that hides tools, or a changed
  selection, invalidates everything.
- **Settings:** changing `tool_choice`, reasoning, or `Citations` between calls
  invalidates the message cache on Anthropic.
- **Known misses:** the reserved final-answer call (it adds system text and
  drops tools), compaction turns (their own summarizer prompt; the replacement
  history starts a new prefix, while the fixed point still hits), and a
  changed model.

## Alternatives considered

- **Dive's placement: fixed system point, explicit tail, and anchors.** Exact
  placement in the body, but Dashi would own ineligible-block rules that broke
  LangChain and Vercel. The automatic point's placement is visible only in
  usage; we accept that.
- **Route-specific option structs** (`anthropic.CacheOptions`, OpenAI
  retention strings). Exact, but an agent that moves between providers must
  change code, and budgets still need a common vocabulary. The TTL table keeps
  one field and records what each route did.
- **Session-level setting.** Natural for keys, but direct calls could not use
  it. The session fills the key instead.

## Tradeoffs

- Default-on raises a single uncached call by up to 25% of its cached prefix.
- Warnings on every call when a cross-provider agent asks for a lifetime a
  route lacks. That is the existing adjust contract; the fix is to not ask.
- The final-answer miss stays. Follow-up: render its instruction as a tail
  reminder and keep the tools with `tool_choice: none`.

## Rollout

Two stacked PRs, no migration (pre-release; records gain optional fields):

1. **Accounting (S):** `CacheCreation1hInputTokens`, `CacheWrite1h`,
   catalog rates, estimate and risk bound, Anthropic decoding. Fixtures:
   mixed 5m/1h usage, a sheet without the rate, streaming frames.
2. **Requests (M):** `PromptCache`, validation, `CacheTTLs`, `Fit` resolution,
   the Anthropic and OpenAI codecs, the session key, revision exclusion, and
   guide text. Fixtures: every row of the TTL table per route; system and
   tool-only cache points; a three-call turn with a reminder, a `NextCallOnly`
   item, and a snapshot item, asserting byte-equal system and tools and the
   expected message prefix; `Disable`; strict failures; caller input
   unmodified; wire key stable, distinct, and raw-free.

Live qualification records model, request, timing, and usage for Claude (5m
and 1h), a pre-5.6 OpenAI model (key and retention), and a 5.6 model. A miss
explained by length minimums or eviction does not fail it. Track 5 edits
session code in parallel; the key change touches only `agentFor`.

## Open questions

- Should `dashi` CLI default to a 1-hour lifetime, as Claude Code does for
  subscriptions? Leaning no: users pause for hours, and 2x writes add up.
  Decide after live qualification shows real hit rates.
