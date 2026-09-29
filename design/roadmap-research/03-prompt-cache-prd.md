# 3. Prompt caching that works by default — PRD

**Status:** Proposed, 2026-09-29; replaces the 2026-09-28 draft. **Roadmap:**
[item 3](../remaining-work.md). **Basis:**
[research](03-prompt-cache-research.md), [model
catalog](../model-catalog.md), [runtime
reminders](../runtime-reminders.md), and the discovery below. **Tech spec:**
[prompt-cache.md](../prompt-cache.md).
**Size:** M. **Risk:** public API (`llm.Settings`), new concepts (cache key,
cache lifetime).

## Problem

An agent resends the same system prompt and tool definitions on every model
call, and the whole conversation so far on every call after the first. A turn
with five tool rounds sends its prefix five times; a session sends it again on
every message. Providers discount a reused prefix by about 90%, but Dashi
cannot ask for that.

- **Anthropic caches nothing unless asked.** Dashi's Messages client sends no
  `cache_control` (`providers/anthropic/messages/messages.go`), so every Claude
  call pays full input price. This is the largest cost gap to Dive, which
  placed cache points automatically ([gap
  E](../gaps/E-providers-cost-budgets.md)).
- **Hosts invent a cache key.** Two production hosts on Dive hash a session ID
  into `prompt_cache_key`, because each turn builds a fresh agent. Sessions
  (item 14) make that the normal case for every Dashi host.
- **One-hour caching would break cost limits.** Anthropic charges 2x input for
  a one-hour cache write and 1.25x for a five-minute one. Dashi prices every
  write at 1.25x, which is exact only because it sends no cache request today
  ([model catalog](../model-catalog.md#pricing)).

If we do nothing, every Claude agent built on Dashi costs several times what
the same agent costs on Dive, and hosts keep writing cache glue.

## Context

**Dashi today.** `llm.Usage` reports cache reads and writes as disjoint input
buckets; `llm.Rates` has one `CacheWrite` rate. OpenAI Responses and Chat
Completions already decode `cache_write_tokens`, and Gemini reports cached
tokens with no write count. No provider sends a cache request field. The
Runner already keeps the system prompt stable: the reminder instruction is
always present, and reminders are user messages at the end of the history
([runtime reminders](../runtime-reminders.md)). Only the reserved final-answer
call changes the system prompt, and it also drops the tools.

**Providers (docs read 2026-09-29).**

- **Anthropic** caches only at cache points (`cache_control`), at most four per
  request, over the prefix in the order tools, system, messages. A top-level
  `cache_control` places one automatically on the last cacheable block and
  moves it forward as the conversation grows. Lifetimes are 5 minutes
  (default) and 1 hour. Writes cost 1.25x (5m) or 2x (1h) input; reads cost
  0.1x or less. A prompt below the model's minimum (512 to 4,096 tokens) is
  silently not cached. Caches are shared per workspace; there is no key.
  Changing tools invalidates everything; changing `tool_choice` or thinking
  settings invalidates the message cache. The automatic cache point works on
  the Claude API, Claude Platform on AWS, Vertex AI, and Foundry; legacy
  Bedrock rejects it with a 400. Recent models (Opus 4.8 and later, Sonnet
  5.5, Fable and Mythos 5 and later, not Sonnet 5) accept a system message
  inside `messages` without invalidating either cache.
- **OpenAI** caches automatically. Before GPT-5.6, `prompt_cache_key` improves
  routing and `prompt_cache_retention` selects `in_memory` (5 to 10 minutes)
  or `24h` (the default without zero data retention) on the models OpenAI
  lists; writes are free. From GPT-5.6, routing is automatic, the only
  lifetime is 30 minutes, writes cost 1.25x, and the key separates per-key
  accounting and blocks cache-hit probing across users; the docs do not say
  whether different keys share a prefix. Keys are limited to 64 characters.
- **Gemini** caches implicitly on 2.5 and later models above 2,048 to 4,096
  tokens. There is no request field; named cached-content resources are a
  separate API.

**Comparable libraries.** Most make caching opt-in and let the developer name
intent at a coarse level, then place cache points for them. Vercel AI SDK
takes per-part `cacheControl` with `ttl: '1h'` and OpenAI `promptCacheKey`.
pydantic-ai offers `anthropic_cache_instructions`, `_tool_definitions`, and
`_messages` settings, each `True`, `'5m'`, or `'1h'`, and trims to four cache
points. LangChain's `AnthropicPromptCachingMiddleware` marks the system prompt,
the last tool, and the tail, with `ttl='5m'|'1h'`. Codex CLI sends the thread
ID as `prompt_cache_key` on every call. Claude Code caches by default, puts
stable content first, and exposes only a lifetime and an off switch. Known
failures: a timestamp in the system prompt, tools in a changing order
(claude-code#49038), more than four cache points (langchain4j#3051), and cache
points on ineligible blocks (langchain#34920, vercel/eve#2753).

## Users and job

**Primary:** a Go developer running agents on a stable system prompt and
toolset, through `dashi.Session` or turns, who wants lower cost and latency
without learning each provider's cache fields. **Secondary:** a developer
making direct `llm.Client` calls who wants the same saving or none at all.

## Concepts

- **Prompt cache:** the provider's store of a request's leading part (tools,
  system prompt, earlier messages), reused by a later request that starts with
  exactly the same content. A reuse is a **cache read**; storing is a **cache
  write**.
- **Cache point:** a place in a request where Anthropic writes the prefix up to
  it. Dashi places them; callers do not.
- **Cache lifetime (TTL):** how long the provider keeps an unused entry. The
  caller asks for a minimum; Dashi picks the shortest lifetime the route
  offers that meets it.
- **Cache key:** an application label, such as a session ID, that groups calls
  sharing a prefix so the provider routes them to the same cache. Dashi sends
  it as given, and a hash only when it is too long for the API.

## Use cases

### UC-1: Caching with no configuration

```go
s := &dashi.Session{Runner: dashi.Runner{Client: anthropicClient}}
out, err := s.Send(ctx, "Summarize the open incidents.")
out, err = s.Send(ctx, "Which one is oldest?")
```

Acceptance:
- [ ] Each Anthropic call carries a 5-minute cache point on the system prompt
  (or the last tool when there is none) and the top-level automatic cache
  point; the request snapshot shows both.
- [ ] In a live run above the model's minimum, the second call of a turn and
  the first call of the second turn, sent within five minutes, report cache
  reads.
- [ ] A fixture with two session turns, each on a fresh agent the session
  builds, shows turn 2's first request starting with the exact bytes of turn
  1's last request: tools, system text, and leading messages.
- [ ] Calls to Gemini carry no new fields. The session's OpenAI calls carry
  the session ID as `prompt_cache_key` on models before GPT-5.6; on 5.6 and
  later only once live qualification shows different keys still share a
  prefix.

### UC-2: One-hour cache for a session with long pauses

```go
agent := dashi.Agent{Instructions: prompt,
    Settings: llm.Settings{PromptCache: llm.PromptCache{TTL: time.Hour}}}
```

Acceptance:
- [ ] Anthropic calls mark both cache points with `"ttl": "1h"`.
- [ ] Usage reports the one-hour part of cache writes; cost prices it at the
  sheet's one-hour rate (2x input for Claude) and the rest at 1.25x.
- [ ] A turn with `MaxCost` reserves each call at the one-hour write rate; a
  price sheet without that rate makes the call's worst case unknown, as for
  any unpriced model.
- [ ] On an OpenAI GPT-5.6 model the request is unchanged and the snapshot
  warns that the lifetime was clamped to 30 minutes; on Gemini it warns that
  the lifetime was omitted. Under `CapabilityStrict` both fail before dispatch.

### UC-3: Turning caching off for one-shot calls

```go
in.Settings.PromptCache = llm.PromptCache{Disable: true}
```

Acceptance:
- [ ] Anthropic calls carry no `cache_control`; other routes are unchanged and
  may still cache on their own, as the field's comment says.
- [ ] `Disable` with a nonzero `TTL` fails validation with a message naming
  both fields.

### UC-4: A host's own cache key

A server sets `PromptCache.Key` to its tenant-scoped conversation ID. Two
Runner instances with the same key send the same `prompt_cache_key`, the key
itself, so OpenAI's per-key accounting shows the host's label. A key over 64
characters is sent as a hash, and the snapshot records the change.

## Requirements

- R-1: `llm.Settings` gains `PromptCache` with `Disable`, `TTL`, and `Key`. It
  applies to direct calls and to `Agent.Settings` alike. The zero value caches
  on the Anthropic destinations the catalog marks as accepting the automatic
  cache point, with the provider's default lifetime.
- R-2: Anthropic Messages places at most two cache points: one on the block
  holding `Input.System` (or the last tool definition), and the automatic one.
  Both use the effective lifetime. Caller input is never mutated.
- R-3: OpenAI Responses sends the key as given (hashed only over 64
  characters) and `prompt_cache_retention` only when the provider's default
  may not meet the requested lifetime. The catalog, from OpenAI's model list,
  says which models offer `24h`. Gemini and other routes send nothing new.
- R-4: A requested lifetime is met or exceeded without a warning, clamped to
  the longest offered with a warning, or omitted with a warning where the route
  has no lifetime control; strict policy turns each warning into an error. A
  key is a routing hint and is never warned about.
- R-5: The request snapshot records what was sent: the body, the effective
  `PromptCache` (a defaulted lifetime appears as a recorded default), and any
  clamp or omission.
- R-6: `Usage` reports the one-hour subset of cache writes; `Rates` gains an
  optional one-hour write rate; the Anthropic catalog states it. Cost and
  budget worst cases use it, and treat a missing rate as unknown.
- R-7: A session supplies its ID as the key when the caller set none, on
  models where the catalog marks a default key safe: OpenAI Responses before
  GPT-5.6, and later models only after the live key check. The key does not
  change the agent's automatic revision.
- R-8: The prefix stays byte-stable across the calls of a turn and across
  turns: the system prompt, tool order, and earlier messages do not change as
  reminders arrive. A replaced `Snapshot` item stays in place and the new one
  is appended. Documented exceptions: the reserved final-answer call, a hook
  that hides tools, a spent `NextCallOnly` item, a system message in the
  history on Anthropic, and compaction.

## Not in scope

- Caller-placed cache points on messages. No consumer needs them; four slots
  are easy to overflow.
- Gemini cached-content resources and cache warming. Both are lifecycle
  managers with background spend.
- OpenAI GPT-5.6 explicit cache points and prewarm, and Chat Completions keys.
  Automatic caching already covers the agent case.
- Keeping the cache across the reserved final-answer call. It is one call per
  turn at most; we accept the miss. See the spec's follow-up.
- A one-hour default for the `dashi` CLI, until live hit rates exist.

## Decisions

- **On by default for Anthropic.** An agent turn almost always reuses its
  prefix within five minutes, and one read repays the write surcharge. OpenAI
  now caches (and charges writes) by default, and Claude Code does too.
  Rejected: opt-in, as in most libraries, which leaves every Claude agent at
  full price. It depends on the catalog's destination evidence, not the
  hostname: the first-party API today, then Claude Platform on AWS, Vertex
  AI, and Foundry as each is qualified; never legacy Bedrock.
- **Snapshot items are append-only in the projection.** Removing a replaced
  item breaks the prefix where it was; a per-call value such as the time
  then rewrites every message on every call. Keeping old copies costs a few
  tokens each, and the reminder instruction tells the model that the latest
  item of a name is current. Rejected: removal with a stated cost.
- **Automatic tail plus one fixed cache point.** The top-level field tracks the
  conversation and skips ineligible blocks; the fixed point keeps tools and
  system cached when messages change (compaction, a new session of the same
  agent). Rejected: placing the tail ourselves, which repeats the ineligible
  block bugs above.
- **`TTL` is a minimum duration.** It is the user's word, and "at least" lets
  one agent run on several providers. Rejected: a `short`/`long` enum, whose
  "long" means 1 hour on Anthropic and 24 hours on OpenAI.
- **The setting lives in `llm.Settings`.** Direct calls and agents share it,
  and the snapshot already records settings. Rejected: a Runner or Session
  setting, which direct calls cannot use.
- **Dashi sends the key as given.** On GPT-5.6 the key's purpose is per-key
  accounting, which a hash would make unreadable. A host that must hide an ID
  hashes it itself. Rejected: hashing every key.

## Success

A Claude agent's second and later calls in a turn read most of their input from
the cache in live qualification. Live qualification reports the net saving
(read discount minus write surcharge) for a tool turn and for a chat turn
after a pause of more than five minutes; the first must be positive.
Budget worst cases never fall below the true charge. No existing fixture
changes except Anthropic request bodies, which gain cache points.

## Risks and open questions

- **Default-on can cost more.** The automatic point covers the whole prompt,
  so a one-shot call pays the 25% write surcharge on all of its input, and a
  chat turn without tool calls after a pause of more than five minutes gets
  no reads and pays only the surcharge. Agent turns with tool calls repay it
  on the next call. One-shot callers should set `Disable`; the guide says so.
  Not blocking.
- **A proxy that speaks Messages may reject the top-level field.** Default-on
  applies only on destinations the catalog documents; elsewhere caching needs
  an explicit request under the normal capability rules. Not blocking.
- **Provider drift.** OpenAI changed its model at GPT-5.6. Catalog lifetimes
  carry dates, and live qualification records each route. Not blocking.
