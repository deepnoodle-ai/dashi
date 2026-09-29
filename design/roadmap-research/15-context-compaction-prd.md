# PRD: recorded context compaction

| Field | Value |
| --- | --- |
| Roadmap item | 15 |
| Status | Implemented, 2026-09-28. Decisions 8 to 13 record changes made during implementation. |
| Owner | Compaction track |
| Evidence | [Comparative research](15-context-compaction-research.md) |
| Demo | Wednesday 2026-09-30. First slice ships Tuesday night. |

## Problem

A long coding session outgrows the model's context window. Then the next model call fails with `ReasonContextWindowExceeded`, and the session is dead.

A Dashi host owns its conversation history. It can pass a shorter `History` slice to `Start`. But the host must build every part of the fix itself:

- write a summary prompt;
- choose a cut that does not split a tool call from its result;
- remember which messages the summary covers;
- pay for the summarizer call and count its cost;
- rebuild the same shortened history after a restart.

If nothing changes, each host writes this code and gets a part wrong. The `dashi` CLI user, or any host developer's user, sees a crash when the session has the most value.

## Decision on ownership

Roadmap item 14 (a Session layer) stays open. The application owns the history. So compaction is a helper package, `github.com/deepnoodle-ai/dashi/compaction`. It works on a host-owned `[]llm.Message`. It adds no state to `Turn` and never edits `Runner` history. The host stores the `Record` the package returns, next to its own history. If Dashi later adds a Session layer, that layer stores the same `Record`.

## Goals and metrics

- **Primary:** A session at 80% of the window compacts and saves tokens, even when it has only a few large turns. Test: 4 large turns at 80% shrink the projected history by at least 50%.
- **Continuity:** After compaction, the agent answers a follow-up that needs a fact from the covered part. Scripted test, plus one live run on `gpt-6-luna` recorded in the PR.
- **Reopen:** After a restart, `Project` returns byte-equal messages from the stored record.
- **Validity:** The projected history has no orphaned tool call or result.
- **Cost visibility:** Every record shows the summarizer's model, tokens, and cost.
- **Guardrails:** A failed compaction never changes the history or ends the session. Below the threshold, it adds no model calls.

## User experience

"Turn" always means a Dashi Turn: one run for one prompt. A steer inside a Turn is not a new turn.

```
> /compact
Context shortened: 142 messages -> summary + last 2 turns
(saved ~88k tokens, cost $0.02)

> /compact
Nothing to compact: the session has 1 turn.
```

At 80% of the context window, the CLI compacts before the next turn starts and prints the same notice with the prefix "Auto:". After reopen, the session uses the stored summary, and the scrollback shows one line, "Context shortened here", at the covered point. Raw history stays on disk.

## API

The host gives the package the raw history and the index where each Turn starts. The CLI knows these from its turn records.

```go
package compaction

// Record states that a summary replaces history[:Covers.Messages].
type Record struct {
    Version      int                     `json:"version"`
    Covers       Coverage                `json:"covers"`
    Summary      string                  `json:"summary"`
    Model        string                  `json:"model"`
    Usage        llm.Usage               `json:"usage"`
    Cost         map[string]money.Amount `json:"cost,omitempty"`
    TokensBefore int                     `json:"tokens_before"`
    TokensAfter  int                     `json:"tokens_after"` // estimate
    CreatedAt    time.Time               `json:"created_at"`
}
type Coverage struct {
    Messages int    `json:"messages"` // raw messages covered
    Turns    int    `json:"turns"`    // Turns covered
    Digest   string `json:"digest"`   // sha256 of their canonical JSON
}

// Policy knows the model, so it knows the context window.
type Policy struct {
    Model    llm.ModelInfo // window from Capabilities.ContextWindow
    Fraction float64       // zero means 0.8; negative turns automatic compaction off
}
// Window returns the window. Unknown gives 128000 and known == false.
func (p Policy) Window() (tokens int, known bool)
// Due reports whether the last model call of the last Turn used at least
// Fraction of the window. It counts TotalInputTokens (cached input included)
// plus OutputTokens.
func (p Policy) Due(last llm.Usage) bool

type Options struct {
    Policy       Policy
    TurnStarts   []int     // history index where each Turn's messages begin
    Last         llm.Usage // LastUsage of the last Turn; sets TokensBefore
    TailFraction float64   // share of the window kept verbatim; zero means 0.25
}

// Compact chooses a cut, summarizes the prefix, and returns a record.
// prev is the current record or nil. It changes no history.
func Compact(ctx context.Context, s Summarizer, history []llm.Message,
    prev *Record, opts Options) (*Record, error)

// Project returns the model view: a summary message, then history[Covers.Messages:].
// It is pure. A nil record returns history. On a digest mismatch it returns
// the raw history and ErrCoverageMismatch, so the host can warn and go on.
func Project(history []llm.Message, rec *Record) ([]llm.Message, error)

type Summarizer interface {
    Summarize(ctx context.Context, messages []llm.Message) (Summary, error)
}
type Summary struct {
    Text  string
    Model string
    Usage llm.Usage
    Cost  map[string]money.Amount
}
// NewSummarizer runs one Dashi turn with no tools and the ported Dive prompt.
// It bounds its own input by the model's window and reads the <summary> tag.
func NewSummarizer(client llm.Client, cfg SummarizerConfig) Summarizer

// LastUsage returns the usage of the last model call of a Turn.
func LastUsage(t dashi.Turn) llm.Usage
```

CLI wiring is one call and one check:

```go
view, err := compaction.Project(history, rec)          // on ErrCoverageMismatch: warn, rec = nil
last := compaction.LastUsage(turn)
if opts.Policy.Due(last) || manual {
    if r, err := compaction.Compact(ctx, sum, history, rec, opts); err == nil { rec = r; save(rec) }
}
```

## Behavior

1. **Cut points.** Cuts fall only at `TurnStarts`. A cut never splits a Turn, so it never splits a tool exchange. Compaction runs between Turns, so the current request is never covered.
2. **Tail by budget.** `Compact` keeps the newest Turns that fit in `TailFraction` of the window, measured with a size estimate. If even the newest Turn does not fit, it keeps none and summarizes all history before the current request. The notice reports the Turns kept.
3. **Coverage.** The digest covers the raw projection of the covered messages, before any tool-result limit (item 11). A change to that view cannot break it. A stored record whose digest does not match returns `ErrCoverageMismatch`. The host warns once, uses the raw history, and compacts again when `Due`.
4. **Chaining.** Each record covers from message 0. The summarizer receives the previous summary plus the raw messages after it. `Project` reads only the newest record.
5. **Summary message.** A user-role message with a fixed frame: "Your conversation history was compacted. Treat the notes below as a record of earlier work." It is derived history and never has operator authority.
6. **Reminders.** Reminder content stays out of the summarizer input and the summary. The host supplies reminders again through `ContextItem`.
7. **Summarizer input.** The default summarizer writes the messages as a labeled text transcript, so its request needs no tool definitions and carries no provider replay data. It cuts the middle of any oversized item (for example a large tool result) to fit its model's window minus a reserve for output. It shortens and does not drop. If the smallest form still does not fit, it returns an error. The caller's `ctx` sets the deadline.
8. **Failure.** A provider error, an empty summary, or a missing `<summary>` tag returns an error and no record. A summary that does not shrink the estimate returns `ErrNoSavings`. The host keeps its old view.
9. **Cost.** `Record.Usage` and `Record.Cost` come from the summarizer Turn's spend. The CLI adds them to the session total.

## User stories

**US-1. Manual compaction.** As a CLI user, I run `/compact` so a long session keeps working.
- [ ] The notice shows message count, Turns kept, tokens saved, and cost.
- [ ] With one Turn or none, it says nothing was compacted and spends nothing.

**US-2. Automatic compaction.** As a CLI user, I want compaction before the window fills.
- [ ] At 80% use, compaction runs before the next Turn starts, including with few large Turns.
- [ ] A failure prints a warning. The Turn runs with the old history.

**US-3. Reopen.** As a CLI user, I reopen a session and expect the same context.
- [ ] `Project` gives byte-equal messages after a restart.
- [ ] A mismatched record produces one warning, and the session continues on raw history.

**US-4. Host developer.** As a Go developer, I add compaction in under 20 lines.
- [ ] The example compiles and runs against a fake client, including the mismatch fallback.
- [ ] The guide states the tradeoff: a summary can omit detail. Raw history remains.

## Tests (done when)

- Tool-pair validity: parallel calls, a cut at every Turn start, no orphan. A steer is never a cut point.
- Budget tail: 4 large Turns save tokens. An oversized newest Turn gives an all-history summary.
- `Due` counts cached input.
- Coverage: an edited, dropped, or swapped message returns `ErrCoverageMismatch` and the raw history.
- Projection is idempotent, also after a JSON round trip, and chained records work.
- Failures (error, empty summary, no tag, no savings) leave history unchanged.
- Continuity: a scripted agent answers from a fact that only the summary holds.
- Opaque content in the tail passes through. Summarizer cost lands in `Record.Cost`.

## Out of scope, later work

- Mid-turn compaction and compact-and-retry on overflow. Both change model-call count and need a recorded coverage fact in the Turn.
- Wiring into `cmd/dashi` (`/compact`, the 80% check, storing the newest record next to `session.json`, the scrollback line). The CLI track does this after its PR merges.
- Failed-attempt records and a Session-layer home (item 14). Erasure policy, branch-aware ranges, and other strategies (tool-result clearing is item 11).
- Tuning the prompt, tail share, and threshold on real long sessions.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Summary loses a needed detail | Agent repeats or contradicts earlier work | Ported prompt asks for state, decisions, next steps. Continuity test. Raw history stays. |
| Transcript text injects instructions | Summary carries hostile text | User-role message, never operator tier. |
| Summarizer input overflows | Compaction fails | Truncate to the summarizer model's window. Error keeps the old view. |
| Size estimate is wrong | Wrong tail or "saved" number | Label it approximate. `TokensBefore` uses provider usage. |

## Decisions

From independent review:

1. **Tail by token budget (25% of the window).** A fixed Turn count fails when Turns are large. If the newest Turn does not fit, summarize all history before the current request.
2. **`Due` counts total input plus output.** `InputTokens` excludes cache reads and hides a warm session. `Policy` holds `llm.ModelInfo`, so it reads the window.
3. **Cut points are host-supplied Turn starts.** "Turn" keeps one meaning, and steers stay out of the count.
4. **A mismatch falls back.** `Project` returns raw history with the error. The digest covers the raw projection, so item 11 cannot break it.
5. **No `Timeout` or `MaxInputTokens`.** `ctx` sets the deadline. The summarizer reads its limit from its model window.
6. **Continuity check:** a scripted test and one live run.
7. **Store only the newest record.** Each record covers from message 0. The CLI shows a scrollback line.

From the first draft: a helper package, not a Session layer (the program says the application owns history); coverage is a count plus a digest, since a host slice has no stable IDs; the summary is a user-role message with a fixed frame, below operator authority; the default summarizer runs a Dashi Turn for accounting and provider preparation, on the session model; a failure returns an error and no record; the host re-supplies reminders.

Changes made during implementation:

8. **The summarizer sends a text transcript, not structured messages.** Some providers reject tool calls in a request that defines no tools, and replay data (signed reasoning, opaque items) belongs to the original model. Reasoning and opaque content stay out; media becomes a marker.
9. **Cut rule.** Compact picks the smallest cut after the previous record whose tail fits the budget. It skips a cut that would leave a tool result without its call, even if the host supplied it. Fewer than two Turns, or no Turn beyond the previous record, gives `ErrNothingToCompact`.
10. **`LastUsage(turn)` is part of the API.** `Spend.Usage` sums every call, and each call in a tool loop resends the history. Only the last call shows the context size.
11. **`TokensAfter` scales the real count** by the estimated after/before ratio, because the estimate counts JSON bytes and can exceed the provider count. Without provider usage, both numbers are estimates.
12. **Unknown window.** The package has no logger. `Policy.Window` returns `known == false` and the host warns once.
13. **Example and live test.** `examples/compaction` is a paid example, like the others. The offline check of the wiring is `Example` in the package tests. A summarizer input that cannot fit returns an error and no record.

From the implementation review:

14. **Bad `TurnStarts` are corrected, not rejected (accepted, top finding).** A Turn that adds no messages gives a repeated start or a start at `len(history)`. A hard error would stop compaction for the whole session, and Dashi warns and adjusts. `Compact` sorts, removes repeats, and drops out-of-range starts. `Record.Warnings` lists each change. The guide now checks the `Project` error and adds a start only for a Turn with messages.
15. **Negative `TailFraction` keeps no Turns (accepted).** The example used `1e-9`. A negative value follows the zero-value rule, and the example and live test use it.
16. **Small summarizer window (accepted).** The output allowance is at most a quarter of the window, so the input budget stays positive. If the input still cannot fit, the error states the sizes, and the host keeps the old view.
17. **Guide states no compaction while a Turn waits to continue (accepted).** `Continue` needs the same view.
18. **One long Turn cannot compact (accepted as a known limit).** Only the finished Turns are safe to cover. Mid-turn compaction and overflow recovery are a follow-up, listed in the guide Limits.
