# 14. Session study: Codex

**Status:** research, 2026-09-29. **Part of:** [session
research](14-session-continuity-research.md); conclusions in the
[PRD](14-session-continuity-prd.md).

**Commit:** `openai/codex` at `0462dcc062` (2026-09-29). Paths are relative to
the repository root; `codex-rs/` is omitted where a crate path is clear.

## 1. Overview

Codex is OpenAI's coding agent: a Rust core, an app server that speaks JSON-RPC
("app-server v2") to the desktop app, IDE, and remote clients, and thin
TypeScript and Python SDKs. Its model is **Thread → Turn → Item**. A *thread* is
Dashi's session; a *turn* is Dashi's turn; an *item* is one user message, agent
message, command run, or file change. Two name clashes: the Rust `Session`
struct is the runtime of *one* thread, and `Thread.session_id` names a *tree* of
threads (a root plus its subagents).

| Layer | Type | Owns |
|---|---|---|
| Process registry | `ThreadManager` | map of loaded threads |
| Handle | `CodexThread` | submit input, read events, interrupt, suspend |
| Runtime | `Session` (+ `ActiveTurn`, `input_queue`) | in-memory history, at most one active turn |
| Persistence | `ThreadStore` → `LocalThreadStore` | JSONL "rollout" per thread, SQLite index, writer lock |
| Protocol | app-server v2 | `thread/*`, `turn/*`, `item/*` |
| SDKs | TS `Thread.run`, Python `thread.turn()` | wrappers over `codex exec` or the app server |

History is always derived from the thread's own record. No caller supplies it.

## 2. Session type and lifecycle

The protocol `Thread` carries identity, lineage (`session_id`, `forked_from_id`,
`parent_thread_id`), `history_mode` (`Legacy | Paginated`), and status; turns
are filled only on resume, fork, or read
(`app-server-protocol/src/protocol/v2/thread_data.rs:216`). Lifecycle methods
(`common.rs:551-864`) include `thread/start`, `resume`, `fork`,
`archive`/`unarchive`, `delete`, `list`, `search`, `read`, `turns/list`,
`items/list`, `name/set`, `metadata/update`, `revert`, `compact/start`,
`inject_items`, `settings/update`, and `queue/*`. `ThreadStatus` is `NotLoaded
| Idle | SystemError | Active{activeFlags: [WaitingOnApproval,
WaitingOnUserInput]}` (`v2/thread.rs:1649-1667`).

**Open by ID.** `thread/resume{threadId}` loads a stored thread, or joins it if
loaded; a `path` is only a consistency check (`thread_processor.rs:4320-4334`).
Overrides sent to a loaded thread that others watch are logged and ignored
(`:4380-4386`). Resume never creates. **Unload**: an idle loaded thread with no
subscribers unloads after `thread_unload_delay`, default 60 seconds
(`core/src/config/mod.rs:3912`;
`app-server/src/request_processors/thread_lifecycle.rs:56-63`).

## 3. Storage contract and format

`ThreadStore` is the boundary. About a dozen methods are required; the rest
default to `Unsupported`, so capabilities are optional:

```rust
// codex-rs/thread-store/src/store.rs:93 (trimmed)
pub trait ThreadStore: Any + Send + Sync {
    fn create_thread(&self, params: CreateThreadParams) -> ThreadStoreFuture<'_, ()>;
    fn resume_thread(&self, params: ResumeThreadParams) -> ThreadStoreFuture<'_, ()>;
    fn append_items(&self, params: AppendThreadItemsParams) -> ThreadStoreFuture<'_, ()>;
    fn load_history(&self, params: LoadThreadHistoryParams) -> ...;
    fn load_latest_model_context(&self, ...) -> ... { /* default: Unsupported */ }
    fn prepare_fork(&self, ...) -> ... { /* default: Unsupported */ }
    fn revert_thread(&self, ...) -> ... { /* default: Unsupported */ }
```

`PersistContext` (`store.rs:66-90`) says *why* a write happens (`TurnStart`,
`SteeredUserInput`, `SubagentSpawn`, …). Implementations: `LocalThreadStore` and
`InMemoryThreadStore` (`thread-store/src/in_memory.rs:894`).

**On disk**, one append-only JSONL *rollout* per thread
(`rollout/src/recorder.rs:82-104`). Each line is a `RolloutLine{timestamp,
ordinal?, item}` (`history/src/lib.rs:350-355`), where `RolloutItem`
(`history/src/lib.rs:201`) includes `SessionMeta`, `ResponseItem` (model-visible
history), `Compacted`, `TurnContext` (settings a turn ran with),
`TokenUsageRecord`, and `EventMsg` (`TurnStarted`, `TurnComplete`,
`TurnAborted`, …). Beside the rollouts sit SQLite files for the thread index,
the queue, and a paged projection of turns and items
(`state/src/sqlite.rs:28-33`). In the newer *paginated* mode, lines carry
ordinals, forks reference parent segments instead of copying, and the model
context loads by a reverse scan to the newest compaction.

## 4. Identity and concurrency

Thread and turn IDs are server-generated UUIDv7 (`thread_data.rs:205`;
`core/src/session/mod.rs:1077-1079`). `clientUserMessageId` on `turn/start` is
copied onto the item for correlation, with no deduplication found. **A retried
`turn/start` is not idempotent**: it starts a second turn, or steers the running
one.

Concurrency has three layers: requests are serialized per thread in the app
server (`app-server/src/request_serialization.rs:24-51`); `active_turn` is a
mutex under which start and steer decide (§6); and across processes an OS file
lock per thread guards the writer:

```rust
// codex-rs/rollout/src/writer_lock.rs:42 (trimmed)
    /// Acquires exclusive writer ownership, returning `WouldBlock` for an active writer.
    pub fn acquire(self: &Arc<Self>, thread_id: ThreadId) -> io::Result<WriterLockGuard> {
        let path = self.directory.join(format!("{thread_id}.lock"));
        match file.try_lock() {
            Err(std::fs::TryLockError::WouldBlock) => { /* "already has an active writer" */ }
```

`create_thread` and `resume_thread` take it
(`thread-store/src/local/live_writer.rs:33,46`) and map `WouldBlock` to
`ThreadStoreError::Conflict` (`local/mod.rs:338-352`). It is a lease held while
a thread is loaded, on one machine's disk. It does not help two Cloud Run
workers.

## 5. Turns and the entry point

`CodexThread` exposes four input modes, each returning a typed result:

```rust
// codex-rs/core/src/codex_thread.rs:366
    pub async fn start_or_steer_turn(&self, request: TurnInputRequest)
        -> CodexResult<TurnInputSubmission>            // Started | Steered | NotSubmitted
    pub async fn start_turn_if_idle(&self, request: TurnInputRequest)
        -> CodexResult<StartIfIdleSubmission>          // Started | NotSubmitted
    pub async fn continue_turn_if_idle(&self, request, expected_previous_turn_id: String) ...
    pub async fn steer_turn(&self, request, expected_turn_id: String)
        -> CodexResult<SteerSubmission>                // Steered | NotSubmitted
```

`NotSubmittedReason` names every refusal: `NotIdle`, `NoActiveTurn`,
`ExpectedTurnMismatch{expected, actual}`, `ActiveTurnNotSteerable{turn_kind}`,
`ActiveTurnOutputSchemaMismatch`, `Superseded`, `ServerDraining`,
`PendingTriggerTurn`, `PlanMode`, `EmptyInput`
(`protocol/src/turn_input.rs:220-252`). Events arrive on one stream per thread
(`codex_thread.rs:694`). The session keeps an in-memory `ContextManager`
(`core/src/context_manager/history.rs:91`) that renders each prompt; it is
rebuilt from the rollout only when a thread loads, so steady-state turns never
reread storage.

## 6. Input while a turn is open

The protocol's plain `turn/start` **steers** a running turn: the app server
calls `start_or_steer_turn`
(`app-server/src/request_processors/turn_processor.rs:651-684`), and core tries
to steer first, starting a turn only on `NoActiveTurn`
(`core/src/session/turn_input.rs:304`). On the wire, both outcomes return
`TurnStartResponse{turn: {id, status: InProgress}}`; the client learns it
steered only by noticing the running turn's ID. The guard inside `steer_input`
refuses review and compaction turns and a different `outputSchema`:

```rust
// codex-rs/core/src/session/turn_input.rs:641 (trimmed)
        let mut active = self.active_turn.lock().await;
        let Some(active_turn) = active.as_mut() else { return Err(NotSubmittedReason::NoActiveTurn); };
        if let Some(expected_turn_id) = expected_turn_id && expected_turn_id != active_turn_id {
            return Err(NotSubmittedReason::ExpectedTurnMismatch { ... });
        }
        match active_task.kind {
            TaskKind::Regular => {}
            TaskKind::Review => return Err(ActiveTurnNotSteerable { turn_kind: Review }),
            TaskKind::Compact => return Err(ActiveTurnNotSteerable { turn_kind: Compact }),
        }
```

Refusal exists off `turn/start`: `start_turn_if_idle` returns `NotIdle`
(`turn_input.rs:431-437`), used by the queue (§7) and by embedders.
`turn/interrupt{threadId, turnId}` checks the ID and answers only once
`TurnAborted` arrives (`turn_processor.rs:1612-1626`).

## 7. Steering and queued input

**Steer.** `turn/steer{threadId, input, expectedTurnId}` (`v2/turn.rs:293-315`)
puts input in the active turn's pending queue, drained before each model
request; the turn keeps looping while input is pending (`session/turn.rs:566`).
A steer that lands as the turn finishes is recorded, not dropped
(`core/src/tasks/mod.rs:671-697`). On **interrupt, undelivered steers are
discarded** without being returned
(`self.input_queue.clear_pending(&active_turn)`, `core/src/tasks/mod.rs:562`).

**Queue.** `thread/queue/add|list|update|delete|reorder|start`
(`common.rs:624-658`) store messages durably in SQLite, each with an ID. On the
idle event the queue extension starts the head item, only if idle, and never
after an interrupt:

```rust
// codex-rs/ext/queue/src/service.rs:439 (trimmed)
            match thread.start_turn_if_idle(TurnInputRequest::new(input).on_start(TurnStartOptions {
                    turn_trigger: Some("queue".to_string()), ..Default::default() })).await
            {
                Ok(StartIfIdleSubmission::Started { .. }) => {
                    self.delete_locked(thread_id, queued_item_id).await?;
                    return Ok(());
                }
                Ok(StartIfIdleSubmission::NotSubmitted { reason }) => { /* leave queued */ }
```

`on_thread_idle` returns early on `ThreadIdleCause::Interrupted`
(`service.rs:549-553`). A watcher polls SQLite every 10 seconds, so another
process can dispatch too (`service.rs:89-92`). Subagent mail uses the same
split: `QueueOnly` versus `TriggerTurn` (`core/src/agent/types.rs:57-62`).

## 8. Resume after a crash or interruption

**Rebuild.** On load, `reconstruct_history_from_rollout` scans newest-first,
stops at the newest `Compacted` item holding a replacement history, skips turns
dropped by old rollback markers, and restores the previous turn's settings from
`TurnContext` (`core/src/session/rollout_reconstruction.rs:230`).

**Reporting.** A crash leaves a `TurnStarted` with no end; views show it as
`Interrupted` whenever the thread is not active
(`thread_processor.rs:5811-5825`). Nothing drives it.

**Handoff and restart.** `suspend_turn_and_shutdown` stops a root turn *without*
writing `TurnAborted`, so another worker can continue it under the same turn ID
with `recover_turn_if_idle` (`codex_thread.rs:415-470`). After a managed daemon
restart, a saved `InterruptedTurn`
(`app-server-transport/src/daemon_recovery.rs:19-27`) leads to a guarded
continuation:

```rust
// codex-rs/app-server/src/request_processors/daemon_continuation.rs:87 (trimmed)
            "The server restarted and interrupted the previous turn. Continue the unfinished \
             work from the saved conversation. Check the current state before repeating \
             actions that may already have completed."));
        // Close the old turn in persisted history before exposing a new running turn.
        thread.append_rollout_items(&[RolloutItem::EventMsg(EventMsg::TurnAborted(...))]).await
        match thread.continue_turn_if_idle(request, saved.turn_id).await {
```

It runs only if the saved turn is still last, has not ended, and the permission
profile and environment still match (`:42-86`).

## 9. Approvals and human-in-the-loop

Approvals are **server-to-client requests** (`item/commandExecution/`,
`item/fileChange/`, `item/permissions/requestApproval`,
`item/tool/requestUserInput`, and `item/tool/call` for host tools;
`common.rs:1765-1796`). Decisions are `Accept | AcceptForSession |
AcceptWithExecpolicyAmendment | ApplyNetworkPolicyAmendment | Decline | Cancel`;
`Decline` continues the turn and `Cancel` interrupts it (`v2/item.rs:66-85`).
Inside core the turn **blocks on an in-memory oneshot** stored in the active
turn's state (`core/src/session/mod.rs:2819`). A client that reconnects while
the thread is loaded gets pending requests replayed
(`app-server/src/outgoing_message.rs:446-451`). A process exit loses the waiting
approval, and the turn reads as `Interrupted`. An optional automatic reviewer
("guardian") can answer instead of a person.

## 10. Compaction

**Trigger.** Automatic inside the turn loop: if follow-up work remains and the
token limit is reached, `run_auto_compact(..., CompactionPhase::MidTurn)` runs
(`session/turn.rs:601-625`), plus pre-turn checks (`:1310`).
`thread/compact/start` runs a `Compact` task, which cannot be steered.

**Record.** `Compacted` stores the **whole replacement history** and what resume
needs:

```rust
// codex-rs/core/src/session/mod.rs:4168 (trimmed)
            CompactedItem {
                message: metadata.message,
                replacement_history: Some(replacement_history),
                retained_context: Some(state.history.retained_context().clone()),
                window_number: Some(metadata.window_number),
                resume_metadata: Some(CompactionResumeMetadata {
                    last_started_turn_id: state.last_started_turn_id.clone(),
                    previous_turn_settings: state.previous_turn_settings(), .. }),
            }
```

It is appended with a fresh `TurnContext` (`:4191-4203`). **Rebuild:**
`load_latest_model_context` reads backwards to the newest compaction with a
replacement history and window number
(`thread-store/src/local/model_context.rs:27-34`), so opening a long thread
costs the last window, not the whole log. Legacy compactions without
`replacement_history` force a full replay (`rollout_reconstruction.rs:438-446`).

## 11. The agent, settings, and tools per turn

There is no agent value. `turn/start` overrides are **sticky** ("for this turn
and subsequent turns"): model, effort, cwd, approval policy, sandbox or
permissions, collaboration mode (`v2/turn.rs:204-243`). **Per turn only**:
`outputSchema`, `serviceTierForTurn`, and the experimental
`turn/settings/update` (`turn.rs:41-48`). A steer applies sticky settings to
*later* turns (`turn_input.rs:193-202`). Each turn records a `TurnContext`.
Host-implemented *dynamic tools* are fixed at `thread/start`
(`v2/thread.rs:145-151`), recorded in `SessionMeta`, and called through
`item/tool/call`. Compaction records the compaction model's hash
(`mod.rs:4124-4134`).

## 12. Forks, rewinds, and child sessions

**Fork.** `thread/fork{threadId, lastTurnId | beforeTurnId}` creates a thread
with `forkedFromId`; the cut turn cannot be in progress
(`v2/thread.rs:544-560`). Paginated forks reference the source's segments up to
an ordinal (`rollout/src/recorder.rs:106`).

**Rewind.** `thread/rollback` is **removed** (`app-server/README.md:336-345`).
`thread/revert{threadId, beforeTurnId}` works for paginated threads only, "does
not revert local file changes" (`v2/thread.rs:1257-1267`), and writes no marker:
it writes a new immutable rollout referencing the retained prefix and swaps the
SQLite path pointer (`thread-store/src/local/revert_thread.rs:15`).

**Children.** A subagent is a thread with source `ThreadSpawn{parent_thread_id,
depth, agent_path, agent_nickname, agent_role}`
(`protocol/src/protocol.rs:2908-2920`). The model gets `spawn_agent`,
`send_message`, `followup_task`, `interrupt_agent`, `list_agents`, and
`wait_agent` (`core/src/tools/spec_plan.rs:694-724`). Clients may not send v2
children direct input
(`app-server/src/request_processors/thread_input.rs:12-21`), and
`suspend_turn_and_shutdown` refuses while live descendants exist.

## 13. Streaming and live updates

Notifications carrying thread and turn IDs go to every subscribed connection:
`thread/started`, `thread/status/changed`, `turn/started`, `turn/completed`,
`item/*`, deltas, `thread/compacted`, `thread/queue/changed`
(`common.rs:1919-1994`). There is **no sequence cursor for live events**; a
dropped client reconnects with `thread/resume`. Durable history pages through
`thread/turns/list{cursor, sortDirection, itemsView}`, with a `backwardsCursor`
for an anchor turn that may still change (`v2/thread.rs:1705-1735`).
Full-history resume of paginated threads is deprecated
(`thread_processor.rs:4390-4396`).

## 14. The first program and a realistic embedding

The TypeScript SDK spawns `codex exec --experimental-json` per `run()`, adding
`resume <id>` after the first turn (`sdk/typescript/src/exec.ts:92,167`;
`thread.ts:118`). `codex.startThread()` then `thread.run("…")` is the hello
world; `codex.resumeThread(id)` reopens (`sdk/typescript/src/codex.ts:25-38`).
The thread ID is null until the first turn starts. The Python SDK returns a turn
handle with `steer`, `interrupt`, and `stream`
(`sdk/python/examples/14_turn_controls/async.py`).

The Rust embedding sample chooses **refuse**:

```rust
// codex-rs/thread-manager-sample/src/main.rs:406 (trimmed)
    let submission = thread.start_turn_if_idle(TurnInputRequest::user_input(...)).await?;
    if let StartIfIdleSubmission::NotSubmitted { reason } = submission {
        bail!("turn input was not submitted: {reason:?}");
    }
    loop {
        match thread.next_event().await?.msg {
            EventMsg::TurnComplete(_) => return Ok(()),
            EventMsg::TurnAborted(_) => bail!("turn aborted"),
```

## 15. Known pain

From `gh search issues --repo openai/codex`, open at this commit:

- **Two stores drift.** Paginated rollouts reuse an ordinal, and the SQLite
  projection stops there for good while the JSONL grows (#41566, #41079, #43142,
  #35746, #40342). The same two-record hazard as Approach 2.
- **Queue and steer confusion.** Queued messages vanish or become steers
  (#43819), edits are lost when another server dispatches (#46911), and queued
  messages are not sent (#49081).
- **Resume cost.** `thread/resume` is effectively quadratic on large active
  threads and blocks remote steering (#38787).
- **Lost turn state.** UI stuck in "Thinking", Stop fails, turn invisible after
  restart (#24287).
- **Wrong target.** The agent answers an earlier message (#8648), a plausible
  symptom of interleaved steers.

## 16. Lessons for Dashi

**Approach 1.** Codex has the same split: `CodexThread` is a thin handle over a
cache rebuilt from the record. Borrow the **typed submission result** (`Started
| Steered | NotSubmitted{reason}`) so a session never hides which path it took.
Codex's single live owner relies on a local file lock; Dashi's
`ErrRevisionConflict` on turn *n* is the right cross-worker guard.

**Approach 2.** The paginated projection bugs (#41566 and relatives) are the
two-records failure: more evidence against a second record the caller keeps
consistent.

**Approach 3.** Codex pays for owning the log: a 30-plus-method store trait,
JSONL plus three SQLite files, a legacy-to-paginated migration, and resume-cost
bugs. Its best idea transfers anyway: store the **replacement view** in the
compaction record so reopening reads back to the last window.

**Open questions.**

1. *Busy session.* Codex offers every policy but defaults `turn/start` to steer,
   while its own sample uses start-if-idle. Keep Dashi's default refusal. If
   steering is offered, require an expected turn ID and report
   `Steered{turnID}`.
2. *Compaction home.* A record with the whole replacement view and resume
   metadata supports option (b). Keep the "previous turn settings" snapshot.
3. *Steering.* Steer (turn-scoped, ID-guarded) and queue (durable, ID'd,
   dispatched only when idle, never after an interrupt) are separate. Return
   undelivered steers on cancel rather than dropping them.
4. *`Send(text)`.* Every SDK leads with `thread.run("…")`. The Python turn
   handle is a good model for the streaming form.
5. *Tools per agent.* Codex fixes host tools at thread start. Borrow per-field
   "sticky or this-turn-only" documentation and the steer-time `outputSchema`
   check.

**Also borrow:** worker handoff that keeps the turn ID; the restart
continuation's guards and its model note ("check the current state before
repeating actions"). **Avoid:** server-only turn IDs with no idempotency key;
in-memory approval waits; live events without a replay cursor.
