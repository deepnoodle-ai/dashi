# Tool progress: how a running tool reports to the caller

**Status:** Proposal; research and recommendation · **Date:** 2026-09-28 · **Relates
to:** decision 3 and [live-delivery.md](live-delivery.md) (open
question "Tool progress"), [gaps/D](gaps/D-protocols-streaming.md) row 13

## The problem

A tool can run for minutes: a shell command, a dataset job, a sub-agent. Today
`Executor.Execute(ctx, Call) (Outcome, error)` (tool/tool.go:367) has no way to
say anything before it returns. A UI shows a spinner, and users cannot tell
working from hung. Codex users asked for exactly this ("Know that the process
has not hung or stalled", [openai/codex#4751](https://github.com/openai/codex/issues/4751));
ADK users ask for sub-agent progress that reaches the caller
([adk-go#292](https://github.com/google/adk-go/issues/292),
[adk-python#3984](https://github.com/google/adk-python/issues/3984)).

Terms. A **step** is a recorded fact in the turn (`tool_started`,
`tool_settled`); it is durable and drives recovery. A **preview** is a live-only
event (model text deltas today) that the record later supersedes. **Snapshot**
progress replaces the previous value (latest wins); **appended** output adds to
what came before.

## What others do

| Pattern                                     | Who                                                                                                                                                                                                                                                                                                                                                                                | What went wrong                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| ------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Reporter carried in `ctx`                   | Dive `ReportProgress` + `StreamOutput` (dive/context.go:48-99); LangGraph `get_stream_writer()`                                                                                                                                                                                                                                                                                    | Dive: the callback ran synchronously on each tool goroutine, raced the collector in parallel calls (fixed in Dive #186), blocked the tool on a slow consumer, and every chunk was appended to `Response.Items` (dive/turn.go:83, 289). LangGraph's Python context variables fail in async code before 3.11 ([docs](https://docs.langchain.com/oss/python/langgraph/streaming), [#6447](https://github.com/langchain-ai/langgraph/issues/6447)); Go's `ctx` has no such gap                                                                                                                                                                                                                                                                                                                                          |
| Explicit sink parameter                     | pi `execute(id, params, signal, onUpdate)` (packages/agent/src/types.ts:452-457); opencode `ctx.metadata({title, metadata})`                                                                                                                                                                                                                                                       | opencode rewrites and persists the whole growing output on every chunk (session/prompt.ts:659-673, tool/bash.ts:175-183)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| Tool returns an iterator of partial results | ADK-Go `RunStream` → `iter.Seq2[string, error]`; Eino `StreamableRun`; ADK-Python generator tools; Vercel AI SDK "preliminary results" ([ai#7987](https://github.com/vercel/ai/pull/7987))                                                                                                                                                                                         | ADK-Go concatenates chunks into the model-facing result outside live mode, so the caller sees nothing (internal/llminternal/base_flow.go:1394-1405), and streaming tools bypass callbacks ([adk-go#1515](https://github.com/google/adk-go/issues/1515)). ADK-Python sends each yield to the model, which re-invokes the tool in a loop ([adk-python#5947](https://github.com/google/adk-python/issues/5947)). Vercel persisted a preliminary result and sent it to the model as final ([ai#19080](https://github.com/vercel/ai/issues/19080), [#21083](https://github.com/vercel/ai/issues/21083)). Eino needs four tool interfaces; middleware silently de-streams ([eino#1064](https://github.com/cloudwego/eino/issues/1064)); sibling streams leak ([eino#1256](https://github.com/cloudwego/eino/issues/1256)) |
| Protocol notifications                      | [MCP `notifications/progress`](https://modelcontextprotocol.io/specification/2025-06-18/basic/utilities/progress) {progress, total, message}: live, "progress MUST increase", "SHOULD implement rate limiting"; [ACP](https://agentclientprotocol.com/protocol/tool-calls) `tool_call_update` (replace), v2 `tool_call_content_chunk` (append); A2A artifact `append`/`last_chunk` | codex-acp crashed, O(n²), from resending the growing output on each update (acp-agent-adapter.md:203, 400). An unchecked progress counter plus reset-on-progress timeouts keeps a request alive forever ([typescript-sdk#1001](https://github.com/modelcontextprotocol/typescript-sdk/issues/1001))                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| Heartbeat                                   | [Temporal `RecordHeartbeat`](https://pkg.go.dev/go.temporal.io/sdk/activity); Claude Agent SDK [`tool_progress`](https://code.claude.com/docs/en/agent-sdk/typescript) (elapsed seconds every 30 s, no tool payload)                                                                                                                                                               | Temporal details are durable, throttled (≤ 80% of the timeout, 30–60 s), and read back on retry: a resume checkpoint plus liveness, not a UI feed                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| Nothing                                     | OpenAI Agents SDK                                                                                                                                                                                                                                                                                                                                                                  | Users asked repeatedly (openai-agents-python #661, #692; openai-agents-js #1209); the maintainer said UI-only progress belongs on "an app-level event channel", not the run-item stream                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |

Three needs keep getting mixed up: a _liveness heartbeat_ (Temporal, Claude),
a _status snapshot_ for people (MCP, Dive `ReportProgress`), and _appended
content_ (ACP chunks, A2A append, Dive `StreamOutput`). Most bugs above come
from blurring progress into the final result or into model context. Hence two
lessons. First, **progress must never be the only copy of the result, nor reach
the model**: Codex's JSON mode lost command output that arrived only as deltas
([openai/codex#10141](https://github.com/openai/codex/issues/10141)); Vercel and
ADK-Python fed partials to the model. Second,
**progress is a UX affordance and must not fail or slow the work**: OpenHands
states "sink failures never fail a turn" and "progress frames are never
replayed" (comparison/openhands.md:478, 557). pi ignores updates after the
tool settles (types.ts:437-438).

## Recommendation

**1. Pattern: a reporter carried in the context, as Dive did.** It keeps
`Execute(ctx, Call)` unchanged, works through wrappers and toolsets (an MCP
adapter can forward remote progress without new interfaces), and costs tools
that never report nothing. An explicit parameter is more discoverable, but it
changes every executor and `tool.Func` callback for a feature most tools skip;
ADK-Go's merged context broke every user mock
(comparison/adk-go.md:128). An iterator
return conflates progress with the result and forks the tool interface, the
root of the Eino and ADK-Go issues above.

**2. Shape: both, as two calls with one meaning each.** A snapshot
(`Progress`, latest wins) serves percent bars and status lines and maps 1:1 to
MCP's {progress, total, message}. Appended output (`Output`, an `io.Writer`)
serves terminals and logs, where snapshots of growing text are quadratic.
Keep them separate: Dive's review removed a `Content` field from the snapshot
because "snapshot vs delta" was ambiguous and nobody used it (Dive ad1107b).
Ship `Progress` first; add `Output` with the first shell tool, per
live-delivery's "wait for a tool that needs it".

**3. Durability: live-only, in the preview class.** Progress is not a fact the
loop acts on, so it is not recorded. The settled `Outcome` must carry
everything the model and the record need. Recording each update would multiply
commits (nvoken already notes about five transactions per tool call,
comparison/nvoken-cloud.md:215) and bloat replay. Consequences:

- _Recovery and replay._ After a crash, `tool_started` without a settle is
  uncertain, as today. A reconnecting reader sees "running" from the record and
  picks up the next live snapshot; nothing is replayed. A snapshot is
  self-contained, so a gap just means "wait for the next one".
- _Detached calls._ Reports after `Execute` returns are dropped, including for
  `StateDetached`. Progress of detached work belongs to the host's job system,
  keyed by `Handle.ID` or `CallID`, because no runner is running.
- _Heartbeats._ Temporal's heartbeat is really a durable resume checkpoint plus
  a liveness timeout. Dashi's lease heartbeat stays with the host
  (turn-interruption.md). A tool that needs resumable progress stores its own
  checkpoint keyed by `(TurnID, CallID)`. A host may treat progress events as a
  liveness hint; Dashi adds no stuck-tool timeout.
- _Backpressure._ `Report` never blocks. The runner keeps one pending snapshot
  per call and delivers the latest when the consumer is ready, so a slow
  `Stream` range coalesces instead of stalling the tool. Output chunks go into a
  bounded buffer; overflow drops chunks and raises live-delivery's `Gap`.
  Protocol bridges throttle further (ACP: at most about 4 updates a second).

**4. Delivery.** Tools run on goroutines (tools_loop.go:356), but `Stream`'s
`yield` must run on the runner goroutine. The tool's reporter therefore writes
to the per-call slot and signals the dispatch loop, which already waits in
`drainOne`; the loop emits the event. That removes Dive's race by
construction and guarantees no progress for a call after its `tool_settled`
step. The reporter is installed only when someone listens (an `Observer` or a
`Stream` range), mirroring how `Run` requests streamed model output.

- **Observer and `Stream`:** a new `Event.Progress` field, a preview like
  `Delta`.
- **Live-update protocol:** a preview frame without a cursor id, keyed by
  `CallID`; output frames keyed by `(CallID, Offset)` like model deltas.
- **ACP:** snapshot → `tool_call_update {content: [text(Message)]}`, throttled,
  with `Data` in `_meta`; output → terminal output chunks. Never resend
  accumulated output.
- **MCP:** as a client, send a `progressToken` and forward
  `notifications/progress` into `tool.Report`; if progress extends a timeout,
  keep a hard cap (typescript-sdk#1001). As a server, forward `Progress` as
  notifications, using a counter when `Done` does not increase.
- **Sub-agents:** a tool that runs a child turn can forward the child's events
  as its own progress.

## Proposed API

```go
// package tool

// Progress is a snapshot of a running call. Each report replaces the last.
type Progress struct {
	Message string          // status line for people, e.g. "47/100 files"
	Done    float64         // work finished, in units the tool chooses
	Total   float64         // 0 means unknown
	Data    json.RawMessage // optional structured detail for custom UIs
}

// Report publishes p for the call running under ctx. It never blocks and does
// nothing when no one listens or after Execute returns.
func Report(ctx context.Context, p Progress)

// Output returns a writer for live, appended output (io.Discard when no one
// listens). Writes never block; the returned Outcome remains the result.
func Output(ctx context.Context) io.Writer // phase 2

// WithReporter installs fn for tests and adapters that run executors directly.
func WithReporter(ctx context.Context, fn func(Progress)) context.Context

// package dashi
type Event struct {
	// ...existing fields...
	Progress *ToolProgress // preview; never recorded
}
type ToolProgress struct {
	CallID   string
	Progress tool.Progress
}
```

`Data` is `json.RawMessage`, not Dive's `map[string]any`, because Dashi clones
every event it delivers (runner.go `cloneEvent`) and cannot deep-copy `any`;
Dive's own doc warned of JSON coercion. No `Runner` field is needed: zero
config works, and progress costs nothing without a listener.

The ported example:

```go
process, _ := tool.Func("process_dataset", "Processes a dataset.",
	func(ctx context.Context, call tool.Call, in ProcessInput) (tool.Outcome, error) {
		for i := 1; i <= total; i++ {
			if err := work(ctx, i); err != nil {
				return tool.Outcome{}, err
			}
			tool.Report(ctx, tool.Progress{
				Message: fmt.Sprintf("processed %d/%d records", i, total),
				Done:    float64(i), Total: total,
			})
		}
		out := tool.TextOutput(fmt.Sprintf("Processed %d records.", total))
		return tool.Outcome{State: tool.StateSucceeded, Output: &out}, nil
	})

for ev, err := range runner.Stream(ctx, turn, start) {
	if err != nil { return err }
	if p := ev.Progress; p != nil {
		fmt.Printf("\r  %-32s %3.0f%%", p.Progress.Message,
			100*p.Progress.Done/p.Progress.Total)
	}
}
```

## Rejected alternatives

- **A parameter on `Execute` or a second `ProgressExecutor` interface:** churns
  every executor, or adds the interface split that hurt Eino.
- **Iterator or preliminary-result tools:** progress must look like the result,
  and the result path forks.
- **Recording progress as steps:** write amplification for no recovery gain.
- **Dive's synchronous callback:** races and blocks tools; kept only its API
  shape.
- **One struct mixing snapshot and appended fields:** ambiguous, as Dive found.

## Open questions

1. Ship `Output` (appended text) now or with the first shell tool?
2. Is `Done`/`Total` worth having, or should the snapshot be `Message` + `Data`
   only, as in Dive?
3. Should pi's opt-in _checkpoint_ exist, where the last snapshot is saved and
   becomes the salvage content of an uncertain call after a crash
   (comparison/pi.md:360, 367)? It needs a recorded step kind.
4. Should the runner also emit a Claude-SDK-style heartbeat (elapsed time for
   a running call, e.g. every 30 s) so UIs can show liveness for tools that
   never report, or is stuck-tool detection the host's job?

Research notes cited as `comparison/…` and `acp-agent-adapter.md` live in the
git-ignored `tmp/research/` directory.
