# PRD: recorded context compaction

| Field | Value |
| --- | --- |
| Roadmap item | 15 |
| Status | Revised after independent review, 2026-09-28 |
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

If you do nothing, each host writes this code and gets one of the parts wrong. The CLI user sees a crash at the moment the session has the most value.

## Decision on ownership

Roadmap item 14 (a Session layer) stays open. The application owns the history. So compaction is a helper package, `github.com/deepnoodle-ai/dashi/compaction`. It works on a host-owned `[]llm.Message`. It adds no state to `Turn` and never edits `Runner` history. The host stores the `Record` the package returns, next to its own history. If Dashi later adds a Session layer, that layer stores the same `Record`.

## Goals and metrics

- **Primary:** A session at 80% of the window compacts and saves tokens, even when it has only a few large turns. Test: 4 large turns at 80% shrink the projected history by at least 50%.
- **Continuity:** After compaction, the agent answers a follow-up that needs a fact from the covered part. Scripted test, plus one live run on `gpt-6-luna` recorded in the PR.
- **Reopen:** After a restart, `Project` returns byte-equal messages from the stored record.
- **Validity:** The projected history never has an orphaned tool call or result.
- **Cost visibility:** Every record shows the summarizer's model, tokens, and cost.
- **Guardrails:** A failed compaction never changes the history or ends the session. A session below the threshold adds zero model calls.

## Users

- **Primary:** the `dashi` CLI user in a long session.
- **Secondary:** a Go developer who owns the message history in a host built on Dashi.

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
    Model    llm.ModelInfo // window from Capabilities.ContextWindow; unknown means 128000
    Fraction float64       // zero means 0.8; negative turns automatic compaction off
}
// Due reports whether the last model call of the last Turn used at least
// Fraction of the window. It counts TotalInputTokens (cached input included)
// plus OutputTokens.
func (p Policy) Due(last llm.Usage) bool

type Options struct {
    Policy       Policy
    TurnStarts   []int     // history index where each Turn's messages begin
    Last         llm.Usage // last model call of the last Turn; sets TokensBefore
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
```

CLI wiring is one call and one check:

```go
view, err := compaction.Project(history, rec)          // on ErrCoverageMismatch: warn, rec = nil
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
7. **Summarizer input.** The default summarizer truncates any oversized message (for example a large tool result) to fit its model's window minus a reserve for output. It truncates and does not drop, so pairs stay valid. The caller's `ctx` sets the deadline.
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

- Tool-pair validity: parallel calls in one message, cut at every Turn start. No orphan.
- Steer: a steer message inside a Turn is never a cut point.
- Budget tail: 4 large Turns at 80% compact and save tokens. A single oversized Turn gives an all-history summary.
- Threshold: `Due` counts cached input. A warm-cache usage crosses the threshold.
- Coverage: an edited, dropped, or swapped covered message returns `ErrCoverageMismatch` and the raw history.
- Idempotent projection: equal messages twice, and after a JSON round trip. Chained records project correctly.
- Failure paths: empty summary, no tag, and no savings leave history unchanged.
- Continuity: a scripted agent answers a follow-up from a fact in the covered part.
- Opaque provider content in the tail passes through unchanged. Summarizer cost lands in `Record.Cost`.

## In scope for Tuesday night

The `compaction` package above, the tests, `examples/compaction`, `docs/guides/compaction.md`, and the roadmap update.

## Out of scope, later work

- Mid-turn compaction and compact-and-retry on overflow. Both change model-call count and need a recorded coverage fact in the Turn.
- Wiring into `cmd/dashi` (`/compact`, the 80% check, storing the newest record next to `session.json`, the scrollback line). The CLI track does this after its PR merges.
- Records for failed attempts and a Session-layer home (item 14).
- Erasure policy, branch-aware ranges, other strategies (tool-result clearing is item 11).
- Tuning the prompt, tail share, and threshold on real long sessions.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Summary loses a needed detail | Agent repeats or contradicts earlier work | Ported prompt asks for state, decisions, next steps. Continuity test. Raw history stays. |
| Transcript text injects instructions | Summary carries hostile text | User-role message, never operator tier. |
| Summarizer input overflows | Compaction fails | Truncate to the summarizer model's window. Error keeps the old view. |
| Size estimate is wrong | Wrong tail or "saved" number | Label it approximate. `TokensBefore` uses provider usage. |
| One Turn fills the window | Between-Turn compaction cannot help | Documented. Mid-turn work is a follow-up. |

## Decisions

Review findings:

1. **Accept: tail by token budget.** A fixed count of Turns fails when few Turns are large. The tail is 25% of the window. If the newest Turn does not fit, summarize all history before the current request.
2. **Accept: `Due` counts total input plus output.** `InputTokens` excludes cache reads and hides a warm session. `Policy` holds `llm.ModelInfo`, so it reads the window. "Last usage" is the last model call of the last Turn. `Options.Last` sets `TokensBefore`.
3. **Accept: cut points are Turn starts the host supplies.** This keeps "turn" one meaning and keeps steers out of the count.
4. **Accept: mismatch falls back.** `Project` returns raw history with the error. The digest covers the raw projection, so item 11 cannot break it. The session never fails.
5. **Accept: drop `Timeout` and `MaxInputTokens`.** `ctx` sets the deadline. The summarizer derives its limit from its model window. The tag check moves into `NewSummarizer`.
6. **Accept: continuity check.** A scripted test and one live run prove the agent keeps earlier facts.
7. **Accept: store only the newest record; show a scrollback line.** Each record covers from message 0, so older records have no use. Both are CLI work.

Kept from the first draft:

8. **Helper package, not a Session layer.** The program says the application owns history.
9. **Coverage is a count plus a digest.** A host slice has no stable entry IDs.
10. **Summary is a user-role message with a fixed frame.** Providers need a user message first, and it stays below operator authority.
11. **The default summarizer runs a Dashi Turn** for accounting and provider preparation. It uses the session model, which the user already chose.
12. **Failure returns an error and no record.** Failed-attempt records wait for a Session layer.
13. **Reminders stay out of summaries.** The host re-supplies them.
