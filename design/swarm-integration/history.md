# Continuing a saved conversation without building another conversation layer

See the [main recommendations](README.md). The developer's job is to store the
conversation and decide when the agent can work. Dashi should make loading it,
continuing it, and summarizing it a supported workflow.

## R05 — let applications use Session with their own storage rules

[Before and after](examples/r05-application-turn-ids.md).

**Problem.** Dashi's `Session` offers convenient chat, but assumes numbered Turn
IDs, can skip unreadable historical Turns with a warning, and can close stopped
work when new input arrives. Swarm uses its own request IDs and needs to preserve
safely resumable work and refuse broken history. Session already protects
pending approvals/tool results and unresolved effects; those are not gaps. It therefore uses Runner directly
and builds the surrounding conversation behavior itself.

**Change.** Make these policies configurable. Add support for ordered Turns with
application-chosen IDs and a strict mode: stop on inconsistent history, and
require a decision before closing safely stopped resumable work. Keep Turn IDs
and command retry keys separate. Preserve
the history/settings used when a Turn was first accepted.

Keep the simple numbered-session path easy. Applications should implement only
the storage operations they need; a large mandatory Store would make adoption
harder. Start by testing these changes against Swarm's workflow.

**Benefit:** production applications could use Session's history and compaction
support. This is not yet evidence that Swarm's whole conductor can become one
`Session.Send` call.

## R14 — make “refuse broken history” easy to request

[Before and after](examples/r14-broken-conversation.md).

**Problem.** `BuildHistory` can warn after omitting an unreadable Turn or using
an unmatched summary. [Swarm][build] treats every warning as a recovery error.
The wrapper is short, but a new adopter can miss this safety decision.

**Change.** Add an explicit strict history option that reports the Turn or
summary causing the problem. Separate harmless notices from integrity failures.
Use the same option in Session and lower-level execution helpers.

**Benefit:** safer, clearer behavior with less warning-to-error glue. Keep
original records available when construction fails. Strict construction checks
supplied records; it cannot independently verify omitted covered history.

## R15 — query only history needed after an applied summary

[Before and after](examples/r15-load-less-history.md).

**Problem.** [Swarm][history] loads retained Turns before BuildHistory stops at
a successful summary.

**Action now.** Today's BuildHistory explicitly accepts a suffix starting at the
latest completed summary. Query from the application's latest activated,
previously validated summary, including all subsequent Turns in order. Freeze
that history at admission; continuations use their original history.

**Benefit:** fewer reads may be possible without a new Dashi API. The 13-of-100
example is arithmetic, not a benchmark. If the stored boundary cannot be
trusted, validate covered records before omitting them. Defer a lazy-loader or
checkpoint protocol until measurement shows remaining duplication.

## R16 — validate and assemble a summary replacement

[Before and after](examples/r16-summary-restart.md).

**Problem.** [Swarm's 331-line adapter][history] saves summary inputs, runs an
accounted Turn, validates its replacement history, and applies it safely.

**Change.** Start with a pure compaction result validator and activation value.
Reuse Compactor.Start and ReplacementHistory. Check the finished summary against
its frozen Start/history; give the host the validated replacement to save.

**Benefit:** remove repeated validation and activation-value assembly. The app
still creates/reuses a durable job before sending, binds its summary Turn,
preserves account/deadline, checks source revision, and atomically activates once.
Unknown writes require reload; old history stays active until activation commits.
Required failures must leave the oversized request waiting.

**Defer the durable coordinator.** LoadOrCreate/Advance concealed those host
contracts. A complete adapter comparison must demonstrate further simplification
before adding job storage or scheduling APIs.

## R17 — use the summarization Dashi already offers during a Turn

[Before and after](examples/r17-growing-turn.md).

**Problem.** A tool-heavy agent can keep accumulating text until its current
Turn becomes too large. [Swarm's Start builder][start] does not configure the
existing `Agent.ActiveCompaction` feature.

**Action now.** Test that feature in Swarm. It can summarize growth inside the
current Turn and may avoid ending it just to summarize and create a successor.
Keep summaries distinguishable from replies and account for their model calls.

It does not rewrite earlier history supplied by the caller or guarantee that
large images fit. Between-Turn summaries remain useful. Future additions could
explain what made the request too large and support a safe summarize-and-resume
operation. Test large tool output, images, failed summaries, restart, and low
model-call allowances; unresolved tools must survive without executing twice. Today ActiveCompaction
requires a Client even with an explicit Window, breaking R01's current no-client
queued-cancellation path. Qualify record-only admission and preserve resolved
window evidence before adopting this in Swarm.

## R18 — use the conversation cache key Dashi already supports

[Before and after](examples/r18-conversation-cache-key.md).

[Swarm's Runner][runner] leaves `Runner.CacheKey` empty. Use a stable conversation
ID or privacy-preserving hash, as Session does. The catalog applies it where
useful. This is an integration change, not a missing API, and provider caching
still does not guarantee a hit or a particular saving. Document the choice for
single-request Generate separately.

[build]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/compaction.go#L167
[history]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/compaction.go#L47
[recording]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/recording.go#L40
[start]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L847
[runner]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L621
