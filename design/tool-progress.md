# Tool progress: how a running tool reports to the caller

**Status:** Decided · **Date:** 2026-09-28 · **Relates to:** decision 3,
[live-delivery.md](live-delivery.md), [gaps/D](gaps/D-protocols-streaming.md)
row 13, [gaps/F](gaps/F-example-ports.md) 10.1

> **Superseded API:** hooks ([hooks.md](hooks.md), decision 31) replaced
> `Policy`. Its `BeforeModel`, `BeforeSend`, `AuthorizeTool` and `OnStop`
> are now the `BeforeModel`, `BeforeSend`, `BeforeTool` and `BeforeFinish`
> hook points; `ModelAuthorization` is `SendDecision`; approvals no longer
> carry a policy version; and a tool with no hook opinion runs.

## The problem

A tool can run for minutes: a shell command, a dataset job, a sub-agent.
`Execute(ctx, Call) (Outcome, error)` cannot say anything before it returns,
so a UI shows a spinner and people cannot tell working from hung
([openai/codex#4751](https://github.com/openai/codex/issues/4751),
[adk-go#292](https://github.com/google/adk-go/issues/292)).

A **step** is a recorded fact (`tool_started`, `tool_finished`). A **preview**
is a live-only event, like model text deltas, that the record supersedes.
**Snapshot** progress replaces the previous value; **appended** output adds to
it.

## What others do

Dive carried a reporter in the context, but ran the callback on each tool
goroutine: it raced in parallel calls (Dive #186) and blocked tools on a slow
consumer. Iterator-returning tools (ADK, Eino, Vercel) leaked partials into
the result or the model
([adk-python#5947](https://github.com/google/adk-python/issues/5947),
[ai#19080](https://github.com/vercel/ai/issues/19080)). MCP and ACP send
replace-style snapshots; resending growing output made one ACP adapter O(n²).
Heartbeats (Temporal) show a worker is alive and save checkpoints; they serve
no UI. Lessons: **progress is never the result and never reaches the model**,
and **progress never fails or slows the work**.

## Decisions

1. **A reporter carried in the context.** `Execute` is unchanged, wrappers pass
   it through, and tools that never report pay nothing.
2. **Snapshot only.** Appended output (`tool.Output(ctx) io.Writer`) is a later,
   separate call, added with the first shell-like tool that needs it.
3. **`Done` and `Total` stay** for progress bars and MCP mapping; 0 `Total`
   means unknown.
4. **Live-only.** Never recorded, replayed, or used for recovery; no crash
   checkpoint. A tool needing resumable progress keeps its own, keyed by
   `(TurnID, CallID)`.
5. **No runner heartbeat.** Showing that a silent tool is still running is
   the host's job for now.
6. **Never blocks, never after the call finishes, no rate limit.** Tools
   should report at human rates (a few per second); consumers throttle
   rendering.

## API

```go
// package tool (tool/progress.go)
type Progress struct {
	Message string          `json:"message,omitempty"` // e.g. "47/100 files"
	Done    float64         `json:"done,omitempty"`    // in the tool's units
	Total   float64         `json:"total,omitempty"`   // 0 means unknown
	Data    json.RawMessage `json:"data,omitempty"`    // detail for custom UIs
}

// Report publishes p for the call running under ctx; each report replaces the
// last. It never blocks and does nothing when no one listens.
func Report(ctx context.Context, p Progress)

// WithReporter returns a context whose Report calls invoke fn synchronously on
// the reporting goroutine; fn must be safe for concurrent use.
func WithReporter(ctx context.Context, fn func(Progress)) context.Context

// package dashi
type Event struct {
	// ...existing fields...
	Progress *ToolProgress // preview from a running tool; never recorded
}
type ToolProgress struct {
	CallID string
	Name   string // tool name
	tool.Progress
}
```

`Report` adjusts rather than fails: it copies `Data`, drops `Data` that is
invalid JSON or larger than 64 KiB, and turns a non-finite `Done` or `Total`
into 0. `Data` is raw JSON because every event sent to a consumer is
deep-copied.
Progress events carry `Event.TurnID`. No `Runner` field is added.

```go
for ev, err := range runner.Stream(ctx, turn, start) {
	if err != nil { return err }
	if p := ev.Progress; p != nil && p.Total > 0 {
		fmt.Printf("\r%s: %s (%.0f%%)", p.Name, p.Message, 100*p.Done/p.Total)
	}
}
```

## Implementation

**Reporter per call.** The runner always installs a reporter for every call it
executes, after committing `tool_started`. It shadows any reporter from an
outer turn, so a nested runner's tool never reports on the parent's call. When
no one listens (no `Observer`, not `Stream`), it is a no-op.

**Slot.** Each call has a slot: a mutex, the latest unsent snapshot, and a
closed flag. The reporter holds a pointer to its own slot; it stores the
snapshot (replacing any unsent one) and does a non-blocking send on a shared
`ready` channel of capacity 1. `execute` closes the slot in a `defer`, so a
panic closes it too. A closed slot rejects new reports but keeps its last
unsent snapshot.

**Drain.** `drainOne` (tools_loop.go) also selects on `ready`. It copies
pending snapshots out under each slot's lock, releases the lock, then emits
them in call order. The lock is never held while emitting, so a slow `Stream`
consumer never blocks `Report`; snapshots merge instead. Only the runner
goroutine emits, which removes Dive's race by construction.

**Finish.** Before committing a call's `tool_finished` or `tool_waiting`, the
runner emits its last unsent snapshot, so a final "100/100" arrives, and then
forgets the slot. Nothing for that call follows, including reports from a
detached call or a leaked goroutine, and nothing is emitted after `tools()`
returns.

**Lag.** Snapshots wait while the runner authorizes or commits another call.
They merge; the latest is emitted at the next wait.

**Observer priority.** The Observer queue is lossy (64 events). A progress
event is queued only while the queue is under half full, and a step or delta
arriving at a full queue evicts the oldest queued progress event, so progress
never costs the Observer an event that would otherwise fit. Once the turn's
context ends (for example, a broken `Stream`), no more progress is emitted.

**Edits.** New `tool/progress.go` and `progress.go`; `policy.go` (field and
docs); `tools_loop.go`; `runner.go` (`cloneEvent`, Observer doc);
`observer.go`; `examples/tool_progress`; `docs/guides/running-turns.md`;
`live-delivery.md`.

## Tests

- `tool`: no-op without a reporter; `Data` copied; adjustments applied.
- `Stream`: events carry `TurnID`, `CallID`, `Name`; they fall between the
  call's `tool_started` and `tool_finished`; the report just before return
  arrives.
- A consumer that blocks on the first progress event: a tool reporting 10,000
  times still returns, and its last snapshot arrives before `tool_finished`.
- Parallel calls, several goroutines reporting in one tool, `ToolConcurrency`
  1 with two calls, all under `-race`.
- Panic after reporting; a waiting or detached call that keeps reporting;
  breaking out of `Stream` or canceling while reporting; nested runner;
  `Observer` and `Stream` together, each with its own `Data` copy.
- A report flood never takes the Observer queue past half full.

## Later

Appended output, with bounded buffers and live-delivery's `Gap` signal. ACP
`tool_call_update` and MCP progress bridges (with a hard timeout cap). Sub-agent
tools forwarding a child turn's progress. Detached calls report through the
host's job system, keyed by `Handle.ID`.
