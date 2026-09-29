# 14. Session study: pi

**Status:** research, 2026-09-29. **Part of:** [session
research](14-session-continuity-research.md); conclusions in the
[PRD](14-session-continuity-prd.md).

**Commit:** `earendil-works/pi` at `2b0a123de` (2026-09-26), coding agent
0.87.1. Paths are relative to `packages/`. `H/` = `agent/src/harness/`, `CA/` =
`coding-agent/src/core/`. Issue states were checked on GitHub on 2026-09-29.

## 1. Overview

pi is a TypeScript agent monorepo (`pi-ai`, `pi-agent-core`, `pi-coding-agent`).
It has two session stacks, with a third specified:

- **Shipping stack.** `AgentSession` (`CA/agent-session.ts`, 4,023 lines) is an
  in-memory object over the agent loop, persisted by `SessionManager`
  (`CA/session-manager.ts`) as a JSONL tree. The CLI and SDK use it.
- **Durable harness.** `AgentHarness` → `AgentLane` over `Session` → `Storage`
  (`H/`), with memory, JSONL, and SQLite backends. Only the experimental worker
  uses it (`coding-agent/src/experimental/mini/worker/run.ts`). Its normative
  spec is `agent/docs/harness.md`. This study focuses on it.
- **Pico5** (`durable/docs/pico-v5.md`), the next design. Its storage is
  implemented in `packages/durable`; its harness is a spec. It changes the
  busy-input and idempotency answers (sections 4 and 6).

| Harness type | Owns | Dashi analogue |
|---|---|---|
| `SessionRepo` | create, open, list, delete, fork | none (host storage) |
| `Session` | entry tree, typed values and lists, usage ledger, one mutation line | session plus recorder |
| `Storage` | atomic `commit(writes[])` and scans | recorder |
| `AgentHarness` | tools, hooks, resources, events; lanes | `Runner` |
| `AgentLane` | a named branch tip, model configuration, inbox, at most one operation | none |
| operation (`run`, `compaction`, `navigation`) | one accepted unit of lane work | **turn** |
| pi "turn" (`turn_start`/`turn_end`) | one model call plus its tools | **model attempt** |

A **lane** is a conversation head inside a session; several lanes can share one
tree.

## 2. Session type and lifecycle

`SessionRepo` has five methods. `open` takes the **metadata record** (which
includes a file path), not an ID:

```ts
// H/session/types.ts:592-602
export interface SessionRepo<TMetadata extends SessionMetadata = SessionMetadata, ...> {
	create(options: TCreateOptions, context: Context): Promise<Session<TMetadata>>;
	open(metadata: TMetadata, context: Context): Promise<Session<TMetadata>>;
	list(options: TListOptions | undefined, context: Context): Promise<TMetadata[]>;
	delete(metadata: TMetadata, context: Context): Promise<void>;
	fork(source: TMetadata, options: ForkOptions, context: Context): Promise<Session<TMetadata>>;
}
```

So a host opens a session by listing all of them and searching for the ID
(`coding-agent/src/experimental/mini/worker/run.ts:45-48`). Open never creates.
There is no archive. Metadata is `{id, createdAt, storageVersion, cwd?,
parentSessionId?}` (`H/session/types.ts:473-480`); names, labels, and
application data are typed session values (`H/session/types.ts:530-555`). The
shipping `SessionManager` has static `create`, `open(path)`, `continueRecent`,
`inMemory`, and `forkFrom` (`CA/session-manager.ts:1750-1810`).

## 3. Storage contract and format

A session holds three stores: a write-once **entry tree**, replaceable **values
and lists**, and an append-only **usage ledger**
(`agent/docs/harness.md:41-46`). The tree has four entry kinds, `message |
compaction | branch_summary | custom` (`H/session/types.ts:16`). Branch tips,
lane and operation state, queued input, and stream frames are values. The write
primitive is one atomic commit:

```ts
// H/session/types.ts:455-471 (trimmed)
export interface Storage {
	commit(writes: Write[], context: Context): Promise<CommitResult>;
	getEntries(ids: string[], context: Context): Promise<Map<string, Entry>>;
	// getValue, scanBranch, scanEntries, scanUsage, readList, scanValues, ..., close
}
```

**JSONL** (format 4) writes one file per session
(`H/session/jsonl/repo.ts:36-43`), one committed transaction per line
(`H/session/jsonl/io.ts:76-78`). On open, a torn last line is dropped and the
file rewritten atomically (`H/session/jsonl/storage.ts:108-111`). Reclaiming
dead value lines is specified but not implemented (`agent/docs/harness.md:145`).
**SQLite** keeps many sessions in one database
(`session-backends/sqlite-node/src/sqlite/migrations/001_initial.sql:9-111`);
**memory** is `MemoryStorage` (`H/session/memory.ts:37,334`). All backends run
one conformance suite (`H/session/testing/conformance/`), exported for
third-party backends (`durable/README.md`).

## 4. Identity and concurrency

Every ID is a UUIDv7 minted by the session (`agent/docs/harness.md:204`).
`accept` takes an optional caller `operationId` (`H/runtime/lane.ts:486`). This
is **not an idempotency key**:

> An operation id may be supplied or minted before acceptance; it correlates a
> host submission with `inspectExecution`, `drive`, and the result record but is
> not an unbounded acceptance-idempotency index. (`agent/docs/harness.md:602`)

A retried `accept` on an idle lane starts a second run. Only `drive` with a
settled ID returns the stored result (`H/runtime/lane.ts:953-966`). Pico5 adds a
host `requestId` that "deduplicates within one conversation before any write"
(`durable/docs/pico-v5.md:1486`).

**Concurrency** relies on one writer. Each session has an in-process
`MutationLine` (`H/session/mutation-line.ts:2`), and each lane plans commands
from its in-memory state, not by re-reading storage
(`H/runtime/lane.ts:339-354`). A second writer silently diverges. The spec lists
"multiple writable owners" as a non-goal (`agent/docs/harness.md:120`), and the
SQLite README says there is "no cross-process lease, lock, fence, heartbeat, or
takeover" (`session-backends/sqlite-node/README.md:31`). A second `open` is
refused only within one process (`H/session/jsonl/repo.ts:96-97`). Parallel work
uses **lanes**: "A second thread in the same channel runs its own lane over the
same shared history with no coordination" (`agent/docs/harness.md:89`).

## 5. Turns and the entry point

The engine is four primitives: `accept` (Start), `drive` (Continue),
`requestAbort` (Cancel), and `inspectExecution` (`agent/docs/harness.md:27`).
`OperationRequest` is a union of `prompt | skill | prompt_template | compaction
| navigation`, each with an optional `operationId` (`H/agent-harness.ts:111-117`).
`prompt` is `accept` plus `drive`:

```ts
// H/runtime/lane.ts:1162-1186 (trimmed)
const admission = await this.accept(request, context);
if (!admission.ok) { ... return Result.err(admission.error); }
const driven = await this.drive({ operationId: admission.value.operationId, waitForRetry: true }, context);
if (driven.value.kind === "settled") return Result.ok(driven.value.outcome);
if (driven.value.reason === "deferred") return Result.ok({ operationId, status: "suspended", deferred });
```

`accept` writes the prompt, moves the tip, and records a `starting` state in one
commit, with no hooks or effects (`H/runtime/lane.ts:628-661`). `drive` may
return `waiting` for a retry or a provider-deferred response
(`H/agent-harness.ts:165-168`), so a server can schedule the next `drive` from
an alarm.

**History is never supplied.** Each model attempt walks the branch from its tip
and projects it, starting at the newest compaction entry
(`H/session/context.ts:10-29`). Failed, aborted, and deferred assistant messages
stay in the tree but leave the model's view. Custom entries reach the model only
through registered `entryProjectors` (`H/session/context.ts:59-61`).

## 6. Input while a turn is open

**Harness: refuse.** All three acceptance paths check the lane and return
`LaneBusy`. They never cancel:

```ts
// H/runtime/lane.ts:574-587 (trimmed)
if (state.operation !== null) {
	return { kind: "return", result: Result.err(new LaneBusy({
		lane: this.name,
		operationId: state.operation.meta.operationId,
		operationKind: state.operation.meta.intent.kind, ... })) };
}
```

The error names the open operation, so the caller can steer, queue, or abort it.
Compaction and navigation count as open work.

**Shipping stack: throw unless the caller names a mode.** `prompt()` while
streaming throws "Agent is already processing. Specify streamingBehavior
('steer' or 'followUp')" (`CA/agent-session.ts:1654-1666`).

**Pico5: queue by default.** Submissions take `whenBusy?: "steer" | "followUp" |
"reject"`, defaulting to `followUp` (`durable/docs/pico-v5.md:287, 578-579`).

## 7. Steering and queued input

The inbox belongs to the lane, is durable, and gives every item an ID. Four
kinds (`H/session/types.ts:129`): `steer` (placed at the next safe point,
between model attempts), `followUp` (placed only when the operation would
otherwise finish, extending it), `nextRun` (waits for the next operation), and
`write` (passive; never starts generation). Each mode is `all` or
`one-at-a-time` (`H/runtime/drive/boundary.ts:88-112`).

**Leftovers become input to the next operation.** `acceptRun` drains eligible
items ahead of the new prompt in the same commit (`selectAcceptedInbox`,
`H/runtime/lane.ts:143-167, 588-592, 628-631`). An `accept` with an empty prompt
is legal when the inbox has items. **Cancel** uses IDs:

```ts
// H/agent-harness.ts:97-102
export type QueueResult = Result<{ entryId: string }, InvalidMessage | Closed>;
export type CancelQueuedResult = Result<{ kind: "cancelled" | "already_consumed" | "not_found" }, Closed>;
export type AbortResult = Result<
	{ operationId: string; steer: AgentMessage[]; followUp: AgentMessage[] }, NoActiveOperation | Closed>;
```

Abort hands back queued `steer` and `followUp` items and keeps `nextRun` and
`write` (`H/runtime/lane.ts:1047-1062`). An item ID becomes its entry ID when
placed, which is how `cancelQueued` reports `already_consumed`
(`H/runtime/lane.ts:1524-1530`). The shipping stack keeps queues as in-memory
strings matched by text (`CA/agent-session.ts:347-349, 905`), which caused #9803
and #10017.

## 8. Resume after a crash or interruption

Every durable transition replaces the operation's **complete** state, one of 13
flat phases (`H/session/types.ts:316-329`). Recovery starts from that state,
with no journal replay (`agent/docs/harness.md:52`). Opening reports open work
and starts nothing: `AgentHarness.create` returns `{harness, open[]}`, one entry
per lane with an open operation (`H/runtime/harness.ts:389-404`). The host
drives each (`worker/run.ts:95-104` calls `lane.resume()`). A **model call in
flight** is settled from committed stream frames as an error saying "the
external outcome is unknown"; the provider is not called again
(`H/runtime/drive/recovery.ts:22-40`). A **tool in flight** is re-run only if
both the recorded intent and the tool declare `replay: "safe"`; otherwise it
gets a synthetic interrupted result (`H/runtime/drive/tools.ts:527-538`).

## 9. Approvals and human-in-the-loop

The harness has **no durable approval state**. Approval is a `before_tool` hook
that blocks the drive while it waits (`H/runtime/drive/tools.ts:450-463`),
returning `{ block: { reason } }` or new arguments
(`H/agent-harness.ts:464-467`). Hook results become durable only when a later
transaction consumes them (`agent/docs/harness.md:54`). A crash while waiting
asks again; a day-long approval keeps a process busy. The only durable
suspension is a provider-deferred response (`SuspendedRun`,
`H/agent-harness.ts:78-82`). Shipping extensions block the same way
(`coding-agent/examples/extensions/permission-gate.ts:13-29`).

## 10. Compaction

Compaction is an **operation kind**. Manual compaction goes through `accept`, so
`LaneBusy` applies (`H/runtime/lane.ts:679-760`). Automatic compaction runs
**inside** a run, checked after each tool batch
(`H/runtime/drive/checkpoint.ts:100`), and after a context-overflow response
(`H/runtime/drive/response.ts:189-237`). The record is a tree entry:

```ts
// H/session/types.ts:33-41
export interface CompactionEntry extends EntryBase {
	type: "compaction";
	summary: string;
	retainedTail: AgentMessage[];
	tokensBefore: number;
	// details?, usage?, fromHook
}
```

`retainedTail` copies the kept messages forward, so the entry holds the whole
replacement view. Entries are never deleted (`agent/docs/harness.md:124`). A
`before_compaction` hook can decline or supply its own result
(`H/agent-harness.ts:488-495`). Preparation reuses the projection helpers
(`H/compaction/compaction.ts:668`), but through `sessionEntryToContextMessages`,
which drops `custom` entries the model's view projects, so the summary can miss
context the model saw (#6451, open).

## 11. The agent, settings, and tools per turn

**Lane configuration** `{model, thinkingLevel, activeToolNames}` is durable
(`H/session/types.ts:69-73`); `setModel` and `setActiveTools` commit it at any
time, even mid-operation (`H/runtime/lane.ts:1883-1897`). **Run settings**
(compaction, queue modes, tool execution mode) are captured at `accept`
(`H/runtime/lane.ts:169-176, 643`). Each model attempt snapshots the lane
configuration (`H/runtime/drive/boundary.ts:69`), so a model change reaches the
open operation at its next attempt.

Tool **implementations** are harness code (`H/agent-harness.ts:524`), supplied
at each open; the lane stores only a **selection** of names. The model is stored
as `{provider, modelId}`. A missing model fails that request; a missing tool
becomes a synthetic error result. Neither fails the open
(`agent/docs/harness.md:971`). The system prompt is a per-request callback
(`agent/docs/harness.md:1094`); no agent definition is recorded.

## 12. Forks, rewinds, and child sessions

**Rewind** is a `navigation` operation that moves the tip and can append a
`branch_summary`; nothing is deleted. **Parallel branches**: `harness.lane(name,
{ createAt: entryId })` (`H/runtime/harness.ts:112-127`). **Fork** is a
repository operation with scope `branch` (up to an entry) or `tree`; operation,
pending, result, and usage state are excluded (`H/session/types.ts:562-590`).
The fork gets "fresh idle lane state" and records `parentSessionId`
(`agent/docs/harness.md:532`). **Subagents** are not in the harness; the
shipping example spawns `pi --mode json -p --no-session`
(`coding-agent/examples/extensions/subagent/index.ts:300`).

## 13. Streaming and live updates

Events carry the lane and `runId` (`H/agent-harness.ts:255-373`), and `recovery:
true` during recovery. A reconnecting client calls `lane.watch()`, which
snapshots on the mutation line and buffers events "with no gap and no duplicate"
(`coding-agent/src/experimental/mini/worker/lane-service.ts:4-6, 40-66`). The
snapshot holds transcript, queues, and the in-flight partial message
(`H/agent-harness.ts:228-248`). There is no event replay cursor
(`agent/docs/harness.md:574`).

## 14. The first program and a realistic embedding

```ts
// coding-agent/docs/sdk.md:7-18 (shipping)
const { session } = await createAgentSession();
await session.prompt("What files are in the current directory?");
console.log(session.getLastAssistantText());
```

The durable embedding runs one worker per session
(`coding-agent/src/experimental/mini/worker/run.ts:64-104`): open the session
from a `JsonlSessionRepo`, call `AgentHarness.create` with models, tools, and
system prompt, take lane `"main"`, and `resume()` each reported open operation.
Every call takes a trailing `Context`, like Go's `ctx`.

## 15. Known pain

- **Settlement races** (#5886, open): `agent_end` fired before retries,
  compaction, and follow-ups finished; the harness's `drive` result avoids it.
- **Projection drift** (#6451, open): partly fixed; see section 10.
- **Compaction checked only at run end** (#6879, closed): now per checkpoint.
- **Queues matched by text** (#9803, closed; #10017, open: a follow-up stranded
  on abort). The shipping stack still matches by text.
- **Lazy file creation** (#9792, open): the file appears once a user or
  assistant message exists (`CA/session-manager.ts:1166-1177`), not at
  `create()`.
- **One process per session** (D#1546, #5700): the harness makes it a written
  rule.
- **Two runtimes** (#8845, D#3337): duplicated compaction bugs; SDK users unsure
  which API is stable.

## 16. Lessons for Dashi

Where the design differs, such as the busy-input policy in question 1, see the
PRD's [Alternatives](14-session-continuity-prd.md#alternatives-considered).

**Approach 1.** pi supports the *API shape*: `prompt = accept + drive`, `resume`
inspects then drives, and the primitives stay public for servers that schedule
`drive` themselves. The *storage* model is **Approach 3**: lane state, inbox,
and history live in session storage, and history is never supplied. pi can do
this only because it assumes one writer. Dashi's `ErrRevisionConflict` on turn
*n* is stronger than anything in pi.

**Approach 2.** pi has none; one atomic store avoids the two-records problem.

**Approach 3.** The cost: a 13-phase state machine, typed value addresses, three
backends, a 1,468-line spec, and still a single-writer rule. It buys exact
recovery of queues and in-flight effects, which Approach 1 loses unless queues
are recorded.

**Open questions:**

1. **Busy input.** The harness refuses with a typed error naming the open
   operation; Pico5 defaults to `followUp` and keeps `reject`. Borrow the named
   policy (`steer | followUp | reject`) and return the open turn's ID. Refusal
   suits a server, queuing suits a chat UI. pi never supersedes.
2. **Compaction.** Record the replacement view (summary plus retained tail) on
   the history path, and project from the newest one. Check the threshold
   between model attempts. Use one projection function, including for custom
   records.
3. **Steering.** The session owns the inbox. Borrow the four kinds, item IDs
   that become entry IDs, `cancelQueued` with three results, abort returning
   undelivered items, and leftovers drained into the next `Start` in the same
   commit. Under Approach 1 the inbox needs a durable home; in memory it has the
   shipping stack's bugs.
4. **`Send(text)`.** Keep it as sugar; pi's `prompt(text)` builds the same
   request `accept` takes.
5. **Tools.** Implementations on the runner, selection on the agent; a missing
   tool is a synthetic error result. pi applies a model change at the next
   attempt; Dashi fixes the agent per turn, which is simpler.

**Also borrow:** open reports but never drives; tool replay policy `safe |
never` with a synthetic interrupted result; `drive` on a settled ID returns the
stored result; an exported conformance suite.

**Avoid:** operation IDs that look like idempotency keys but are not (dedupe
`Send` by `CommandID`, as Pico5's `requestId` does); open by metadata or path
instead of ID; approvals that block a process and are asked again after a crash;
queues matched by text; a compaction path that projects differently.
