# PRD: recorded context compaction

| Field | Value |
| --- | --- |
| Roadmap item | 15 |
| Status | Draft for review, revised 2026-09-28 |
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

- **Primary:** After `/compact`, the next turn runs with a projected history that is at least 50% smaller in tokens on a 100-message coding session. It never contains an orphaned tool call or result.
- **Reopen:** After a restart, `Project` returns byte-equal messages from the stored record. Tested.
- **Cost visibility:** Every applied record shows the summarizer's model, tokens, and cost.
- **Guardrails:** A failed or empty summary never changes the history. Compaction adds zero model calls to a session that never crosses the threshold.

## Users

- **Primary:** the `dashi` CLI user in a long session. This user sees a one-line notice and keeps working.
- **Secondary:** a Go developer who builds an agent host on Dashi and owns the message history.

## User experience

In the CLI, the user meets three things.

```
> /compact
Context shortened: 142 messages -> summary + last 6 turns
(saved ~88k tokens, cost $0.02)

> /compact
Nothing to compact: the session has 4 turns.
```

At 80% of the context window, the CLI compacts before the next turn starts. It prints the same notice, prefixed "Auto:". After reopen, the session loads the stored record and uses the same summary. The full raw history stays on disk.

The developer meets this API:

```go
package compaction

// Cut returns n so that history[:n] ends at a settled turn boundary and
// history[n:] holds the last keep turns. It returns ErrNothingToCompact
// when the history has keep turns or fewer.
func Cut(history []llm.Message, keep int) (n int, err error)

// Record states that a summary replaces history[:Covers.Messages].
type Record struct {
    Version      int                     `json:"version"`
    Covers       Coverage                `json:"covers"`
    Summary      string                  `json:"summary"`
    Model        string                  `json:"model"`
    Usage        llm.Usage               `json:"usage"`
    Cost         map[string]money.Amount `json:"cost,omitempty"`
    TokensBefore int                     `json:"tokens_before"` // last request size
    TokensAfter  int                     `json:"tokens_after"`  // estimate
    CreatedAt    time.Time               `json:"created_at"`
}
type Coverage struct {
    Messages int    `json:"messages"` // count of raw messages covered
    Digest   string `json:"digest"`   // sha256 of their canonical JSON
}

// Project returns the model view: one summary message, then history[Covers.Messages:].
// It is pure. With a nil record it returns history. It returns
// ErrCoverageMismatch if the digest does not match history[:Covers.Messages].
func Project(history []llm.Message, rec *Record) ([]llm.Message, error)

// Compact summarizes the prefix chosen by Cut and returns the new record.
// prev is the current record or nil. It changes no history.
func Compact(ctx context.Context, s Summarizer, history []llm.Message,
    prev *Record, opts Options) (*Record, error)

type Options struct {
    KeepTurns int // zero means 6
    MaxInputTokens int // summarizer input bound; zero means 150000
    Timeout time.Duration // zero means 2m
}

// Summarizer turns messages into summary text and reports what the call used.
type Summarizer interface {
    Summarize(ctx context.Context, messages []llm.Message) (Summary, error)
}
type Summary struct {
    Text  string
    Model string
    Usage llm.Usage
    Cost  map[string]money.Amount
}

// NewSummarizer returns the default Summarizer. It runs one Dashi turn with
// no tools and the ported Dive compaction prompt. The zero SummarizerConfig works.
func NewSummarizer(client llm.Client, cfg SummarizerConfig) Summarizer

// Policy decides when to compact automatically.
type Policy struct {
    Window   int     // context window in tokens; zero reads the catalog, then 128000
    Fraction float64 // zero means 0.8; negative turns automatic compaction off
}
func (p Policy) Due(last llm.Usage) bool
```

The host flow is short:

```go
rec, err := compaction.Compact(ctx, summarizer, history, current, compaction.Options{})
if err == nil { current = rec; store.Save(sessionID, rec) }
view, _ := compaction.Project(history, current)
runner.Run(ctx, dashi.Start{History: view, Input: input})
```

## Behavior

1. **Cut.** A turn starts at a user message with no tool result and no reminder-only content. `Cut` picks the latest start such that the tail holds `KeepTurns` turns. The cut never falls inside a tool exchange, so no call loses its result. It never covers the current turn, because compaction runs only between turns.
2. **Coverage.** The record names a message count and a digest of the covered raw prefix. `Compact` and `Project` check the digest. A changed, missing, or reordered prefix returns `ErrCoverageMismatch`. The digest uses canonical JSON, like the history fingerprint, so a store that keeps JSON meaning still matches.
3. **Chaining.** A new record covers the raw prefix from message 0, not just the new part. The summarizer receives the previous summary plus the raw messages after it. `Project` reads only the newest record.
4. **Summary message.** The summary is a user-role message with a fixed frame: "Your conversation history was compacted. Treat the notes below as a record of earlier work." It is derived history. It is never operator authority.
5. **Reminders.** `Compact` leaves reminder content out of the summarizer input and the summary. The host supplies reminders again through `ContextItem`, as it does each turn.
6. **Bounded input.** `Compact` truncates any single oversized message (for example a large tool result) to fit `MaxInputTokens`. It truncates. It does not drop, so pairs stay valid.
7. **Failure.** A provider error, a timeout, an empty summary, or a missing `<summary>` tag returns an error and no record. A summary that does not shrink the estimate returns `ErrNoSavings`. In all cases the host keeps its old view.
8. **Cost.** `Record.Usage` and `Record.Cost` come from the summarizer turn's spend. The CLI adds them to the session total.
9. **Threshold.** `Due` compares the last request's input tokens plus output tokens with `Window * Fraction`. With an unknown window it uses 128,000. The CLI warns once and continues.

## User stories

**US-1. Manual compaction.** As a CLI user, I run `/compact` so a long session keeps working.
- [ ] The notice shows message counts, tokens saved, and cost.
- [ ] The next turn sends the summary plus the last 6 turns.
- [ ] With 6 turns or fewer, it says nothing was compacted and spends nothing.

**US-2. Automatic compaction.** As a CLI user, I want the CLI to compact before the window fills.
- [ ] At 80% use, compaction runs before the next turn starts.
- [ ] A failure prints a warning. The turn still runs with the old history.

**US-3. Reopen.** As a CLI user, I reopen a session and expect the same context.
- [ ] `Project` gives byte-equal messages after a restart.
- [ ] Raw messages stay readable on disk.

**US-4. Host developer.** As a Go developer, I add compaction to my host in under 20 lines.
- [ ] The example compiles and runs against a fake client.
- [ ] The guide states the fidelity tradeoff: a summary can omit detail. Raw history remains.

## Tests (done when)

- Tool-pair validity: a history with several parallel calls in one message, cut at every boundary. No orphaned call or result.
- Coverage digest: an edited, dropped, or swapped covered message returns `ErrCoverageMismatch`.
- Idempotent projection: `Project` called twice, and after a JSON round trip of the record, returns equal messages. Chained records project correctly.
- Failure paths: empty summary, no tag, timeout, and no savings leave history unchanged.
- Reminder and opaque content: signed provider content in the tail passes through unchanged.
- Summarizer cost lands in `Record.Cost`.

## In scope for Tuesday night

`compaction` package (`Cut`, `Record`, `Project`, `Compact`, `Policy`, default summarizer with the Dive prompt), tests above, `examples/compaction`, `docs/guides/compaction.md`, roadmap update.

## Out of scope, later work

- Mid-turn compaction and automatic compact-and-retry on overflow. Both change model-call count and need a recorded coverage fact in the turn. Follow-up item.
- Wiring into `cmd/dashi` (`/compact`, the 80% check, record storage). The CLI track does this after the CLI PR merges. The package API above is the whole surface it needs.
- Records for failed attempts, in-progress markers, and a Session-layer home (item 14).
- Compacting a tail that alone exceeds the window.
- Erasure policy, branch-aware ranges, other strategies (sliding window, tool-result clearing; the latter is item 11).
- Tuning the prompt and threshold on real long sessions.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Summary loses a needed detail | Agent repeats or contradicts earlier work | Ported prompt asks for state, decisions, and next steps. Raw history stays. Evaluate later. |
| Transcript text injects instructions into the summary | Summary carries hostile text | Summary is a user-role message. Never operator tier. |
| Summarizer call overflows | Compaction fails | `MaxInputTokens` bound and truncation. Error leaves the old view. |
| Token estimate is wrong | Wrong "saved" number | Label it approximate. `TokensBefore` uses the provider's usage. |
| Window unknown for a model | Threshold too high or low | Default 128,000. CLI warns. `Policy.Window` overrides. |
| Tool-heavy single turn fills the window | Between-turn compaction cannot help | Documented. Mid-turn work is a follow-up. |

## Decisions

1. **Helper package, not a Session layer.** The program says the application owns history. A helper needs no new authority over the log. It reuses the draft PRD's "applications own the log" branch.
2. **Coverage is a message count plus a digest of the raw prefix.** The host history is an ordered slice with no stable entry IDs. A count is exact, and the digest catches an edited prefix. A host with IDs can store them beside the record.
3. **Each record covers from message 0.** `Project` then reads one record. It has no chain to walk, and a lost older record cannot break the view.
4. **Default keep is 6 turns.** It matches the CLI notice and keeps recent work verbatim. The host can change it.
5. **Summary role is user, with a fixed frame.** Most providers need a user message first. It also keeps the summary out of operator authority.
6. **Default summarizer runs a Dashi turn** with no tools. It gets cost accounting, retries, and provider preparation for free. `Summarizer` stays an interface for tests and other models.
7. **Failure returns an error and no record.** The host decides what to show. This cuts the first slice. Failed-attempt records wait for a Session layer.
8. **Reminders are omitted from summaries.** The host owns them and re-supplies them. A summary must not promote them to user text.
9. **Unknown window falls back to 128,000 with a warning.** The program rule is to warn and adjust, not fail.
10. **Default summarizer model is the session model.** The user already chose it. A cheaper summary model is a later option.
