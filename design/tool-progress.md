# Tool progress: how a running tool reports to the caller

**Status:** Decided; implementing on `dx/tool-progress` · **Date:** 2026-09-28 ·
**Relates to:** decision 3, [live-delivery.md](live-delivery.md) (open question
"Tool progress"), [gaps/D](gaps/D-protocols-streaming.md) row 13,
[gaps/F](gaps/F-example-ports.md) 10.1

## The problem

A tool can run for minutes: a shell command, a dataset job, a sub-agent.
`Executor.Execute(ctx, Call) (Outcome, error)` cannot say anything before it
returns, so a UI shows a spinner and people cannot tell working from hung.
Codex users asked for this ([openai/codex#4751](https://github.com/openai/codex/issues/4751)),
as did ADK users for sub-agents ([adk-go#292](https://github.com/google/adk-go/issues/292)).
Dive's `tool_progress_example` could not be ported.

Terms. A **step** is a recorded fact (`tool_started`, `tool_settled`); it is
durable and drives recovery. A **preview** is a live-only event (model text
deltas today) that the record later supersedes. **Snapshot** progress replaces
the previous value (latest wins); **appended** output adds to it.

## What others do, and what went wrong

- **Reporter in the context** (Dive `ReportProgress`/`StreamOutput`, LangGraph
  `get_stream_writer()`). Dive ran the callback on each tool goroutine: it raced
  in parallel calls (Dive #186), blocked the tool on a slow consumer, and
  appended every chunk to the response.
- **Explicit sink parameter** (pi `onUpdate`, opencode `ctx.metadata`).
  opencode rewrites and persists the whole growing output on every chunk.
- **Tool returns an iterator** (ADK-Go `RunStream`, Eino, ADK-Python
  generators, Vercel preliminary results). Partials leak into the model-facing
  result or the model sees them and loops
  ([adk-python#5947](https://github.com/google/adk-python/issues/5947),
  [ai#19080](https://github.com/vercel/ai/issues/19080)); Eino needs four tool
  interfaces and middleware silently de-streams.
- **Protocol notifications** (MCP `notifications/progress` {progress, total,
  message}; ACP `tool_call_update`, replace). Resending growing output on each
  update made one ACP adapter O(n²); reset-on-progress timeouts kept a request
  alive forever (typescript-sdk#1001).
- **Heartbeat** (Temporal `RecordHeartbeat`, Claude Agent SDK `tool_progress`):
  liveness and a durable resume checkpoint, not a UI feed.
- **Nothing** (OpenAI Agents SDK): users kept asking.

Two lessons. **Progress is never the result and never reaches the model**
(Codex lost output that arrived only as deltas, codex#10141). **Progress must
not fail or slow the work**: sink failures never fail a turn, progress is
never replayed, and updates after the tool settles are ignored.

## Decisions

1. **A reporter carried in the context, as in Dive.** `Execute(ctx, Call)`
   stays unchanged, wrappers and toolsets pass it through, and tools that never
   report pay nothing. A parameter or a second executor interface churns every
   tool; an iterator return conflates progress with the result.
2. **Snapshot only, for now.** `tool.Report` with latest-wins semantics.
   Appended output (`tool.Output(ctx) io.Writer`) is a later addition, with the
   first shell-like tool that needs it; it will be a separate call, since
   Dive's review removed a mixed snapshot/delta field as ambiguous.
3. **`Done` and `Total` stay** as optional fields, for progress bars and a 1:1
   mapping to MCP progress. Zero `Total` means unknown.
4. **Live-only.** Progress is never recorded, replayed, or used for recovery.
   No recorded checkpoint for crash salvage now; a tool that needs resumable
   progress stores its own, keyed by `(TurnID, CallID)`. A reconnecting reader
   sees "running" from the record and waits for the next live snapshot.
5. **No runner heartbeat now.** Liveness for silent tools is the host's job for
   now (a host may also treat progress events as a liveness hint). Dashi adds
   no stuck-tool timeout.
6. **Never blocks, never after settle.** Delivery goes through a latest-wins
   slot per call, drained by the runner goroutine (below).

## API

```go
// package tool (tool/progress.go)

// Progress is a snapshot of a running call. Each report replaces the last.
type Progress struct {
	Message string          `json:"message,omitempty"` // status line, e.g. "47/100 files"
	Done    float64         `json:"done,omitempty"`    // work finished, in the tool's units
	Total   float64         `json:"total,omitempty"`   // 0 means unknown
	Data    json.RawMessage `json:"data,omitempty"`    // structured detail for custom UIs
}

// Report publishes p for the call running under ctx. It never blocks, copies
// Data, and does nothing when no one listens or after Execute returns.
func Report(ctx context.Context, p Progress)

// WithReporter returns a context whose Report calls invoke fn. Tests and
// adapters that run executors directly use it; the runner installs its own.
func WithReporter(ctx context.Context, fn func(Progress)) context.Context

// package dashi
type Event struct {
	// ...existing fields...
	// Progress is a preview from a running tool; it is never recorded.
	Progress *ToolProgress
}

type ToolProgress struct {
	CallID   string
	Name     string // the tool name, so a UI can label the call
	Progress tool.Progress
}
```

`Report` adjusts rather than fails: invalid JSON in `Data` is dropped, and a
non-finite `Done` or `Total` becomes 0. `Data` is `json.RawMessage`, not Dive's
`map[string]any`, because `cloneEvent` must deep-copy every delivered event.
No `Runner` field is needed.

The ported example (`examples/tool_progress`):

```go
tool.Report(ctx, tool.Progress{
	Message: fmt.Sprintf("processed %d/%d records", i, total),
	Done:    float64(i), Total: total,
	Data:    data, // {"bytes_processed":…, "elapsed_ms":…}
})
...
for ev, err := range runner.Stream(ctx, turn, start) {
	if err != nil { return err }
	if p := ev.Progress; p != nil {
		fmt.Printf("\r  %s: %-32s %3.0f%%", p.Name, p.Progress.Message,
			100*p.Progress.Done/p.Progress.Total)
	}
}
```

## Implementation

**Listening.** The runner installs a reporter only when someone listens: an
`Observer`, or a `Stream` range. `Run` without an `Observer` leaves `Report` a
no-op, mirroring how streamed model output is requested.

**Slots** (new `progress.go` in package dashi). A `progressHub` owned by one
`tools()` call holds, under a mutex, one pending snapshot per open call and a
`ready` channel with capacity 1. The call's reporter stores the snapshot
(replacing any unsent one) and does a non-blocking send on `ready`. The tool
never waits on the consumer.

**Draining** (`tools_loop.go`). `drainOne` already waits for results and the
context; it also selects on `ready`, emits each pending snapshot in call order
through `r.emit`, and keeps waiting. `yield` and the Observer channel are
therefore only touched on the runner goroutine, which removes Dive's race by
construction. A slow `Stream` consumer coalesces snapshots instead of stalling
the tool.

**Ordering.** The reporter is installed after `tool_started` is committed. The
tool goroutine closes its slot when `Execute` returns, before it sends its
result; `settle` emits that call's last unsent snapshot, if any, and removes
the slot before it commits `tool_settled` or `tool_waiting`, so a final
"100/100" still reaches a `Stream` consumer. Since only the runner goroutine
emits, no progress for a call follows its settle event, and nothing is emitted
after `tools()` returns, including from detached or leaked goroutines.

**Observer headroom** (`observer.go`). The Observer buffer is lossy (64
events). A progress event is queued only when the buffer is at most half full,
so a tool reporting in a tight loop cannot crowd out steps.

**Edits.** `tool/progress.go` (new); `progress.go` (new); `policy.go` (the
`Event` field); `tools_loop.go` (install reporter, `drainOne` select, drop slot
on settle); `runner.go` (`cloneEvent` copies `Progress.Data`); `observer.go`
(headroom); `examples/tool_progress`, `examples/README.md`;
`docs/guides/running-turns.md` (a short "Tool progress" section);
`live-delivery.md` (close the open question with a link here).

## Tests

- `tool`: `Report` without a reporter is a no-op; `WithReporter` receives a
  copy of `Data`; invalid `Data` and non-finite numbers are adjusted.
- `Stream`: progress events carry `CallID` and `Name`, arrive after the call's
  `tool_started` and before its `tool_settled`; the report made just before
  `Execute` returns is delivered; no step contains progress.
- Parallel calls (`ToolConcurrency: 4`) reporting in tight loops, under
  `go test -race`: events attribute to the right call and never follow settle.
- A slow `Stream` consumer does not slow a tool that reports 10,000 times.
- A goroutine that keeps reporting after `Execute` returns delivers nothing.
- `Run` without an `Observer`: `Report` is a no-op and the turn completes.
- A blocked Observer during a report flood still receives every step.

## Later

Appended output (`tool.Output`), with a bounded buffer and live-delivery's
`Gap` on overflow. Protocol bridges: ACP `tool_call_update` (throttled, `Data`
in `_meta`); MCP client forwards `notifications/progress` into `tool.Report`
with a hard timeout cap; MCP server forwards snapshots as notifications.
Sub-agent tools can forward a child turn's events as their own progress.
Detached calls report through the host's job system, keyed by `Handle.ID`.
