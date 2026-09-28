# Turns and their record

**Status:** Local contract implemented; host qualification pending · **Date:** 2026-09-27
**Workflow:** Specified, then built. Public Go names and serialized shapes remain provisional.

## Boundary

A **turn** is one input processed until the agent settles, possibly across
many model calls. It is identified, may span invocations and processes, and
need not belong to a session. (Some SDKs call one model call a "turn"; Dashi
calls that a model attempt.) A session, reserved for later, organizes turns and
supplies conversation history; it does not own a turn's execution rules. An
`Agent` describes reusable behavior. The turn records which agent revision
accepted its work, while the `Runner` supplies clients, executors,
credentials, and storage.

The authoritative turn is an ordered, versioned record of facts. Its current
state, model transcript, UI transcript, usage, and pending work are projections.
Replaying the record validates each transition and never replays an effect.
Unknown record kinds may be retained for inspection, but block execution until
understood. No synthetic user, assistant, or tool message is stored to repair
model history; projection decisions are explicit and leave facts intact.

## Identities and commands

| Identity | Job |
| --- | --- |
| Turn ID | Recover the accepted work, independently of a session or invocation. |
| Step sequence | Order facts and provide the expected state revision for a write. |
| Effect ID and attempt ID | Separate a logical model/tool operation from each execution try. |
| Command ID | `CommandID`, an idempotency key: deduplicate a delivered input, decision, result, or cancellation. |
| Agent revision | Pin accepted behavior; each model request also captures its resolved tools and effective settings. |
| Ownership token | Fence a host's stale writer separately from step revision. |

The runner generates an empty turn ID (on `Start`) and an empty command ID, and
derives an empty agent revision from the agent's behavior fields; see
[zero-config.md](zero-config.md). A generated ID cannot deduplicate a retry, so
durable hosts choose and save their IDs.

All mutating commands carry a stable command ID and payload digest. Repeating the
same ID and content acknowledges the prior application; changing the content
conflicts. A different ID cannot settle an already settled subject again. The
host authenticates and authorizes each command submitter; the runner validates
its turn state and pending-subject correlation.

| Command | Contract |
| --- | --- |
| **Start** | Admit a new turn, its origin, pinned prior history, initial input, limits, and agent revision; only the first acknowledged start permits execution. |
| **Deliver** | Record context for an open, waiting, or continuable turn without advancing it. The next eligible model call sees it according to its scope and replacement rule. |
| **Continue** | Resume a stopped turn, including one interrupted by a context end or crash. It does not reset limits. |
| **Settle** | Record a typed late fact against a pending subject: an external tool result, a human decision, or a reconciled uncertain effect. It never runs the turn. |
| **Cancel** | Request no further work. Unstarted calls become known not executed; started effects remain pending for late reconciliation and accounting, without restarting the loop. |

The admitted agent must be available by revision on recovery. A hash alone
detects drift but cannot reconstruct behavior. A different agent cannot be
silently accepted by `Continue`. The first implementation rejects a changed
agent for that turn (`ErrAgentChanged`); a later explicit revision transition
can define safe in-place changes. Dynamic toolsets resolve before each model
call, and that call records the effective definitions. Durable context
must contain its content or an immutable, resolvable reference. A digest-only
delivery proves what was offered but cannot reconstruct it; such context cannot
support automatic recovery. Context retained for just one model call still
needs a recorded value if that call is recoverable.
Prior session history is copied into the start fact so later invocations cannot
substitute different history. Scheduled starts retain their origin; only an
operator-origin start may introduce a system or developer role. Hosts authorize
that origin before admission.

## Facts and state

The minimum step groups are: turn started; context delivered; model intent and
settlement; tool decision, start, wait, and settlement; validated output
recorded when a schema is requested; turn stop. A model
settlement records the complete assistant result and its client tool calls as
one accepted fact. Requested and effective tool arguments, the authorization
decision, and the agent/policy revision belong to the tool facts. A
provider-hosted activity remains inside the model result; it is never converted
into a locally executable call. Each step has a schema version, turn ID,
monotonic sequence, and one validated payload.

Work state, effect knowledge, persistence acknowledgement, and caller delivery
are separate. A turn may be waiting after its worker has returned; a caller may
disconnect while it runs; a result may be known while its commit is uncertain.
The returned outcome contains the turn view, stop reason, pending subjects,
and a `ReadyToContinue` flag; `Run`'s error reports a step it could not record. The saved turn's replay has the
same flag, so a host can find ready turns after a worker exits. Caller delivery
belongs to the runner, not the turn. Known tool failure, confirmed
nonexecution, waiting, and uncertain execution remain different states. Stops
distinguish completed, waiting, interrupted, limited, refused, canceled,
failed, and uncertain. `Settle` commits a report or approval but does not
restart the loop; only `Continue` does.
Invalid commands and corrupt records fail before a first write; after a first
acknowledged step, the outcome retains partial work and its stop even on failure.

## Recording rule

Before a model send or tool execution, the host acknowledges its intent under
the expected step revision and ownership token. After the effect, it
acknowledges the result before dependent work advances. The recorder may commit
related application rows, such as a budget reservation, accounting, or an
external-tool outbox, in the same transaction. A write rejection stops the
invocation; an ambiguous acknowledgement requires reload by identity before
any further dispatch. Neither becomes model-visible tool failure. An intent
without a result means the effect **may** have happened. An interrupted model
call is sent again unless it may have run an unsafe provider-hosted tool. A
started local tool is rerun only when its definition says read-only or
idempotent. Other unfinished effects require an application report; they are
never blindly repeated. Fencing prevents stale commits, while downstream
idempotency or reconciliation addresses duplicate external effects.

Interrupted model output is kept for display and its usage for accounting,
but is never acted on. A context end uses a bounded commit context to save an
interrupted stop with the context cause. `Cancel` (or a caller's explicit
`ContextCancel` option) ends the turn; unstarted calls become known not
executed. `WithPause` requests interruption before the next effect
without cutting off current work. `Limits.Interruptions` bounds resumes
independently of model attempts; `Limits.Deadline` is a limit, while a host
deadline interrupts the turn.
`BeforeSend` may supply bounded, non-secret metadata derived from the exact
prepared request for the recorder's atomic spending reservation. An explicit
HTTP rejection or proven pre-dispatch failure is known nonexecution; transport
and protocol failures remain uncertain. A host can reconcile an unresolved
attempt with verified nonexecution and an evidence reference. Dashi cannot
verify that external evidence itself.

The initial implementation includes an in-memory recorder and a storage-neutral
acknowledged recorder contract. Session storage, claims, and transcript caching
are adapters above it. Durable record formats must avoid secret-bearing request
bodies; authenticated hosted tools require the snapshot redaction/binding work
already identified in [resources and hosted tools](2026-09-27-resources-and-hosted-tools.md).

**Tradeoff:** per-effect acknowledgements add writes and explicit recovery
states. They let hosts make their own transaction authoritative without making
Dashi a database or queue framework.

Sources: [primary conceptual model](../tmp/research/design/2026-09-26-dive-v2-conceptual-model.md),
[companion design](../tmp/research/design/2026-09-26-dive-v2-conceptual-design.md),
[proposal comparison](../tmp/research/proposal-comparison.md), and
[Nvoken comparison](../tmp/research/comparison/nvoken-cloud.md).
