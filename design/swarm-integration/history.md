# Continuing a saved conversation without building another conversation layer

See the [main recommendations](README.md). The developer's job is to store the
conversation and decide when the agent can work. Dashi should make loading it,
continuing it, and summarizing it a supported workflow.

## R05 — let applications use Session with their own storage rules

**Problem.** Dashi's `Session` offers convenient chat, but assumes numbered Turn
IDs, can skip unreadable historical Turns with a warning, and can close stopped
work when new input arrives. Swarm uses its own request IDs and needs to preserve
unfinished work and refuse broken history. It therefore uses Runner directly
and builds the surrounding conversation behavior itself.

**Change.** Make these policies configurable. Add support for ordered Turns with
application-chosen IDs and a strict mode: stop on inconsistent history, and
require a decision about unfinished work before starting new work. Preserve
the history/settings used when a Turn was first accepted.

Keep the simple numbered-session path easy. Applications should implement only
the storage operations they need; a large mandatory Store would make adoption
harder. Start by testing these changes against Swarm's workflow.

**Benefit:** production applications could use Session's history and compaction
support. This is not yet evidence that Swarm's whole conductor can become one
`Session.Send` call.

## R14 — make “refuse broken history” easy to request

**Problem.** `BuildHistory` can warn after omitting an unreadable Turn or using
an unmatched summary. [Swarm][build] treats every warning as a recovery error.
The wrapper is short, but a new adopter can miss this safety decision.

**Change.** Add an explicit strict history option that reports the Turn or
summary causing the problem. Separate harmless notices from integrity failures.
Use the same option in Session and lower-level execution helpers.

**Benefit:** safer, clearer behavior with less warning-to-error glue. Keep
original records available when construction fails.

## R15 — avoid loading old records a summary has made unnecessary

**Problem.** [Swarm][history] loads and validates retained Turns before calling
BuildHistory. BuildHistory itself walks backward and may stop at a successful
summary. Swarm can therefore do storage work the history builder will not use.
The Recorder and command loop also repeatedly load entire Turns.

**Change.** Let history construction read Turns lazily, newest first. Stop at
a validated, applied summary. Give a continuing Turn access to the original
history version it started with. Cache reconstructed views by Turn ID and saved
sequence where worthwhile, retaining the records needed to rebuild them.

**Benefit:** potentially fewer reads and copies on long conversations. This
needs measurement. Summary validation may still require older records unless a
trusted checkpoint records that they were checked; an index entry alone is not
a reason to skip validation.

## R16 — help applications save and apply conversation summaries

**Problem.** Summarizing text is only part of the job. [Swarm's 331-line adapter][history]
also saves what is being summarized, runs an accounted summary Turn, checks the
replacement history, and applies it safely. It has to recover if any stage
fails or the process restarts.

**Change.** Extend the `compaction` package with reusable saved-job states,
validation, and the changes needed to apply a successful summary. Use the
application's storage and executor. Make it explicit whether summarization is
optional or required before the next request can fit.

`Compactor.Start` and `ReplacementHistory` already build the request and summary
history. Reuse them. Preserve job identity across repeated writes; apply the
summary only after the application's transaction saves it. Keep the old history
on failure. Required summarization must not fall back to the oversized request.

**Benefit:** remove repeated job coordination and validation. Swarm keeps its
queue, account, deadline, and transaction. The adapter's full 331 lines are not
all removable.

## R17 — use the summarization Dashi already offers during a Turn

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
model-call allowances; unresolved tools must survive without executing twice.

## R18 — use the conversation cache key Dashi already supports

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
