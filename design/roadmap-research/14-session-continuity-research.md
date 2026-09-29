# 14. Sessions — comparative research

**Status:** research, 2026-09-29; replaces the 2026-09-28 draft. **Roadmap:**
[item 14](../remaining-work.md). **Question:** How should Dashi model a session,
a conversation of many turns, over its turn engine? **Conclusions:**
[PRD](14-session-continuity-prd.md). **Deep studies:**
[pi](14-session-study-pi.md), [Codex](14-session-study-codex.md).

## Dashi baseline

[Decision 19](../decisions.md) is the current contract. `Start.History` and each
`Continue.History` supply the earlier conversation. The turn stores its own
input and a history fingerprint, never the prior messages (at `015b0ec`:
`turn.go:248-256,428-461`; `runner.go:333-362`). `Continue` warns on a mismatch
by default, or rejects before a commit with `HistoryMismatchReject`
(`runner.go:389-445`). `Project(Turn)` returns only this turn's messages
(`project.go:10-27`). A recorder's expected sequence and owner protect one turn,
not the order of turns (`recorder.go:178-230`). The host persists history,
serializes writers, and passes the same prefix on recovery. [History
storage](../history-storage.md)'s copied-history analysis is superseded; its
Nvoken and Mobius evidence still holds.

Three shapes were weighed. **Approach 1**: a `Session` object over the unchanged
engine, storing nothing of its own; turn IDs are `<session>/<n>`. **Approach
2**: a serializable session value that the runner takes and returns, beside the
turn records. **Approach 3**: sessions in the engine; the recorder stores
sessions and builds history, reversing decision 19. The 2026-09-28 draft took a
fourth, smaller direction: an optional opaque `HistoryRef` on `Start` and
`Continue`, resolved by a host `HistoryResolver`, so a host could name an
immutable prefix instead of passing the slice. It was considered; it eases the
argument but leaves every host to write the session layer.

The design chose Approach 1 with turn IDs `S-n`, the CLI's separator, and finds
the last turn by doubling then binary search, about 2·log2(n) reads
([sessions.md](../sessions.md)). The `<session>/<n>` IDs and the per-turn
probing cost below are the evidence as gathered, not the design.

## Sources and pins

Go: ADK-Go `12f7cabc`; Eino `main` `ba04fde8` and v0.10 alpha
(`origin/alpha/10`) `58daeda8`; MAF-Go `425452c`. Other: pi `2b0a123de`;
OpenHands SDK and agent server `d77ada7`, Canvas `47a10808d`; Claude Agent SDK
(Python) `7f74cdf`; Codex `0462dcc062` (survey notes at `9558d830f6`); opencode
`361a96267`. OpenAI Agents SDK, LangGraph, Pydantic AI, and Vercel AI SDK from
public docs and issue trackers, retrieved 2026-09-29. Ours: ACP docs `128845f`
(inject RFD `7b7c86c`), Dive `65c393f`, Nvoken `deea133a` (dirty tree; cited
paths checked against `HEAD`), Mobius `de93a9e6f`. Issue numbers without a
prefix belong to the project named beside them. This is source and documentation
inspection, not runtime qualification; cross-process guarantees are claimed only
where a lock, lease, index, or version predicate was read in code.

## Verdict

Nearly every source supports **Approach 1's API shape**: two levels over an
unchanged engine. pi's harness has `accept` and `drive`, and `prompt()` is both;
OpenHands has `send_message` plus `run`. Both store history in the session,
which is Approach 3's storage, and pi's holds up only because it assumes one
writer. The other approaches show their costs:

- **Approach 2** means two records kept in step. OpenAI's history log and
  paused-run blob drift apart (py#4827, py#4775, js#701); OpenHands' event log
  and branch head diverge after a crash (sdk#4487); Codex's rollout and its
  SQLite page projection drift (#41566). MAF's session has no ID, so every host
  rebuilds reopen-by-ID.
- **Approach 3** works but is heavy. Eino v0.10 took 2,000 lines and still keeps
  a separate interrupt checkpoint. LangGraph stores a full snapshot per step,
  with unbounded growth. ADK-Go infers turns by scanning the log, the source of
  many bugs. Production Nvoken ignored Dive's whole `session` package, which
  copies history into a mutable session (`dive/dive.go:29-43`), and built its
  own in Postgres: the strongest warning against a session that owns storage.

**Decision 19's rationale needs restating.** It says no compared library copies
history into each run, but Eino v0.10, ADK-Go, LangGraph, OpenHands, and Codex
all rebuild it from their own durable log. Its other grounds (storage growth,
hosts that own their history) stand.

## At a glance

| | Session owner | Turn noun | Store contract | New input mid-turn | Compaction record |
|---|---|---|---|---|---|
| ADK-Go | Session service | Implicit (invocation ID) | 5 methods, timestamp checks | Nothing stops it | Event in log |
| Eino v0.10 | Runner config | Yes (`TurnID`) | `LoadEvents`, `AppendEvents` | Runs anyway; TurnLoop asks | `MessagesReplaced` event |
| MAF-Go | Caller (JSON bag, no ID) | No | `HistoryProvider` | Not addressed | Flags in the bag |
| pi harness | `Session` + `Storage` | Operation | Atomic `commit(writes)` | Refuse (`LaneBusy`) | Entry in tree |
| OpenHands | `Conversation` | No | `FileStore`, event log | Append, then run again | `Condensation` event |
| Claude SDK | CLI process | Query + result | `append`, `load` | Undefined | Transcript entry |
| Codex | Live thread | Yes | `ThreadStore` trait, JSONL rollout + index | Start steers; refuse, guarded steer, queue | Full replacement history |
| opencode | Storage | No | JSON files | Queue and join | Summary message pair |
| OpenAI Agents | Caller | No | 4 item methods | Interleaves | Session decorator |
| LangGraph | Checkpointer | Run | Checkpoint saver | 4 strategies (Platform) | Graph state |
| Dive | `session.Session` | Yes | Store + claims + revisions | Refuse or supersede | Barrier event |
| Nvoken | Conversation (optional) | Yes | Postgres | `reject` / `supersede` / `interrupt` | Advancing cut + summary |

## Our own hosts

- **Nvoken** owns one canonical, ordered transcript per Conversation. A turn's
  `through_message_sequence` names its prefix, and compaction changes only the
  model's view (`docs/guides/session-history.md:1-42`;
  `internal/adapters/postgres/queries/conversations.sql:180-215`). The
  Conversation is optional and does not own agent behavior
  (`docs/guides/sessions.md:6-13`). One nonterminal turn per stream is a unique
  index (`internal/adapters/postgres/schema.sql:2166`). Admission is idempotent
  on a key plus a request fingerprint. History drops canceled turns' messages
  when read, without rewriting them (`schema.sql:190-198`).
- **Mobius** appends sequenced transcript rows with stable message IDs and an
  SQL version predicate, and repairs legacy tool-result replay before Dive
  (`internal/services/session.go:1210-1330`;
  `internal/adapters/sqlite/session_repo.go:502-643`); checkpoints and leases
  handle recovery (`internal/agentruntime/executors/agent_resume.go:27-85`).
  That much repair is application policy, not a thin wrapper.



## By design question

### 1. Input while a turn is open

**Most chat peers accept input while busy.** Codex's `turn/start` steers, pi's
next design (Pico5) queues, LangGraph Platform enqueues, OpenHands appends and
runs again (`event_service.py:745-790`; only its separate `/run` returns 409),
and opencode queues and joins. ACP's inject RFD lists Cursor, Windsurf, Claude
Code, Replit, and Devin too. **Refusal is the choice of our own systems** (Dive,
Nvoken, our ACP adapter design) and of pi's current harness. Codex's embedding
sample also refuses; its caller owns the loop. Hidden choices confuse callers:
opencode gives every waiter the same reply, the Claude SDK defers to its CLI,
and ADK-Go and OpenAI let runs interleave. `cmd/dashi` steers typed text into a
running turn (`control.go:24,247-251`) and cancels a stopped open turn on new
input (`control.go:197-211`).

The best models name the policy:

- **Nvoken**: `if_active` is `reject` (default,
  `internal/services/public_turns.go:1047-1055`), `supersede` (cancel the open
  turn and exclude its context), or `interrupt` (keep completed work, swap at a
  safe point, otherwise record a durable request). Browsers may never supersede.
- **LangGraph Platform**: `reject`, `enqueue` (default), `interrupt`, or
  `rollback` (discard the open run and its input).
- **Codex**: separate calls. `turn/start` steers silently; `start_turn_if_idle`
  refuses; `turn/steer` takes an `expected_turn_id`; a durable queue feeds the
  next turn. Interrupting drops undelivered steers.
- **pi**: the harness refuses with a typed error naming the open turn; Pico5
  defaults to queuing (`whenBusy: steer | followUp | reject`).
- **Eino TurnLoop**: the host's callback returns `Resume` or `StartNewTurn`.

LangGraph settled a closed turn's open tool calls with a fake success result
(deepagents#5547).

### 2. Where compaction lives

**Everyone records compaction and keeps the full history; only the model's view
changes.** Nobody uses a side store that must be synced. Three homes need no new
store under Approach 1:

- **(a) A reference on the next `Start`** to the record the history was built
  from (ADK-Go, Eino, OpenAI's view callbacks).
- **(b) A compaction record in the next turn slot**, `<session>/<n>`, holding
  the summary and the cut (Dive's barrier event, Nvoken's cut, ACP's compaction
  RFD, `rfds/session-compaction.mdx:37-110`). It maps one to one to ACP's
  `compaction_update` on replay.
- **(c) The whole replacement view** in the record, not just a summary. Codex
  stores `replacement_history` (`core/src/session/mod.rs:4168`), Eino v0.10
  `MessagesReplaced`, and pi copies `retainedTail` forward.

Why store the view: it is often not a contiguous tail (Codex keeps recent user
messages within a budget, drops media, and reinjects initial context,
`core/src/compact.rs:649-720`; Dashi's `compaction` package keeps covered system
and developer messages). Provider compaction can return opaque items (Codex
`compact_remote_v2.rs`). A stored view is frozen against projection code
changes, and retention can delete everything before it. pi and Codex run
compaction as its own operation, and both also compact **inside** a turn (Codex
`session/turn.rs:601-625`; pi per checkpoint, which fixed #6879), since one
coding turn can overflow the window.

ADK-Go honors compaction records only when compaction is enabled, so history
depends on the reader's configuration. ADK-Go and Nvoken skip compaction after a
failed or canceled turn and keep the previous view when a summary fails. pi's
maintainers traced a bug cluster to having several projection functions (#6451).

### 3. Steering and queued input

Peers split input into two explicit modes. **Steer** is delivered into the open
turn and expires with it (Nvoken's turn-scoped nudges,
`queries/nudges.sql:72-81`; Codex's `turn/steer`). **Queue** becomes the next
turn's input (pi's `followUp` and `nextRun`, MAF's carried-over injection queue,
ACP's draft `session/inject {mode: queue|steer}`). `cmd/dashi` feeds leftover
steers into the next turn, which is the queue mode by accident.

Every peer queue **starts the next turn when the session goes idle**: Codex's
queue extension calls `start_turn_if_idle` and polls every 10 seconds
(`ext/queue/src/service.rs:89-92,439-460`); Pico5 starts a successor at the
final boundary; LangGraph runs enqueued input after the current run; ACP's
`queue` delivers on idle; OpenHands re-runs on `_rerun_requested`.

Items need IDs. pi's shipping stack matched by text (#9803, #10017). Its harness
gives each item an ID that becomes its entry ID and drains the inbox in the
`accept` commit, so `cancelQueued` can answer `cancelled`, `already_consumed`,
or `not_found`. Nvoken serializes cancel and drain on the row. Codex starts the
turn, then deletes the item, and its open bugs sit in that gap (#46911, #43819).
A steer that misses its turn needs a durable status: Nvoken settles it
`expired`; ACP's revoke returns `already_delivered`.

### 4. Storage, identity, and concurrency

- **Small contracts, optional capabilities, one conformance suite.** The Claude
  SDK requires only `append` and `load`; Eino's store has two methods; Codex's
  `ThreadStore` defaults optional methods to `Unsupported`. ADK-Go, pi, and the
  Claude SDK run every backend through one suite; ADK-Go's backends that skipped
  it diverged (#679, #1329).
- **Concurrency from the store, not locks.** Eino, MAF, pi, and opencode lock
  within one process; Codex's file lock covers one machine; both fail on Cloud
  Run. ADK-Go compares timestamps and fails under clock skew (#1229). Dive
  needed a lock, then TTL claims, then revisions (`session/claims.go:50-60`;
  `dive.go:101-112`). Nvoken's unique index and Dashi's `ErrRevisionConflict` on
  turn *n* hold up.
- **Liveness.** A record cannot tell a running turn from a crashed one. Codex
  (`ThreadStatus`), Eino (`SessionBusyError{ExpiresAt}`,
  `adk/runner.go:280-315`), and Nvoken (leases) keep a liveness signal;
  `cmd/dashi` uses a process lock (`engine.go:87,164`).
- **Retried sends.** Under `<session>/<n>`, a retried start after a lost
  response finds turn *n* committed and starts *n+1*. pi's `operationId` and
  Codex's `turn/start` do not prevent it. Pico5 checks a host `requestId` across
  the whole conversation (`pico-v5.md:1486,2216`); Nvoken uses an admission key
  plus a fingerprint. The usual retry arrives while the original still runs.
  (py#4775 is a lost storage acknowledgement, which Dashi's recorder handles,
  not a retried send.)
- **Reopen versus create.** Dive's create-if-missing `Open`
  (`session.go:1112-1127`) hid typos. LangGraph's `if_not_exists` makes creation
  explicit.
- **Listing versus probing.** Probing turns 1, 2, … breaks after retention
  deletes turns and costs a load per turn. OpenHands failed a whole open on one
  unreadable record (sdk#4080); `cmd/dashi` skips it. Codex, pi, and ACP also
  offer list and delete.
- **Turn IDs `<session>/<n>`** fit ACP (message IDs `m_<turn>_<i>` stay stable
  across replay, `docs/rfds/v2/prompt.mdx:153-161`) and Nvoken (a column).
  `cmd/dashi` uses `-` (`cmd/dashi/session/store.go:43-45`).
- **Metadata.** ACP's `session/list` needs cwd, title, and update time, so an
  adapter needs a small metadata store either way.

### 5. The agent and its tools

**The agent is chosen per turn** wherever turns exist: Eino builds it per turn,
Nvoken's Conversation does not own behavior, and Codex and ACP change model or
mode between turns. Where the agent or session sits on the runner, hosts build a
runner per request (ADK-Go's server and approval example; Eino v0.10,
`adk/session_test.go:622-640`); `cmd/dashi` has the same problem.

**Tool selection** belongs to the agent (Claude SDK `AgentDefinition.tools`,
OpenAI, Pydantic, opencode per message, pi `activeToolNames`);
**implementations** stay with the runtime. OpenHands persists the agent with the
conversation and allows adding tools on resume but not removing them. pi turns a
missing tool into a synthetic error result, not a failed open.

Carry-forward rules differ. Codex's overrides are sticky per setting, except
`output_schema`, which applies to one turn; pi changes model and tools
separately; OpenAI makes callers pass `last_agent` by hand.

### 6. Resume, approvals, and "done"

- **Open reports; it never drives.** pi's `create` returns `{harness, open[]}`.
  Codex labels a crashed turn interrupted; only after a managed restart does it
  start a new turn that tells the model to check what already happened.
- **One call to approve and continue** is popular (OpenAI's `state.approve` then
  `run`).
- **Distinct stopped states.** OpenHands' "unmatched action" conflates approval,
  crash, and interrupt, so its SDK reruns tools on resume.
- **Done is not settled.** pi hosts raced for a year because `agent_end` arrived
  before retries, compaction, and queued follow-ups finished (#5886).
- **Approvals bound to the recorded request** (MAF; Dashi's `Pending` builders),
  so a client cannot forge one or change its arguments.

### 7. Forks, rewinds, and child sessions

A fork is a new session whose first record names the parent session and cut turn
(Dive, ACP's fork RFD, `rfds/session-fork.mdx:28-47`). Dive copies an open turn
as closed, since its owner still owns the pending calls. Eino's
`RollbackSession` rewinds with a recorded marker and an expected-head check;
Codex removed its rollback, which did not undo file changes. Child sessions
carry a parent link (Codex `parent_thread_id`, opencode `parentID`) and can be
resumed by task ID (OpenHands), which fits delegate follow-ups as the next
session turn.

### 8. The first program and live updates

Every first program takes a plain string: `thread.run("…")` (Codex),
`session.prompt("…")` (pi), `Runner.run(agent, "…", session=)` (OpenAI),
`client.query("…")` (Claude SDK), `runner.Query(ctx, "Hello")` (Eino). Where two
input paths diverge, one gets skipped: MAF's streaming path once ignored the
session (maf#3848).

A silent in-memory default loses data. OpenHands lost events on Cloud Run until
it added a warning (sdk#2915); pi wrote nothing until the first reply (#9792).
The OpenAI SDK refuses to combine a session with provider-side state
(`previous_response_id`, server conversations).

Queued and follow-up turns start with nobody streaming them. ACP v2's prompt
returns on insertion and streams at any time; pi's `watch()` gives a snapshot
"with no gap and no duplicate"; Codex has no live cursor and pays in resume cost
(#38787). ACP `session/resume` with `replayFrom: start` replays all retained
history; OpenHands Canvas pages events, then replays over WebSocket `since`
(`src/hooks/query/use-conversation-history.ts:1-95`).
