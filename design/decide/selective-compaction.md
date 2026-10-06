# Selective compaction

**Status:** Proposed · **Date:** 2026-10-02 · **Owner:** Curtis · **Builds
on:** [README](README.md), [compaction guide](../../docs/guides/compaction.md)

## Problem

The stock compactor (`compaction.Compactor`) keeps a recent tail word for word
and summarizes everything older. A summary loses exact details the agent needs
later: an error string, a path, a decision and its reason. Decide's
`patterns/compact` asks, for each segment, whether it is still needed and
whether its exact text is. This design keeps the needed older segments word
for word and summarizes only the rest, at the same post-compaction size.

## Design

### Same size, split differently

Selective keeps the stock total: `Base`'s tail budget, 0.25 of the window by
default. `OlderFraction` (0.15 by default) of it goes to older segments, so
the tail is cut later, at 0.10. Older segments fill whatever room the tail
leaves. History after compaction fits the stock budget, and compactions
come no more often. (Stock often keeps less than its budget, because it cuts
only where a user message starts.)

The current turn, from the user's latest own words on, stays whole as stock
keeps it. When the turn is larger than 0.10, the tail starts at the latest
request (or at stock's cut, if that is later). A large file read in the last
turn is then never judged or shortened; it counts in the budget and leaves
less room for older segments.

### What happens to each older message

Each message before the tail falls into one class:

- **Instructions** (system and developer): kept first, as stock does.
- **The user's own words**: a user-role message with only text. It has no
  tool result, no reminder, no named `Context`, and it is not a summary. This
  covers the request, follow-ups, steers, and queued input. These are
  **pinned**: kept whole and never judged, because summaries mangle the task
  and constraints most.
- **Reminders without a sender**: removed, as stock does; the host sends
  them again.
- **Previous summaries**, as `compaction.IsSummary` recognizes them (both
  the stock and active-compaction frames): always summarized again.
- **Named items**, such as snapshots: older items of a name are removed; the
  newest is judged.
- **Segments**, judged: agent mail (a reminder with a sender) alone, or a run
  of consecutive assistant messages plus every tool-result message after it.
  A provider pause can spread one reply over several assistant messages, so a
  run is never split. Each tool call stays with all of its results.

A kept assistant segment keeps its text, tool calls, and their replay data,
but loses reasoning, as the stock summary does. A later model switch then
cannot make a provider reject old reasoning.

### Short forms

A **short form** is a smaller copy of a segment, written by code, never by a
model. The default keeps the assistant messages and call IDs, cuts each large
tool result to its first and last lines with a note saying what was cut, and
turns images and documents into a note. A host may supply its own.

### Asking Decide

The state is the task (the first of the user's own words), the latest
request, and the tail as text, cut by encoded size to fit Decide's limits.

A segment too large for a Decide request is judged by its short form and, if
kept, kept short. If its short form is too large too, it is summarized.
Selective runs `compact.Select` itself and treats any unscored segment as
dropped; Decide's own order would keep unscored segments, the big logs, first.

Thresholds follow README decision 4. At zero, a segment is kept only if
P(needed) is above 0.5, and kept whole rather than short only if P(exact text
needed) is above 0.5. A negative `MinNeeded` keeps by rank alone; a negative
`MinVerbatim` always prefers the whole text.

### The summary

One summary call, with the stock model and transcript fitting, covers what
was not kept whole: dropped segments, the full text of shortened ones, and
previous summaries. The user's own words are included, marked as already
kept, so the summarizer knows the task and the user's preferences without
restating them. Its `DefaultPrompt` asks for notes on the omitted parts:
facts, exact errors and paths, decisions with reasons, what failed, and where
the work stands. The record keeps the stock frame, so `IsSummary` recognizes
this summary next time.

The replacement history is instructions, then pinned and kept segments in
original order, then the summary, then the tail. `CompactionFact` fits
unchanged: `Keep` may have gaps, and `SummaryAt` sits just before the tail,
as in the stock layout.

### Checks and fallback

Before returning, `Start` checks `Keep`: every tool result follows its call,
and every call answered in the input is answered in `Keep`. Dashi checks
messages one at a time, so a pairing bug would otherwise break every later
request.

`Start` returns the stock Start unchanged when the client cannot be made,
Decide fails or passes `Timeout`, pinned messages exceed the budget, nothing
was dropped or shortened, or the pairing check fails. A compactor cannot add
a session warning, and returning an error makes the session skip compaction.
So the closest correct path is to fall back to stock and give the cause to
`OnSelection` in `Selection.Fallback`. A failed summary call takes the
existing `compaction_failed` path.

### Spend and the record

Decide runs inside `Start`, outside the session lock, canceled by its `ctx`.
Every `Start` calls `OnSelection` once, even one that loses the race for the
compaction slot. A host joins its spend to a turn by `Selection.Hash`, which
equals the winning `CompactionFact.Hash`. The record needs no new field:
`Keep` already shows what was kept, shortened, or dropped.

### Go API (package `selective`, in `decide/selective`)

```go
type Compactor struct {
    Base          compaction.Compactor // when to compact, window, total size, summary model
    Client        *decide.Client       // nil: decide.NewClient() from the environment
    OlderFraction float64              // window share for older segments; zero: 0.15; negative: off
    MinNeeded     float64              // zero: P(needed) above 0.5; negative: rank only
    MinVerbatim   float64              // zero: P(exact text) above 0.5; negative: always whole
    MaxRequests   int                  // concurrent Decide requests; zero: 4
    Timeout       time.Duration        // whole Decide wait; zero: 10s
    Prompt        string               // empty: DefaultPrompt
    ShortForm     func(segment []llm.Message) ([]llm.Message, bool) // nil: DefaultShortForm
    OnSelection   func(Selection)      // optional
}

func (c *Compactor) Needed(messages []llm.Message, last llm.Usage) bool
func (c *Compactor) Start(ctx context.Context, messages []llm.Message) (dashi.Start, error)

type Selection struct {
    Hash       string          // history hash, as in CompactionFact.Hash
    Segments   []SegmentChoice // message indexes, action, probabilities, size
    Usage      decide.Usage
    RequestIDs []string        // Decide request IDs
    Models     []string        // resolved decision model versions
    Fallback   string          // why stock ran; empty if selective ran
}
```

`selective.Compactor{}` works. The host pins the decision model with
`decide.WithModel`.

**Needed first:** Dashi's `Start(ctx)` outside the lock and
`compaction.IsSummary` (both being built), and request IDs, models, and usage
in Decide's `compact.Result`.

## Eval (part of v1)

**Needle in history.** Each session plants exact details early: an error
string, a path, a decision and its reason, a constraint the user stated once.
After compaction, a probe turn asks one question per needle. Sessions come
from Decide's labeled transcript and recorded CLI sessions. Every arm
compacts to the same size:

1. **Stock**, tail 0.25.
2. **Recency**: selective's layout, pinning, and prompt, but `Select` keeps
   the newest older segments without Decide. It is the cheap competitor.
3. **Selective.**
4. **Selective, summary first**, to settle where the summary goes.

The **keyless** mode uses `decidetest` with canned answers per segment and
`dashitest` for the summary and probe. It checks that needles survive word for
word in the replacement history, plus pairing, fallbacks, and the rebuilt
history. It guards the plumbing and proves nothing about Decide's judgment.
The **`-live`** mode uses real Jev and a real model, scores answers by exact
match on the planted string, and records Decide and summary tokens, Decide
wait, and fallbacks. It also sends one selective history to each provider.
Results go in `docs/dev`.

**The bar.** The package stays only if, live, selective beats both stock and
recency on needle recall by at least 10 points, falls back in under 5% of
compactions, and spends less on Decide than on the summary call. If recency
matches selective, Decide is not earning its place. Then we drop this package
and move pinning and the prompt into the stock compactor.

## Open questions

1. Unnamed items from a tool or the runtime look like the user's own words in
   history, so they are pinned. Rare enough to accept?
2. Should Dashi let a compactor attach a warning to its Start, so a fallback
   reaches the session's warnings?
3. A very large paste in the user's own words stays pinned and can force a
   fallback. Should it ever be shortened?
