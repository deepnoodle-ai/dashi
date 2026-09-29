# Prompt caching: tech spec

**Status:** Proposed, 2026-09-29. **PRD:** [roadmap item
3](roadmap-research/03-prompt-cache-prd.md). **Decision:** [41](decisions.md).
**Workflow:** spec, then build in parallel with review. Terms (prompt cache,
cache point, cache lifetime, cache key) are in the
[glossary](ubiquitous-language.md).

## Context

This spec adds one setting, default Anthropic cache points, the OpenAI key
and retention fields, one-hour write accounting, and prefix rules. Provider
facts are in the PRD's Context (docs read 2026-09-29).

## The setting

```go
// PromptCache asks the provider to keep the start of the request for reuse.
// The zero value caches where Dashi places cache points (Anthropic Messages
// on destinations the catalog marks), with the provider's default lifetime.
type PromptCache struct {
    // Disable sends no cache request. A provider that caches on its own,
    // such as OpenAI or Gemini, may still cache.
    Disable bool `json:"disable,omitempty"`
    // TTL is the shortest lifetime wanted for unused entries. Zero uses the
    // provider's default. Dashi picks the shortest lifetime the route offers
    // that is at least TTL; see Lifetimes.
    TTL time.Duration `json:"ttl,omitempty"` // JSON: a Go duration string, "1h"
    // Key groups calls that share a prefix, such as one session's, so the
    // provider can route them to the same cache. It is a hint: Dashi sends it
    // where the API has a key field (hashed if too long) and ignores it
    // elsewhere.
    Key string `json:"key,omitempty"`
}

type Settings struct {
    // ...existing fields
    PromptCache PromptCache `json:"prompt_cache,omitzero"`
}
```

Adjustments name the lifetime as `SettingPromptCacheTTL`
(`"prompt_cache.ttl"`): the recorded settings treat each `prompt_cache` field
as its own setting, so a lifetime can be omitted while a key stays.
`Input.Validate` rejects a negative `TTL` and `Disable` with a nonzero `TTL`
("prompt_cache: disable and ttl are both set; remove one"). `Disable` with a
`Key` is valid: OpenAI still uses the key for accounting.

**Wire key.** The key as given, so OpenAI's per-key accounting shows the
host's label. A key over the API's 64 characters is sent as `"dashi-" +
hex(sha256(Key))[:32]` (38 characters, deterministic across processes).
`Effective` keeps the caller's key and `Body` records the hash; no adjustment
is recorded, since a key is never warned about. A host that must keep a raw ID
off the wire or out of records passes its own hash.

**Where callers set it.** Direct calls set `Input.Settings.PromptCache`. Agents
set `Agent.Settings.PromptCache`, which reaches `Start.Agent`,
`Session.Agent`, and carry-forward like any setting. The automatic agent
revision (`defaults.go`) leaves `PromptCache.Key` out, as it does
`Limits.Deadline`. Children of `delegate` keep their agent's key.

**Session key.** A session hands its ID to the Runner as a fallback key. When
the agent sets no `Key` and the model's knowledge sets `cache_key_default`
(`Capabilities.CacheKeyDefault`), the Runner puts the ID in the call's input
settings before hooks run, so the model intent records it as requested; the
recorded agent keeps no key. That flag is on for
OpenAI Responses before GPT-5.6, where the key improves routing. On 5.6 a
key may isolate caches, so a key per session would make each session pay
1.25x to write the shared tools-and-system prefix; the flag stays off there
until the live key check (Rollout) shows two keys sharing a prefix.

## Lifetimes

Each model entry in the catalog gains `cache_ttls`, the lifetimes its route
offers, shortest first, as `llm.Capabilities.CacheTTLs` of type
`llm.Durations`, a `[]time.Duration` that JSON writes as `["5m","1h"]`.
Anthropic's `limits()` states `[5m, 1h]` for the API, since every current model
takes both. Resolution runs in `Fit`, with the other settings:

| Requested `TTL` | Offered lifetimes | Effective | Adjustment |
|---|---|---|---|
| 0 | any | provider default | `default` when Dashi sends one (Anthropic 5m) |
| ≤ route's sure default | e.g. OpenAI `[5m, 24h]` | default, no field | none |
| ≤ some offered value | e.g. `[5m, 1h]` | shortest offered ≥ TTL | none |
| > every offered value | e.g. `[30m]` | longest offered | `clamp` (warning) |
| > 0 | none known, or route has no control | omitted | `omit` (warning) |

A route's *sure default* is the lifetime it gives with no field for every
account; Dashi sends a field only when that may fall short. Under
`CapabilityStrict`, a clamp or omission is an error before dispatch, as for
other settings. Route tables:

- **Anthropic Messages:** `[5m, 1h]`; sure default 5m.
- **OpenAI Responses before GPT-5.6:** sure default 5m (`in_memory`, which
  zero-data-retention accounts get; others get `24h`). Models on OpenAI's
  extended-retention list (`gpt-5.5`, `5.5-pro`, `5.4`, `5.2`, `5.1` and its
  codex and chat variants, `5`, `5-codex`, `4.1`) offer `[5m, 24h]`; others
  offer `[5m]`. The catalog copies that list, not a version rule. A TTL of
  5m sends nothing (keeping `24h` where the account has it); 1h sends
  `"24h"`.
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

Both points share one lifetime, meeting Anthropic's rule that longer
lifetimes come first, and leave two of four slots free. Below the model's
minimum length Anthropic caches nothing and returns no error. The codec
copies blocks and never marks caller content.

**When caching is on.** Zero value: on catalog destinations marked
`auto_cache` (a new `Destination` field), for any model whose knowledge does
not reject `prompt_cache`; under `CapabilityStrict` only where it states
support, as for the `MaxTokens` default. Catalog Claude models state it. Today
that is only
the Claude API; Claude Platform on AWS, Vertex AI, and Foundry, documented
as accepting the automatic point, are added as each is qualified. Legacy
Bedrock, which rejects it with a 400, never is. A nonzero `TTL` is an
explicit request: elsewhere it follows the rules for `FeaturePromptCache`
(unverified under adjust, an error under strict), so a proxy that rejects
the field breaks only for callers who asked. `Disable` sends no cache
points. Default-on is this feature's decision (PRD); decision 41 holds only
the prefix rule.

## OpenAI Responses

A nonempty `Key` (the caller's, or the session's where allowed) becomes
`prompt_cache_key` (the wire key) on every model. A lifetime becomes
`prompt_cache_retention` only when the sure default may fall short, which in
practice means `"24h"` on listed models before GPT-5.6. Both fields go to
OpenAI's endpoint, or to another Responses endpoint (such as xAI's) only when
its ModelInfo states `prompt_cache` support. Usage already decodes
`cached_tokens` and `cache_write_tokens` (`llm/usage.go`). Chat Completions
and compatible endpoints get nothing in this slice.

## Gemini

Implicit caching needs no field and cannot be turned off; Dashi sends nothing
and reports `cachedContentTokenCount` as today. A nonzero TTL is omitted with a
warning.

## What the request snapshot records

`Body` holds the exact cache points, wire key, and retention. `Effective`
holds the resolved `PromptCache`: `TTL` is the minimum met (30m stays 30m
though Anthropic sends 1h), clamped, or omitted, and `Key` is the caller's.
`Adjustments` holds any lifetime default, clamp, or omit; `Unverified` holds
`prompt_cache` when a lifetime is requested without evidence. A recorded
default marks a call cached by default. Chat Completions and Gemini declare
`prompt_cache` unsupported, so their `Fit` omits a lifetime.

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
- The catalog states `cache_write_1h` at 2x input on every Anthropic sheet,
  pinned by a test; `model-catalog.md`'s pricing paragraph is updated.
- The worst-case bound (`llm/risk.go`) adds `CacheWrite1h` to the maximum
  input rate when the call may write one-hour entries: an effective TTL above
  5m, unless the model's known lifetimes show the route sends another one
  (OpenAI's 24h or 30m). A nil rate makes the bound unknown, so a `MaxCost`
  turn stops as it does for an unpriced model. The 5m default needs no
  change: the bound already uses the highest of the input rates.

## Keeping the prefix stable

The cache pays only if call *n+1* starts with the exact bytes of call *n*.
Decision 41 makes these rules binding for all features:

- **System prompt:** fixed within a turn. `modelSystem` already always sends
  the reminder instruction. Per-call facts (time, budget left, file lists) go
  in reminders, never in instructions.
- **Reminders:** user messages appended at the end. A spent `NextCallOnly`
  item or crash-resume reminder leaves the projection, so the next call
  rewrites one model step.
- **Snapshot items (changed here):** the projection (`project.go`) keeps a
  replaced `Snapshot` item in place and appends the new one. Removal broke
  the prefix at the old item: with a per-call value such as the time, every
  cache entry ended in that item, so each call hit only the fixed point and
  rewrote every message at 1.25x (2x for 1h). The reminder
  instruction gains "When reminders share a name, the latest is current";
  `Turn.Context` still returns the latest. Each session turn is projected
  on its own, so this changes nothing across turns. See [runtime
  reminders](runtime-reminders.md).
- **Tools:** the toolset's order, unsorted; built-in toolsets (MCP too) keep
  it stable, checked by a test. Hiding tools in a `BeforeModel` hook, or a
  changed selection, invalidates everything.
- **Settings:** changing `tool_choice`, reasoning, or `Citations` invalidates
  Anthropic's message cache.
- **Known misses:** the reserved final-answer call (it adds system text and
  drops tools), compaction (a new history; the fixed point still hits), a
  changed model, and, on Anthropic, a system or developer message in the
  history: the codec moves its text into the top-level system after the
  fixed point (`messages.go`, near line 343), so one arriving mid-session
  invalidates the message cache.

**Follow-up.** Anthropic now keeps both caches for a `{"role":"system"}`
message inside `messages` on recent models (PRD Context). A catalog flag
would let the codec keep such messages in place, and the final-answer call
could use one, but it also drops the tools, so we accept its miss.

## Alternatives considered

- **Dive's placement: fixed system point, explicit tail, and anchors.** Exact,
  but Dashi would own ineligible-block rules that broke LangChain and Vercel.
- **Route-specific option structs** (`anthropic.CacheOptions`, OpenAI
  retention strings). Exact, but an agent that moves between providers must
  change code, and budgets still need a common vocabulary.
- **Session-level setting.** Direct calls could not use it.

## Tradeoffs

- Default-on costs more where nothing is read back: a one-shot call pays the
  25% surcharge on all its input, and a turn without tool calls after a
  pause over five minutes pays only the surcharge.
- A cross-provider agent asking for a lifetime a route lacks warns on every
  call; that is the adjust contract.
- The final-answer miss stays (see the follow-up above).
- Append-only snapshots let the model see stale values and grow the history.
- The `dashi` CLI keeps the default lifetime, not 1 hour: users pause for
  hours, and 2x writes add up. Revisit once live hit rates exist.

## Rollout

Two stacked PRs, no migration (pre-release; records gain optional fields):

1. **Accounting (S):** `CacheCreation1hInputTokens`, `CacheWrite1h`,
   catalog rates, estimate, Anthropic decoding (the risk bound moved to PR 2,
   since it reads the effective TTL). Fixtures:
   mixed 5m/1h usage, a sheet without the rate, streaming frames.
2. **Requests (M):** the setting, catalog fields, `Fit`, both codecs, the
   session key, append-only snapshots, and guides. Fixtures: each TTL row
   per route; system and tool-only cache points; a three-call turn with a
   reminder, a `NextCallOnly` item, and a replaced snapshot, each request
   starting with the previous one except at the spent item; two session
   turns on fresh agents, turn 2's first request starting with the bytes
   of turn 1's last; `Disable`; strict failures; caller input unmodified;
   the key raw, hashed only over 64 characters.

Live qualification records requests and usage for Claude (5m and 1h), a
pre-5.6 OpenAI model, and a 5.6 model; reports the net saving (read discount
minus write surcharge) for a tool turn and for a chat turn after a pause over
five minutes; and runs the **key check**: one prefix on a 5.6 model under two
keys, where only a cache read on the second key turns on `cache_key_default`.
Misses from length minimums or eviction do not fail it. Track 5 edits session
code in parallel.
