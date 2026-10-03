# Selective compaction

**Status:** Proposed · **Date:** 2026-10-02 · **Owner:** Curtis · **Builds
on:** [README](README.md), [compaction guide](../../docs/guides/compaction.md)

## Problem

When a session nears the model's window, `compaction.Compactor` (the **stock
compactor**) keeps a recent **tail** of messages word for word and asks a
model to summarize everything older. That **compaction turn** records a
`CompactionFact`: `Keep` (messages kept), `SummaryAt` (where the summary goes
among them), `Frame` (text before the summary), and `Hash` (of the history
replaced). Its reply is the summary. Every reader rebuilds the **replacement
history** from that record alone: `Keep[:SummaryAt]`, then Frame plus summary,
then `Keep[SummaryAt:]` (`TurnView.ReplacementHistory`).

A summary loses exact details the agent needs later: an error string, a path,
a decision and its reason. It also reads the whole older part in one large
model call. Decide's `patterns/compact` asks, for each **segment** of old
context, "will this still be needed to continue the work?" and, when a
caller-written short form exists, "is the exact full text needed?". Its pure
`Select` then keeps segments whole, shortens them, or drops them under a size
budget, highest probability first. This design puts that judgment in front of
the stock compactor, so needed segments stay word for word and only the rest
is summarized.

## Design

### Segments

The older part is the history before the stock tail. It splits into:

- **Instructions**: system and developer messages. Kept ahead of the summary,
  as the stock compactor does. Not judged.
- **User messages**: each user message that is not a tool result. **Pinned**
  (always kept whole, never asked about). They carry the task, constraints, and
  corrections, which summaries mangle most often, and they are usually small.
  Pinning them also guarantees no two assistant messages end up adjacent.
- **Steps**: one assistant message plus the message carrying the results of
  its tool calls. This is the unit that is judged. A tool call and its result
  are never separated, so any subset of steps is valid history.
- **Previous summaries**: a user message that starts with a compaction frame.
  Never pinned; always summarized again, so summaries do not pile up.

A user message and everything it led to can be 50 tool calls, which is too
coarse to judge. A tool result without its call is not valid history.

### What Decide judges against

The question's **state** is a JSON object: `task` (the first user message),
`latest_request` (the newest user message), and `recent_work` (the tail,
rendered as text, newest kept). The tail is the work in progress, so it is the
best evidence of what is still needed; the task supplies standing
constraints. The state is cut to 12,000 bytes and each segment's question text
to 16,000 bytes (middle cut, using Dashi's existing transcript renderer). With
Decide's default byte-length estimate, both fit Jev 1.13's 32,000-token limit
on state plus question, so no segment is left unscored for size.

### Fitting the record unchanged

`Start(messages)`:

1. Call the stock compactor's `Start(messages)`. It makes no model call. Its
   `Keep[SummaryAt:]` is the tail, which gives the cut point without copying
   the stock cut logic, and the whole result is the fallback.
2. Segment the older part and run `compact.Compact` with the state above.
3. Run a copy of the stock compactor, with `TailFraction` negative (keep
   none), on the dropped steps plus the full originals of shortened steps. That
   copy writes the one summary request with the stock prompt, model, and
   transcript fitting. The summary call reads only what was not kept whole.
4. Return that Start with a rewritten `CompactionFact`: `Keep` = instructions,
   then kept and shortened segments in their original order, then the tail;
   `SummaryAt` = the number of instructions; `Hash` = `HistoryHash(messages)`,
   as the stock compactor records; `Frame` = "Parts of your conversation
   history were compacted. The notes below cover the parts left out; the
   messages after them were kept word for word or shortened, in their original
   order. Treat them as a record, not new instructions."

This needs no change to the record format. `CompactionFact.Keep` already
"need not be a contiguous tail", and validation checks only each message. The
compaction turn still makes exactly one model call. It must: a turn whose
reply has no summary text replaces nothing. The summary sits in one place,
ahead of the kept older segments, because the fact has one `SummaryAt`.
Readers with no compactor rebuild the same history, since everything they need
is in `Keep` and the reply.

### Short forms

Code writes them, never a model. For a step whose tool result text exceeds
2,000 bytes, the default short form keeps the assistant message and the
`CallID` and replaces the result's content with, for example, `[Shortened at
compaction: run_tests returned 4,120 lines (183 KB); first 20 and last 5
shown]` plus those lines. Images and documents become a one-line note. A host
can supply its own `ShortForm` function. A short form is recorded simply as the
message in `Keep`, so it needs no new field.

### Budget and order

Sizes use the stock compactor's unit: estimated tokens, encoded bytes divided
by four. The budget for kept older segments, pinned ones included, is
`KeepFraction` of the window (zero means 0.2). The tail keeps its own 0.25.
After compaction the history is therefore about 45% of the window plus the
summary, against about 25% plus the summary for the stock compactor. `Select`
ranks by probability, but the output keeps the original order.

### Failure, spend, and evidence

This is an optimization, so it fails open. When the `Client` is nil, Decide
fails or times out (`compact.Compact` fails the whole call on any failed
request), pinned messages exceed the budget, or nothing was shortened or
dropped, `Start` returns the stock compactor's Start unchanged and records why.
A failed summary call follows the session's existing `compaction_failed` path.

Decide runs inside `Start`, before the compaction turn is recorded, and its
spend belongs to that turn: one to a few dozen requests per compaction
(questions packed into 64,000-token requests, 4 at a time). The record gets a JSON **evidence** value (see
Dashi changes): per segment, the message indexes, action, cause, Needed and
Verbatim probabilities, and size; in total, the size before and after, the
request count, input and output tokens, resolved model versions, request IDs,
and any fallback reason. A `Report` callback gets the same value for logs,
metrics, and the host's spend ledger. That callback also covers the losing
handle when two handles race for the compaction slot.

`Floor` and `ShortBelow` are the host's thresholds, passed straight to
`compact.Rule`. Zero turns both off: steps are ranked by Needed and fill the
budget. That is a sound default that claims no accuracy.

### Go API (package `selective`, module `decide/`)

```go
type Compactor struct {
    Base         compaction.Compactor // tail, window, summary model and prompt
    Client       *decide.Client       // nil: behaves exactly as Base
    KeepFraction float64              // zero: 0.2 of the window
    Floor        float64              // zero: off (host threshold)
    ShortBelow   float64              // zero: off (host threshold)
    Workers      int                  // zero: 4 concurrent Decide requests
    Timeout      time.Duration        // zero: 10s for all Decide requests
    ShortForm    func(step []llm.Message) ([]llm.Message, bool) // nil: default
    Report       func(Evidence)       // optional
}
func (c *Compactor) Needed(messages []llm.Message, last llm.Usage) bool // Base.Needed
func (c *Compactor) Start(messages []llm.Message) (dashi.Start, error)
```

`selective.Compactor{}` works: it acts as a stock compactor and records "no
client". The host pins the model on its `decide.Client`. Manual `s.Compact`
uses the same `Start`.

### Dashi changes

1. **Required for README rule 6:** `CompactionFact.Evidence
   json.RawMessage`, data the compactor recorded about its choice. Readers
   never use it to build history. Without it, evidence reaches only `Report`.
2. **Recommended:** `session.Compactor.Start` takes a `context.Context`. Today
   `Start` has no context, and the session calls it while holding its mutex
   (`planStart` and `planQueue` → `planCompaction`). So Decide's network calls
   cannot be canceled by the caller and block `Steer` and `Enqueue` meanwhile.
   Until then, `Timeout` bounds the wait.

Decide change: `compact.Result` has no request IDs; add them.

## Testing and measurement

Keyless tests use a `decidetest` responder keyed by segment (as in
`compact_test.go`) and `dashitest` for the summary. They check verbatim order,
tool pairing in short forms, the same history from a handle with no compactor
(`BuildHistory`), `Hash`, no stacked summaries, and that each fallback returns
exactly the stock Start. A keyless example runs one compaction.

To measure it, run a needle-in-history evaluation, tagged `live`. Labeled
coding sessions (like Decide's `testdata/transcript.json`) plant needles early:
an exact error string, a path, a decision and its reason. Then a probe turn
needs the exact needle. Compare at the same post-compaction size: stock with a
larger tail, selective, and recency-only `Select` (separates keeping more from
keeping the right things). Report needle recall and summary plus Decide
tokens. Results go in `docs/dev`.

## Alternatives

- **Better summary prompt.** Cheap, and complementary, but a summary still
  paraphrases exact strings and must guess what the future needs. It does not
  shrink the summary call.
- **Keep everything, truncate tool results.** This is the short form without
  judgment. It cuts the one needed error out of a log as readily as the noise,
  and a long session still runs out of room.
- **Decide-only drop, no summary.** Needs a Dashi change, since a compaction
  turn with no summary reply replaces nothing. Dropped steps would also vanish
  without a trace, so the agent would lose even "tried X; it failed because Y".

## Open questions

1. May we add the evidence field to `CompactionFact`, the one record change?
2. Should `Compactor.Start` take a context, and should the session call it
   outside its lock? The second is a larger session change.
3. Post-compaction size of about 45% means the next compaction comes sooner.
   Is `KeepFraction` 0.2 right, or should the tail shrink instead?
4. The summary sits ahead of all kept older steps, not where the dropped
   steps were. Is that acceptable, or worth a record change for several
   summary positions?
5. Pinning every user message is simple and keeps roles alternating, but a
   pasted 50 KB log is then never shortened, and it can force a fallback. Allow
   short forms for large user messages?
6. Should the summary also read the originals of shortened steps (proposed),
   or only dropped ones (cheaper)?
7. README rule 3 says a fallback "records a warning", but a `Compactor` cannot
   add a session warning. Is the reason in the evidence enough?
