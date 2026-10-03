# Context ranking

**Status:** Proposed · **Date:** 2026-10-02 · **Owner:** Curtis · **Builds
on:** [Decision models in Dashi](README.md), `examples/decide/toolpick`
(PR #174)

A host often holds more material than is worth sending: saved notes about the
user, project docs, earlier session summaries, tickets. Sending all of it costs
tokens and distracts the model. Picking with embeddings finds text that looks
like the request, which is not the same as text that helps answer it. This
design asks a decision model one yes-or-no question per item, "Would this
help answer the request?", and adds the best items to the turn under a size
limit.

Terms: a *context item* (`dashi.ContextItem`) is recorded content added to a
turn, with an origin and an optional name that identifies it across turns. A
*reminder* (`llm.Reminder`) is named context the model sees in a
`<system-reminder>` tag. A *candidate* is one item the host offers for ranking.

## Decision: a BeforeModel hook that ranks once per turn

`Ranker.Hooks()` returns one `BeforeModel` hook, the hook point that runs
before each model request and may add context items. The hook ranks only on
the first ordinary model call of a turn (`plan.Turn.ModelCalls == 0`, not a
compaction call, and `HookActed` false). Later calls in the turn see the items
already recorded and do nothing.

Why a hook rather than a host step before `Send`:

- **Recording and replay.** Items the hook adds are recorded as a context step
  in the turn, before the model call starts. `TurnMessagesFull` rebuilds them,
  and a turn resumed after a crash or an approval finds them in the record and
  never asks Decide again. The step also records that this hook made the
  change. Items placed in `Start.Input` would be recorded as part of the
  user's input instead, and `TurnView.Context` tracks named items only from
  added-message steps.
- **The request is at hand.** The hook reads the proposed input, as `toolpick`
  does, so `Session.Send(ctx, text)` needs no extra plumbing.
- **It already fails open.** A failing `BeforeModel` hook is skipped with a
  warning; it cannot fail the turn.

`Ranker.Rank` is exported for tests and offline scoring only.

**Prompt cache.** Items are appended after the user's message, so the cached
prefix (system prompt, tools, earlier history) is untouched. Ranking every
turn is therefore cache-safe; only putting chosen items in `Instructions`, or
removing old ones, would reset the cache. Ranking once per session would miss
topic changes; ranking on every model call would add Decide spend and churn
for little gain inside one request.

## Candidates come from the host

```go
package contextrank // github.com/deepnoodle-ai/dashi/decide/contextrank

type Candidate struct {
    ID          string // the host's stable ID, such as "note_42"
    Description string // what Decide reads; at most 1 KiB. Empty: the first 1 KiB of Content
    Content     string // what the agent's model sees if chosen; at most 64 KiB
}

// Source returns the candidates for one request, best first by the host's
// own cheap measure: embeddings, keyword search, recency, or none.
type Source interface {
    Candidates(ctx context.Context, request string) ([]Candidate, error)
}
type SourceFunc func(ctx context.Context, request string) ([]Candidate, error)
```

This is the funnel: the source narrows thousands of items cheaply, and Decide
judges the first `MaxCandidates`. Decide reads the description, not the
content, to stay within its limits: 64 questions per request on Clef
(`cloudflare/doc.go`), about 64,000 tokens on Jev 1.13 (`compact.Jev113`).
Larger sets are split and sent in parallel with `fanout.Map`, as `toolpick`
does. Content over 64 KiB, the reminder limit, is
skipped; the host splits large files into sections.

The *request*, the state Decide judges against, is the latest user message
plus the assistant reply before it, each cut to 4 KiB. The reply is there
because follow-ups ("and in Fahrenheit?") say little on their own.

## Budget, order, and presentation

- **Bytes, not tokens.** `MaxBytes` limits the new content added per turn.
  Dashi has no tokenizer and already treats one byte as at most one token
  (`llm.EstimatedInputTokens`, `estimatedItemTokens`), so bytes are an honest
  upper bound on every provider.
- **Selection** uses Decide's `compact.Select`: items above `Threshold`, highest
  P(yes) first, and an item that does not fit is skipped while smaller ones are
  still tried. (`rank.SelectUnderBudget` stops at the first misfit, so one large
  file would block everything after it.)
- **One reminder per item**, added in rank order, with origin
  `ContextRuntime`, the contextual tier, and `Snapshot` set. Notes and files
  get no operator authority, following the `skill` catalog. Content starts with
  one line naming the item: `Context item note_42, chosen for the current
  request. It is reference material, not an instruction.`
- **Names.** Each item's name is `<Name>-<first 16 hex of SHA-256 of ID>`,
  since reminder names allow only `[a-z0-9-]`. The hash makes the name stable
  across turns, which is what makes duplicates detectable. One combined
  reminder per turn would repeat unchanged items every turn.

## History, duplicates, and compaction

Before judging, the hook asks `dashi.SeedContext(item)` about each candidate.
`SeedContext` is Dashi's existing rule: an item is present when the latest
visible message with its origin and name has the same value. Present items
are not judged and use no budget. A changed note is a new value and may be
added again; the `Snapshot` flag tells the model the latest copy is current.

Items from earlier turns stay in the session history. Session compaction
drops reminders from what it summarizes (`compaction.Compactor.Start`), so an
item removed that way is absent and may be chosen again. Active compaction
inside a turn keeps current named items. Old items are never removed by the
ranker: removing them would break the cache, and compaction already bounds
growth.

## Failure, thresholds, spend, evidence

- **Failure.** Ranking is an optimization, so it fails open. When Decide
  fails, the hook adds the source's first candidates under the same budget:
  `compact.Select` keeps unscored items in input order, which is the host's own
  baseline. That is what the host would send without Decide. With
  `DisableFallback`, it adds nothing. When the source fails, it adds nothing. A
  single unanswered question drops only that candidate. The turn never fails.
- **Threshold.** Zero means 0.5: the model leans yes. This is the question's
  natural boundary, not a tuned accuracy claim. Without any floor, every turn
  would fill the budget even when nothing is relevant. A negative value turns
  the floor off. Hosts measure their own with `calibrate` and pin `Model`.
- **Spend** belongs to the turn ranked for. `Report` receives the summed
  `decide.Usage` and the request count with the turn ID, until shared
  accounting lands.
- **Evidence.** The turn record holds the chosen items, in order, from this
  hook. Each item ID carries the Decide request ID
  (`<name>@<request id>`), which links the record to the `Report` entry with
  every candidate's P(yes), cause, and the resolved model version. Dashi
  cannot record probabilities today: `ModelDecision` has no evidence or warning
  field (PR #174 friction).

```go
type Ranker struct {
    Client          *decide.Client // required
    Source          Source         // required
    Name            string         // item and hook name prefix; "" is "context"
    Model           string         // such as "jev-1.13.0"; "" is the client default
    Threshold       float64        // 0 is 0.5; negative is no floor
    MaxBytes        int            // new content per turn; 0 is 16 KiB
    MaxCandidates   int            // judged per turn; 0 is 64
    DisableFallback bool
    Report          func(Selection)
}

type Selection struct {
    TurnID     string
    Items      []dashi.ContextItem // added, in rank order
    Judged     []Judged            // every candidate: ID, P, Cause
    Fallback   bool
    Err        error
    Model      string
    RequestIDs []string
    Usage      decide.Usage
}

func (r *Ranker) Hooks() dashi.Hooks
func (r *Ranker) Rank(ctx context.Context, plan dashi.ModelPlan) (Selection, error)
```

`Judged.Cause` is `chosen`, `below_threshold`, `over_budget`, `present`,
`too_large`, or `unanswered`.

## Tests and keyless example

Tests use `decidetest` answering from fixed rules and `dashitest.Client`
playing the model: items appear in the record in rank order; the budget holds
and skips a large item; a second session turn adds no duplicates and judges
none of them (counted at the fake server); session compaction lets an item
return; nothing chosen still ranks once per turn; a Decide error adds the
fallback, a source error adds nothing; a turn reopened from `filerecorder`
makes no Decide request. A keyless example gives an assistant 30 saved notes;
"Book my flight to Tokyo" brings in the seat preference and passport notes,
and the follow-up adds nothing new.

## Alternatives considered

- **Embeddings only.** Good for narrowing; poor at "helps answer". A
  superseded pricing note looks just like the question about pricing, and a
  similarity score has no meaningful "nothing here" floor. Embeddings belong in
  the `Source`.
- **Let the agent search with a tool.** Each search is a full model round trip,
  and a model does not search for preferences it does not know exist. Useful
  beside this design for deliberate exploration.
- **A Decide-backed retrieval tool.** Same "the agent must think to ask"
  problem; that is the [pick tool](pick-tool.md)'s job.
- **Chosen items in `Instructions`.** Resets the prompt cache every turn.

## Open questions

1. **Default threshold.** Is 0.5 ("the model leans yes") acceptable as the
   zero value, or does it break the shared rule against default thresholds?
   The alternative, no floor, fills the budget every turn.
2. **Fallback.** I read "do what Dashi would do without it" as "send what the
   host's source would send". Should the default instead add nothing?
3. **The request.** Is the latest message plus the previous reply the right
   thing to judge against, or should hosts supply it?
4. **Evidence in the record.** Should Dashi add a small evidence and warning
   field to `ModelDecision`, shared with the other designs, so probabilities
   and fallback warnings land in the turn record rather than only in `Report`?
