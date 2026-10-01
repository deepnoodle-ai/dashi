# Prompt caching

An agent sends the same system prompt and tools on every model call, and the
whole conversation so far on every call after the first. Providers keep the
start of a request (the prefix) and charge about a tenth of the input price
when a later request starts with the same bytes. Dashi asks for that by
default where it must, and gives one setting, `llm.Settings.PromptCache`, for
the rest.

## What happens with no configuration

- **Anthropic (Claude API):** every call carries two cache points with the
  5-minute lifetime: one on the system prompt (or the last tool when there is
  none) and Anthropic's automatic one, which moves forward with the
  conversation. The request snapshot records the lifetime as a `default`
  adjustment of `prompt_cache.ttl`.
- **OpenAI and Gemini** cache on their own. Dashi sends nothing new.
- **Sessions:** a `session.Session` sends its ID as OpenAI's `prompt_cache_key`
  on models before GPT-5.6, where a key per conversation improves cache
  routing. The model catalog's `cache_key_default` marks those models. The
  session sets `Runner.CacheKey`; a host that drives `Runner` itself sets that
  field to its conversation ID.

Prompts below the model's minimum length (512 to 4,096 tokens on Claude) are
not cached, and no error is reported.

## The setting

```go
agent := dashi.Agent{
	Instructions: prompt,
	Settings: llm.Settings{
		PromptCache: llm.PromptCache{TTL: time.Hour},
	},
}
```

- `TTL` is the shortest lifetime you want. Dashi sends the shortest lifetime
  the route offers that is at least `TTL`: 1 hour on Claude, 24 hours on the
  OpenAI models that offer it. Above the longest it sends the longest, with a
  `setting_clamped` warning (GPT-5.6 offers only 30 minutes). Where no
  lifetime can be requested (Gemini, Chat Completions, and Responses routes
  such as Grok's whose model info does not state `prompt_cache` support) it
  is omitted with a `setting_omitted` warning. Under `CapabilityStrict` both
  are errors.
- `Key` groups calls that share a prefix, such as one conversation's. OpenAI
  Responses receives it as `prompt_cache_key`, as given, so OpenAI's per-key
  accounting shows your label; a key over 64 characters is sent as a hash.
  Hash an ID yourself if it must stay off the wire. Other routes ignore it
  without a warning.
- `Disable` sends no cache request. Use it for one-shot calls: a call whose
  prefix is never read again pays the write surcharge (25% of input on Claude)
  for nothing. OpenAI and Gemini may still cache on their own.

Direct calls set `llm.Input.Settings.PromptCache` the same way.

## Cost and budgets

Claude bills 5-minute writes at 1.25x input and 1-hour writes at 2x.
`llm.Usage.CacheCreation1hInputTokens` is the one-hour part of
`CacheCreationInputTokens`, and `Pricing.Estimate` prices it at the sheet's
`CacheWrite1h` rate. A turn with `MaxCost` and a one-hour lifetime reserves
each call at that rate; a price sheet without it cannot bound the call, so the
turn stops `limited` as it does for an unpriced model.

## Keeping the prefix stable

The cache pays only when each request starts with the bytes of the one
before. Dashi keeps its own additions stable: the system prompt is fixed for a
turn, reminders are added at the end, and a `Snapshot` context item that
replaces an older one is added after it rather than in its place. Your part:

- Put per-call facts (the time, remaining budget) in reminders or `Snapshot`
  items, never in `Instructions`. Each replaced snapshot copy stays in the
  session history, since removing it would break the cached prefix, so keep
  snapshot items small and change them rarely.
- Keep the toolset's order stable. Hiding tools in a `BeforeModel` hook, or a
  different tool selection, resets the whole cache.
- Changing `ToolChoice`, reasoning, or `Citations` resets Anthropic's message
  cache.

Known misses: the reserved final-answer call, compaction (the fixed cache
point still hits), a changed model, and on Anthropic a system message in the
history, which Dashi moves into the top-level system prompt. See
[design/prompt-cache.md](../../design/prompt-cache.md) for the reasons.
